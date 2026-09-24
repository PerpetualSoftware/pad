package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/items"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// storedStateUnreadableCode mirrors internal/cli.StoredStateUnreadableCode
// (BUG-2675) as a literal rather than importing it. internal/server does not
// import internal/cli — a layering choice two other files state explicitly
// (internal/cli/client_items_copy.go's mirror note, and itemRefOrSlug's
// "avoids pulling internal/cli into the server package for one helper") — and
// one error code is not worth falsifying it. internal/mcp/errors.go carries the
// same string for the same reason.
const storedStateUnreadableCode = "stored_state_unreadable"

// maxBulkItems caps how many items a single bulk request may touch.
// The lane-header bulk actions (TASK-1668) operate on a whole filtered
// lane, which is realistically tens of items; the cap is a guardrail
// against a pathological request, not an expected ceiling.
const maxBulkItems = 1000

// bulkItemsRequest is the body of POST /workspaces/{ws}/items/bulk.
// `ids` accepts issue refs (TASK-5) or UUIDs; `op` selects the verb.
// The remaining fields are op-specific params — see handleBulkItems for
// which op consumes which.
type bulkItemsRequest struct {
	IDs []string `json:"ids"`
	Op  string   `json:"op"`

	// move
	Status     string `json:"status,omitempty"`     // move-to-status (within or across collection)
	Collection string `json:"collection,omitempty"` // move-to-collection (target slug)

	// set-priority
	Priority string `json:"priority,omitempty"`

	// tag / untag
	Tags []string `json:"tags,omitempty"`

	// assign
	AssignedUserID    *string `json:"assigned_user_id,omitempty"`
	AgentRoleID       *string `json:"agent_role_id,omitempty"`
	ClearAssignedUser bool    `json:"clear_assigned_user,omitempty"`
	ClearAgentRole    bool    `json:"clear_agent_role,omitempty"`

	// Force overrides the open-children guard on status-bearing moves,
	// mirroring `pad item update --force` and the move handler's
	// ?force=true. No effect on ops that don't flip a terminal status.
	Force bool `json:"force,omitempty"`
}

// bulkItemOutcome is one successfully-mutated row.
type bulkItemOutcome struct {
	Ref string `json:"ref"`
	ID  string `json:"id"`
	// NotUnique names each carried value a collection move dropped for
	// colliding on a destination unique field (BUG-2367). Additive, omitempty.
	NotUnique []models.NotUniqueDrop `json:"not_unique,omitempty"`
}

// bulkItemFailure is one row that failed, carrying the structured
// server error (code + details) when present — e.g. an open_children
// rejection — so MCP/web callers see the same shape the single PATCH
// surfaces, not just a flattened string.
type bulkItemFailure struct {
	Ref     string          `json:"ref"`
	Error   string          `json:"error"`
	Code    string          `json:"code,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

// bulkItemsResponse is the structured envelope returned to the caller.
type bulkItemsResponse struct {
	Op      string            `json:"op"`
	Updated []bulkItemOutcome `json:"updated"`
	Failed  []bulkItemFailure `json:"failed"`
	Total   int               `json:"total"`
}

// bulkOpError carries a per-row failure with an optional structured
// code/details (open_children, plan_limit_exceeded and others), surfaced on
// the row's failed[] entry.
type bulkOpError struct {
	message string
	code    string
	details json.RawMessage
}

func (e *bulkOpError) Error() string { return e.message }

// handleBulkItems applies one mutation verb to many items in a single
// request, emitting ONE SSE batch event and ONE webhook for the whole
// batch instead of per-item fan-out (TASK-1668). Editor/owner gated:
// the lane-header bulk actions are `canEdit`-only in the UI, so the
// endpoint requires workspace editor role (owner satisfies it too).
//
// Reuses the store mutation paths (UpdateItemWithPreCheck / MoveItem /
// DeleteItem) rather than re-implementing writes; the open-children
// guard runs per status-bearing move exactly as the single PATCH path
// does. Per-row failures are collected, not fatal — the response
// envelope reports updated vs failed so the caller can react.
func (s *Server) handleBulkItems(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	// Owner/editor gated. Bulk lane actions are canEdit-only; viewers
	// and guests (grant-based access) cannot bulk-mutate.
	if !requireRole(r, "editor") {
		writeError(w, http.StatusForbidden, "forbidden", "Bulk mutations require editor or owner role")
		return
	}

	var req bulkItemsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "ids is required")
		return
	}
	if len(req.IDs) > maxBulkItems {
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("too many items: %d (max %d per request)", len(req.IDs), maxBulkItems))
		return
	}

	// Set by the move branch below and threaded to bulkMoveCollection so the
	// target is resolved once per REQUEST rather than once per item.
	var resolvedTarget *models.Collection

	// Validate the verb + its required params up front so a malformed
	// request fails fast before touching any rows.
	switch req.Op {
	case "archive":
	case "restore":
	case "move":
		if req.Status == "" && req.Collection == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "move requires status or collection")
			return
		}
		// Resolve the target ONCE, here, and carry the resolved collection to
		// the per-item path rather than re-resolving per row.
		//
		// Two reasons. req.Collection is compared against item.CollectionSlug,
		// written into activity metadata, and used as an SSE scope; leaving
		// the caller's `spec` in place while the move lands in `specs` would
		// make a same-collection move look like a cross-collection one, log a
		// to_collection nobody can look up, and address the arrival event to a
		// lane no client watches (codex round 1). And re-resolving per item
		// multiplies the lookup by the batch size — up to four queries per row
		// for a target that never resolves (codex round 3).
		if req.Collection != "" {
			targetColl, rerr := s.resolveItemCollectionSlug(workspaceID, req.Collection)
			if rerr != nil {
				writeInternalError(w, rerr)
				return
			}
			// A target that resolves to nothing is deliberately NOT refused
			// here. Failing the whole request up front would answer
			// "no such collection" with a different STATUS than an
			// existing-but-hidden target, which fails per item inside the
			// normal 200 envelope — and that difference is an existence
			// oracle: a restricted caller could probe slugs and learn which
			// collections they cannot see exist (codex round 4). Passing nil
			// down keeps both cases on the identical per-item path, which is
			// also the pre-change behaviour.
			if targetColl != nil {
				req.Collection = targetColl.Slug
				resolvedTarget = targetColl
			}
		}
	case "set-priority":
		if req.Priority == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "set-priority requires priority")
			return
		}
	case "tag", "untag":
		if len(req.Tags) == 0 {
			writeError(w, http.StatusBadRequest, "bad_request", req.Op+" requires tags")
			return
		}
	case "assign":
		if req.AssignedUserID == nil && req.AgentRoleID == nil && !req.ClearAssignedUser && !req.ClearAgentRole {
			writeError(w, http.StatusBadRequest, "bad_request", "assign requires assigned_user_id, agent_role_id, or a clear flag")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("unknown op %q", req.Op))
		return
	}

	actor, source := actorFromRequest(r)
	actorName := actorNameFromRequest(r)
	user := currentUser(r)
	role := workspaceRole(r)

	// Pre-compute collection visibility once. A member with
	// collection_access="specific" (even an editor) must not be able to
	// bulk-mutate items in collections they can't see — the single-item
	// handlers enforce this per row via requireItemVisible, so the bulk
	// path must too. nil = all-access (admin / fresh install).
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	resp := bulkItemsResponse{
		Op:      req.Op,
		Updated: []bulkItemOutcome{},
		Failed:  []bulkItemFailure{},
	}
	affectedIDs := make([]string, 0, len(req.IDs))
	// Group affected items by their resulting collection slug so each
	// batch SSE event carries a Collection and routes through the SSE
	// visibility filter correctly (restricted members only get events
	// for collections they can see; guests with grants still get the
	// reconcile trigger). For the dominant lane-header case (one lane =
	// one collection) this is exactly one event.
	type collBatch struct {
		count  int
		maxSeq int64
	}
	batches := map[string]*collBatch{}

	// One id for the whole bulk OPERATION, stamped onto every canonical event
	// its members emit (SPEC-3 v1.5, migration 082). Bulk is a handler loop
	// over per-item store mutations with no enclosing transaction, so each
	// member writes its own outbox row; without this correlation the drain
	// would put N single-item events on the webhook wire for one lane action —
	// the flood TASK-1668's batch event exists to prevent.
	//
	// Minted before the loop and unconditionally, including for the runs where
	// every item fails: an unused batch id costs nothing, while deciding
	// mid-loop whether this "counts as" a batch would make the correlation
	// depend on how far the loop got.
	batchID := uuid.NewString()

	for _, ref := range req.IDs {
		// `restore` operates on ARCHIVED items, which ResolveItem hides —
		// resolve include-deleted for that op (used by undo, TASK-1674).
		var item *models.Item
		var err error
		if req.Op == "restore" {
			item, err = s.store.ResolveItemIncludeDeleted(workspaceID, ref)
		} else {
			item, err = s.store.ResolveItem(workspaceID, ref)
		}
		if err != nil {
			resp.Failed = append(resp.Failed, bulkItemFailure{Ref: ref, Error: err.Error()})
			continue
		}
		if item == nil {
			resp.Failed = append(resp.Failed, bulkItemFailure{Ref: ref, Error: "item not found"})
			continue
		}

		// Per-item visibility gate. Report invisible items as
		// not-found so a restricted member can't probe existence by ref.
		visible, verr := s.checkItemVisible(workspaceID, item, user, role, isBearerAuth(r))
		if verr != nil {
			resp.Failed = append(resp.Failed, bulkItemFailure{Ref: ref, Error: verr.Error()})
			continue
		}
		if !visible {
			resp.Failed = append(resp.Failed, bulkItemFailure{Ref: ref, Error: "item not found"})
			continue
		}

		var droppedFields []string
		updated, opErr := s.applyBulkOp(r, workspaceID, item, &req, actor, source, visibleIDs, resolvedTarget, batchID, &droppedFields)
		if opErr != nil {
			resp.Failed = append(resp.Failed, bulkItemFailure{
				Ref:     itemRefOrSlug(*item),
				Error:   opErr.message,
				Code:    opErr.code,
				Details: opErr.details,
			})
			continue
		}

		// TASK-2533: per-item, NOT batched like publishBulkItemsEvent
		// below — see publishWatchNotifications' doc comment for why
		// that's an accepted noise-discipline tradeoff for Phase 1.
		// No-op when updated.LastMutation is nil (most bulk ops:
		// archive/restore/tag/untag never touch status or assignment).
		s.publishWatchNotifications(workspaceID, updated, actor, actorName)

		// Per-row activity log keeps the audit trail intact — it's a
		// DB write, not the SSE/webhook fan-out the task is avoiding.
		// A cross-collection move logs action="moved" with from/to
		// collection slugs — same shape as the single-item move path
		// (handleMoveItem). This is also what /items-changes reads to
		// emit moved-out tombstones (BUG-1675), so a generic "updated"
		// here would silently break cross-visibility move eviction.
		action := "updated"
		meta := map[string]string{"bulk_op": req.Op}
		switch {
		case req.Op == "archive":
			action = "archived"
		case req.Op == "restore":
			action = "restored"
		case req.Op == "move" && req.Collection != "" && req.Collection != item.CollectionSlug:
			action = "moved"
			meta["from_collection"] = item.CollectionSlug
			meta["to_collection"] = req.Collection
		}
		// Same key, same joined-string shape and the same reason as
		// handleMoveItem's: this map is map[string]string, and a raw array
		// renders as a Go map literal in the timeline (BUG-2628). Outside the
		// move branch because a bulk STATUS or PRIORITY change can discard a
		// relation default too, and a drop nobody records is the defect
		// BUG-2674 closed.
		if resolvedTarget != nil && updated != nil && updated.Warnings != nil && len(updated.Warnings.NotUnique) > 0 {
			meta["not_unique"] = notUniqueSummary(updated.Warnings.NotUnique, resolvedTarget.Name)
		}
		if len(droppedFields) > 0 {
			meta["dropped_fields"] = strings.Join(droppedFields, ", ")
		}
		s.logActivityWithMeta(workspaceID, item.ID, action, r, auditMeta(meta))

		outcome := bulkItemOutcome{Ref: itemRefOrSlug(*item), ID: item.ID}
		if updated != nil && updated.Warnings != nil {
			outcome.NotUnique = updated.Warnings.NotUnique
		}
		resp.Updated = append(resp.Updated, outcome)
		affectedIDs = append(affectedIDs, item.ID)

		// Determine which collection scopes need a reconcile event.
		// Every op notifies the collection the item lives in. A
		// cross-collection move ALSO notifies the target, so a member
		// watching the source lane (the item leaving) AND one watching
		// the target lane (the item arriving) both reconcile. (Move
		// rejects same-collection, so source != target here.)
		scopes := []string{item.CollectionSlug}
		if req.Op == "move" && req.Collection != "" && req.Collection != item.CollectionSlug {
			scopes = append(scopes, req.Collection)
		}
		var seq int64
		if updated != nil {
			seq = updated.Seq
		}
		for _, sc := range scopes {
			b := batches[sc]
			if b == nil {
				b = &collBatch{}
				batches[sc] = b
			}
			b.count++
			if seq > b.maxSeq {
				b.maxSeq = seq
			}
		}
	}

	resp.Total = len(resp.Updated) + len(resp.Failed)

	// One SSE batch event per affected collection + ONE batch header for the
	// drain. The core of the original task: a whole-lane bulk action must not
	// emit N per-item events. Per-collection (not fully per-item) keeps SSE
	// visibility routing correct while still collapsing a lane action to a
	// single event.
	//
	// "when something actually changed" USED TO SAY MORE THAN IT MEANT, and is
	// corrected here rather than left to be re-derived (codex rounds 1-2): the
	// condition is that at least one row was TOUCHED without erroring, which
	// is not the same as a semantic change. Untagging a tag nobody has
	// succeeds on every row and changes nothing, so this fires with a count
	// and the store — which emits only for real changes — writes no member
	// events at all.
	if len(affectedIDs) > 0 {
		for collSlug, b := range batches {
			s.publishBulkItemsEvent(workspaceID, req.Op, collSlug, b.count, actor, actorName, source, b.maxSeq)
		}
		// The batch HEADER for the outbox drain (SPEC-3 v1.6): the operation,
		// the shared delta and the member refs — the three things the drain
		// cannot work out from the member rows, which carry post-mutation
		// snapshots rather than deltas.
		//
		// affectedIDs counts rows the handler TOUCHED, not rows that
		// semantically changed, so an operation whose members were all no-ops
		// (untagging a tag nobody had) produces a header with a count and no
		// members. That asymmetry is inherited, not introduced: the webhook
		// this replaces fired on exactly the same condition with exactly the
		// same count, and the store's "a mutation that changed nothing emits
		// nothing" rule is the other half of it. Recorded rather than fixed
		// here — narrowing the count to semantically-changed rows is a wire
		// change to `count`/`item_ids` and belongs with a contract version,
		// not inside a delivery refactor (codex round 1).
		//
		// Written AFTER the loop, in its own transaction, because a handler
		// bulk has no enclosing one. A failure here is logged and swallowed on
		// purpose: the members' own events are already committed and will
		// deliver individually, so the operation degrades to a flood rather
		// than to a loss, and failing the request after every row has
		// committed would be the worse answer.
		if err := s.store.EmitBulkHeaderEvent(workspaceID, batchID, req.Op, affectedIDs, bulkEventDelta(&req)); err != nil {
			slog.Error("failed to emit bulk batch header", "workspace_id", workspaceID, "batch_id", batchID, "error", err)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// applyBulkOp dispatches one verb against one item, reusing the same
// store paths as the single-item handlers. Returns the post-mutation
// item (for seq) on success, or a structured per-row error.
// resolvedTarget carries the request-level collection resolution for `move`
// (nil for every other op), so the per-item path never re-resolves.
// bulkStoreError wraps a store error as a per-item bulk failure, classifying
// the NUL refusal so it carries a real code instead of arriving as an
// unlabelled message (codex round 1, finding 4).
//
// A bulk response stays 200 with the item listed under Failed — that is the
// bulk contract, and a deterministic refusal of ONE item should not fail the
// other forty-nine. What it should not do is look like an internal fault.
func bulkStoreError(err error) *bulkOpError {
	if reason, ok := nulRefusalReason(err); ok {
		return &bulkOpError{message: reason, code: "bad_request"}
	}
	return &bulkOpError{message: err.Error()}
}

// droppedFields is an OUT-PARAMETER, and it is one deliberately. Only the
// collection-move branch produces dropped field keys, the caller needs them to
// build ONE activity row per item (a second row would misreport a single
// move as two events), and threading a third return value through fourteen
// unrelated returns would put `nil` in every branch that has nothing to say.
// Reset by the caller each iteration; nil is accepted and means "not
// interested".
func (s *Server) applyBulkOp(r *http.Request, workspaceID string, item *models.Item, req *bulkItemsRequest, actor, source string, visibleIDs []string, resolvedTarget *models.Collection, batchID string, droppedFields *[]string) (*models.Item, *bulkOpError) {
	switch req.Op {
	case "archive":
		if err := s.store.DeleteItem(item.ID, store.WithEventBatch(batchID)); err != nil {
			return nil, bulkStoreError(err)
		}
		// DeleteItem bumps seq; re-read so the batch event carries the
		// post-archive cursor. Falls back to the pre-delete row on a
		// lookup miss (downstream backfills on a stale/zero seq).
		if d, derr := s.store.GetItemIncludeDeleted(item.ID); derr == nil && d != nil {
			return d, nil
		}
		return item, nil

	case "restore":
		// Undo of a bulk archive (TASK-1674). RestoreItem clears
		// deleted_at and bumps seq; returns the live row.
		// BUG-3101: each restore is decided on its own, in request order, so
		// a batch with room for some restores those and lists the rest under
		// failed with the plan-limit code and details.
		opts := append([]store.MutationOption{store.WithEventBatch(batchID)}, s.restoreLimitOpts()...)
		restored, err := s.store.RestoreItem(item.ID, opts...)
		if err != nil {
			var ple *store.PlanLimitError
			if errors.As(err, &ple) {
				details, _ := json.Marshal(planLimitDetails(&ple.Result))
				return nil, &bulkOpError{message: planLimitMessage(&ple.Result), code: "plan_limit_exceeded", details: details}
			}
			if err == sql.ErrNoRows {
				return nil, &bulkOpError{message: "item not found or not archived"}
			}
			if strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "duplicate key") {
				return nil, &bulkOpError{
					message: "cannot restore: another item has claimed this slug or invocation slug",
					code:    "conflict",
				}
			}
			return nil, bulkStoreError(err)
		}
		return restored, nil

	case "move":
		if req.Collection != "" {
			return s.bulkMoveCollection(r, workspaceID, item, req, visibleIDs, resolvedTarget, batchID, droppedFields)
		}
		// Status-only move = a field update on the same collection.
		return s.bulkFieldUpdate(r, workspaceID, item, map[string]any{"status": req.Status}, req.Force, visibleIDs, actor, source, batchID, droppedFields)

	case "set-priority":
		return s.bulkFieldUpdate(r, workspaceID, item, map[string]any{"priority": req.Priority}, req.Force, visibleIDs, actor, source, batchID, droppedFields)

	case "tag":
		return s.bulkTagUpdate(item, req.Tags, true, actor, source, batchID)

	case "untag":
		return s.bulkTagUpdate(item, req.Tags, false, actor, source, batchID)

	case "assign":
		input := models.ItemUpdate{
			AssignedUserID:    req.AssignedUserID,
			AgentRoleID:       req.AgentRoleID,
			ClearAssignedUser: req.ClearAssignedUser,
			ClearAgentRole:    req.ClearAgentRole,
			LastModifiedBy:    actor,
			Source:            source,
		}
		updated, err := s.store.UpdateItem(item.ID, input, store.WithEventBatch(batchID))
		if err != nil {
			return nil, bulkStoreError(err)
		}
		return updated, nil
	}
	// Unreachable: op was validated in handleBulkItems.
	return nil, &bulkOpError{message: fmt.Sprintf("unsupported op %q", req.Op)}
}

// bulkFieldUpdate merges field changes into the item's existing fields,
// validates against the collection schema, runs the open-children guard
// (unless force), and writes via UpdateItemWithPreCheck — the same path
// the single PATCH handler uses. Used by status moves and set-priority.
func (s *Server) bulkFieldUpdate(r *http.Request, workspaceID string, item *models.Item, changes map[string]any, force bool, visibleIDs []string, actor, source, batchID string, droppedFields *[]string) (*models.Item, *bulkOpError) {
	coll, err := s.store.GetCollection(item.CollectionID)
	if err != nil || coll == nil {
		return nil, &bulkOpError{message: "failed to load collection"}
	}
	var schema models.CollectionSchema
	if err := models.UnmarshalItemFieldSchema([]byte(coll.Schema), &schema); err != nil {
		return nil, &bulkOpError{message: "failed to parse collection schema"}
	}

	// REFUSE a key the item's collection does not declare (BUG-3154). Every
	// caller passes a key the SERVER chose (`status` for a status-only move,
	// `priority` for set-priority) and validation below walks only declared
	// fields, so on a collection without that field the value was written as
	// an orphan no schema-driven surface renders, and the item was reported
	// under `updated` although the operation has no meaning there. Refused per
	// ITEM, so the rest of the batch still applies.
	//
	// Deliberately NOT the accept-and-warn of a single-item update
	// (BUG-2850): there the caller TYPES the key and round-trips whole blobs;
	// here the caller named an operation and the key is ours.
	if undeclared := items.UndeclaredOverrideKeys(changes, schema.Fields); len(undeclared) > 0 {
		return nil, &bulkOpError{
			code:    "validation_error",
			message: fmt.Sprintf("collection %q has no %q field, so this operation does not apply to this item", coll.Slug, undeclared[0]),
		}
	}

	fieldMap := make(map[string]any)
	if item.Fields != "" && item.Fields != "{}" {
		// REFUSE an unreadable stored blob rather than discard it (BUG-3049,
		// codex round 3). This unmarshal error used to be ignored: the map
		// stayed empty, the caller's changes were written over the top, and the
		// unreadable bytes were GONE — a bulk status move silently destroyed
		// whatever the row held. That is the repair path codex correctly
		// identified as lost, and it is not one worth keeping: the room's
		// standing answer for unreadable stored state is to refuse and say so
		// (BUG-2627 part 3, BUG-2675's `stored_state_unreadable`), because the
		// raw bytes are still there for a human to repair and a write that
		// throws them away cannot be undone.
		//
		// Refused per ITEM, not per batch, so one broken row fails on its own
		// and the other rows in the request still apply.
		if err := json.Unmarshal([]byte(item.Fields), &fieldMap); err != nil {
			return nil, &bulkOpError{
				code: storedStateUnreadableCode,
				message: "this item's stored fields are not valid JSON, so a field update cannot be merged onto them. " +
					"Repair the item's fields first (pad item show, then a full `fields` write); retrying this request will fail identically.",
			}
		}
	}
	// The item's own stored values, before the caller's changes merge in. Held
	// separately because "carried" and "supplied" get different treatment
	// everywhere in this unit, and after the merge the map cannot tell them
	// apart.
	storedFields := make(map[string]any, len(fieldMap))
	for k, v := range fieldMap {
		storedFields[k] = v
	}
	for k, v := range changes {
		fieldMap[k] = v
	}

	// Coerce strings to their declared types before validating (BUG-2850).
	fieldMap = items.CoerceFields(fieldMap, schema)
	// BUG-3028: a blank relation in `changes` is one this write SETS, removed
	// before validation so a required one is refused; a blank carried from the
	// stored row is removed after validation, landing as key-absent.
	items.DropBlankRelations(fieldMap, schema, func(k string) bool {
		_, set := changes[k]
		return !set
	})
	// Snapshot before validation, which INJECTS schema defaults: this door's
	// relation pass looks only at the keys `changes` names, so a relation
	// default validation fills in was persisted raw — never canonicalised and
	// never checked against its target collection (codex round 3). The same
	// late-arrival the migrate doors hit, reached by a different route.
	relBefore := store.RelationKeysPresent(schema, fieldMap)
	// BUG-3079: a default failing its own type check is DISCARDED and reported
	// through the same out-parameter the relation drops below already use.
	defaultDrops, err := items.ValidateFieldsWithDrops(fieldMap, schema)
	if err != nil {
		return nil, &bulkOpError{message: err.Error(), code: "validation_error"}
	}
	items.DropBlankRelations(fieldMap, schema, nil)
	if droppedFields != nil && len(defaultDrops) > 0 {
		*droppedFields = append(*droppedFields, defaultDrops...)
	}
	// Referent validation for relation values (TASK-2878). Refuses like any
	// other write — per item, so one bad referent fails its own item and not
	// the batch — but ONLY over the keys this operation CHANGES.
	//
	// `fieldMap` is the item's STORED blob merged with `changes` a few lines
	// above, so resolving all of it would re-litigate values the caller never
	// touched and refuse them: an item carrying a legacy relation value would
	// become permanently un-bulk-updatable, its status and priority frozen by
	// a field the operation does not mention. Same rule as the fields_patch
	// door, and for the same reason — `internal/items` accepted any string in
	// a relation field for as long as the type has existed, so such items are
	// not hypothetical.
	//
	// Read out of the COERCED map rather than out of `changes`, so the value
	// resolved is the one that would be stored, and written back so a supplied
	// ref is canonicalised to its id exactly as at every other write door.
	suppliedRelations := make(map[string]any, len(changes))
	for k := range changes {
		// GATED ON THE PROVENANCE SNAPSHOT taken above, not on presence in the
		// post-validation map (codex round 3). `ValidateFields` injects schema
		// defaults, and an empty `multi_relation` in `changes` is normalised to
		// an absent key BEFORE that injection — so `fieldMap[k]` could hold a
		// DEFAULT while `changes` still named the key, and this loop then
		// resolved that default as though the caller had typed it. An
		// unresolvable default must be DROPPED and reported, never refused:
		// otherwise one bad default in a schema fails every bulk update into
		// that collection, on a defect its author has to fix elsewhere.
		if !relBefore[k] {
			continue
		}
		if v, present := fieldMap[k]; present {
			suppliedRelations[k] = v
		}
	}
	relIssues, relErr := s.resolveRelationReferents(r, workspaceID, schema, suppliedRelations)
	if relErr != nil {
		return nil, &bulkOpError{message: "Failed to resolve relation references", code: "internal_error"}
	}
	if len(relIssues) > 0 {
		return nil, &bulkOpError{message: relationIssuesMessage(relIssues), code: "validation_error"}
	}
	for k, v := range suppliedRelations {
		fieldMap[k] = v
	}
	lateDropped, lateErr := s.store.ResolveLateRelationDefaults(s.relationVisibility(r, workspaceRole(r)), workspaceID, schema, fieldMap, relBefore)
	if lateErr != nil {
		return nil, &bulkOpError{message: "Failed to resolve relation references", code: "internal_error"}
	}
	if cerr := s.collapseInvisibleRelationIssues(r, workspaceID, workspaceRole(r), lateDropped); cerr != nil {
		return nil, &bulkOpError{message: "Failed to resolve relation references", code: "internal_error"}
	}
	if req := store.RequiredRelationIssues(schema, lateDropped); len(req) > 0 {
		return nil, &bulkOpError{
			message: "required fields missing: " + relationIssuesMessage(req),
			code:    "missing_required_fields",
		}
	}
	invisibleDefaults, invErr := s.dropInvisibleRelationDefaults(r, workspaceID, workspaceRole(r), schema, fieldMap,
		notDefaultKeys(changes, storedFields))
	if invErr != nil {
		return nil, &bulkOpError{message: "Failed to resolve relation references", code: "internal_error"}
	}
	if req := store.RequiredRelationIssues(schema, invisibleDefaults); len(req) > 0 {
		return nil, &bulkOpError{
			message: "required fields missing: " + relationIssuesMessage(req),
			code:    "missing_required_fields",
		}
	}
	if all := append(lateDropped, invisibleDefaults...); droppedFields != nil && len(all) > 0 {
		for _, ri := range all {
			*droppedFields = append(*droppedFields, ri.Key)
		}
	}
	if err := s.checkUniqueFields(workspaceID, item.CollectionID, item.ID, schema, fieldMap); err != nil {
		return nil, &bulkOpError{message: err.Error(), code: "conflict"}
	}
	// Which keys autoPopulateDates ADDED, as opposed to keys the caller set or
	// the item already carried. Needed below: an auto-date is an "only if still
	// empty" intent, and that decision was made against a snapshot read outside
	// the write transaction (codex round 4).
	beforeAuto := make(map[string]any, len(fieldMap))
	for k, v := range fieldMap {
		beforeAuto[k] = v
	}
	autoPopulateDates(fieldMap, item.Fields, schema)
	autoDateKeys := make([]string, 0, 2)
	for k, v := range fieldMap {
		if old, had := beforeAuto[k]; !had || !reflect.DeepEqual(old, v) {
			if _, isChange := changes[k]; !isChange {
				autoDateKeys = append(autoDateKeys, k)
			}
		}
	}

	// BUG-3049: send a field-level PATCH, not the whole blob. `fieldMap` is the
	// item's stored fields (read at the top of this function, OUTSIDE the write
	// transaction) merged with this operation's changes, so writing it as
	// `Fields` reverted any concurrent write that landed in between — a bulk
	// status move on ten items could undo ten unrelated single-field edits.
	//
	// The patch is the DIFF of this pipeline's own output against the stored
	// values, so it still carries everything this operation legitimately
	// changes beyond `changes` itself: schema defaults ValidateFields injected,
	// canonicalised relation refs, autoPopulateDates' completion stamps, and
	// keys the relation passes DROPPED (carried as an explicit nil, which
	// mergeFieldsPatch removes). What it no longer carries is keys this
	// operation did not touch at all.
	//
	// The auto-date stamps are the one entry in that list whose inclusion is
	// re-decided UNDER THE LOCK, in the precheck below: they express "only if
	// still empty", and emptiness was read outside the transaction.
	//
	// Residual, stated rather than hidden: a carried value whose only change is
	// CoerceFields normalising its type (stored `"3"` on a number field
	// becoming `3`) differs from the stored value and is therefore still in the
	// patch. That reproduces exactly what the blob write did to such a key, so
	// it is not a regression — it is the part of the window this fix does not
	// close, and it closes for good when the stored value is already canonical.
	patch := fieldsPatchFromMerge(storedFields, fieldMap)

	var precheck func(tx *sql.Tx, existing *models.Item) error
	{
		var settings models.CollectionSettings
		if coll.Settings != "" {
			_ = json.Unmarshal([]byte(coll.Settings), &settings)
		}
		var gctx openChildrenGuardContext
		if !force {
			guestFull, guestGranted, gerr := s.guestResourceFilter(r, workspaceID)
			if gerr != nil {
				return nil, &bulkOpError{message: gerr.Error()}
			}
			gctx = openChildrenGuardContext{
				r:                    r,
				workspaceID:          workspaceID,
				itemID:               item.ID,
				parentSchema:         schema,
				parentSettings:       settings,
				newFieldMap:          fieldMap,
				visibleCollectionIDs: visibleIDs,
				guestFullCollIDs:     guestFull,
				guestGrantedItemIDs:  guestGranted,
			}
		}
		// The precheck is installed UNCONDITIONALLY now, because it carries two
		// jobs and only one of them is the guard. It runs inside the write
		// transaction, before the store merges the patch (see
		// store.UpdateItem's ordering), which is the only place an "only if
		// still empty" decision can be made truthfully.
		precheck = func(tx *sql.Tx, existing *models.Item) error {
			// JOB 1 (BUG-3049, codex round 4): re-decide each AUTO-POPULATED
			// date against the LOCKED row. autoPopulateDates fills start_date /
			// end_date only when the value is empty, and it read a snapshot from
			// outside this transaction — so a concurrent writer who set the date
			// in between would have had it replaced by today's. Nobody in this
			// request typed that value, so the concurrent one wins: the key is
			// dropped from the patch. A date the CALLER supplied is not in
			// autoDateKeys and is untouched by this.
			if len(autoDateKeys) > 0 {
				locked := map[string]any{}
				if existing.Fields != "" && existing.Fields != "{}" {
					if err := json.Unmarshal([]byte(existing.Fields), &locked); err != nil {
						// Unreadable under the lock: the merge itself will
						// refuse, so leave the patch alone rather than guess.
						return nil
					}
				}
				for _, k := range autoDateKeys {
					if cur, ok := locked[k].(string); ok && cur != "" {
						delete(patch, k)
					}
				}
			}
			if force {
				return nil
			}
			// JOB 2: the open-children guard, as before, against the same
			// in-tx snapshot the UPDATE will write.
			txCtx := gctx
			txCtx.currentFieldsJS = existing.Fields
			details, derr := s.runOpenChildrenGuard(tx, txCtx)
			if derr != nil {
				return derr
			}
			if details != nil {
				return &openChildrenGuardError{details: details}
			}
			return nil
		}
	}

	input := models.ItemUpdate{
		FieldsPatch:    patch,
		LastModifiedBy: actor,
		Source:         source,
	}

	updated, err := s.store.UpdateItemWithPreCheck(item.ID, input, precheck, store.WithEventBatch(batchID))
	if err != nil {
		if details, ok := asOpenChildrenGuardError(err); ok {
			raw, _ := json.Marshal(details)
			return nil, &bulkOpError{
				message: "cannot mark item terminal while it has open children",
				code:    "open_children",
				details: raw,
			}
		}
		return nil, bulkStoreError(err)
	}
	if updated == nil {
		return nil, &bulkOpError{message: "item not found"}
	}
	return updated, nil
}

// bulkTagUpdate adds (add=true) or removes (add=false) the given tags
// from the item's tag set, preserving existing order and de-duplicating.
func (s *Server) bulkTagUpdate(item *models.Item, tags []string, add bool, actor, source, batchID string) (*models.Item, *bulkOpError) {
	existing := []string{}
	if item.Tags != "" && item.Tags != "[]" {
		_ = json.Unmarshal([]byte(item.Tags), &existing)
	}

	remove := make(map[string]bool)
	if !add {
		for _, t := range tags {
			remove[t] = true
		}
	}
	seen := make(map[string]bool)
	result := make([]string, 0, len(existing)+len(tags))
	for _, t := range existing {
		if remove[t] || seen[t] {
			continue
		}
		seen[t] = true
		result = append(result, t)
	}
	if add {
		for _, t := range tags {
			t = strings.TrimSpace(t)
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			result = append(result, t)
		}
	}

	tagsJSON, err := json.Marshal(result)
	if err != nil {
		return nil, &bulkOpError{message: "failed to marshal tags"}
	}
	tagsStr := string(tagsJSON)
	updated, err := s.store.UpdateItem(item.ID, models.ItemUpdate{
		Tags:           &tagsStr,
		LastModifiedBy: actor,
		Source:         source,
	}, store.WithEventBatch(batchID))
	if err != nil {
		return nil, bulkStoreError(err)
	}
	return updated, nil
}

// bulkMoveCollection moves one item into req.Collection, migrating its
// fields between schemas — the same core as handleMoveItem, applied
// per row. A status override (req.Status) lands as a field override on
// the migrated set.
// targetColl is the collection the request-level resolution already produced,
// and taking it as a parameter is what keeps the resolution one-per-request
// instead of one-per-item. It is NIL when the caller named a collection that
// resolves to nothing — deliberately, so that case and an existing-but-hidden
// target fail identically here rather than at different HTTP statuses (the
// existence oracle from codex round 4). Hence the nil check below.
func (s *Server) bulkMoveCollection(r *http.Request, workspaceID string, item *models.Item, req *bulkItemsRequest, visibleIDs []string, targetColl *models.Collection, batchID string, droppedFields *[]string) (*models.Item, *bulkOpError) {
	if targetColl == nil {
		return nil, &bulkOpError{message: "target collection not found", code: "invalid_collection"}
	}
	// Target-collection visibility gate — same as handleMoveItem. A
	// restricted member must not be able to move items into a
	// collection they can't see.
	if !isCollectionVisible(targetColl.ID, visibleIDs) {
		return nil, &bulkOpError{message: "target collection not found", code: "invalid_collection"}
	}
	if targetColl.ID == item.CollectionID {
		return nil, &bulkOpError{message: "item is already in this collection", code: "same_collection"}
	}
	sourceColl, err := s.store.GetCollection(item.CollectionID)
	if err != nil || sourceColl == nil {
		return nil, &bulkOpError{message: "failed to load source collection"}
	}

	var sourceSchema, targetSchema models.CollectionSchema
	if err := json.Unmarshal([]byte(sourceColl.Schema), &sourceSchema); err != nil {
		return nil, &bulkOpError{message: "failed to parse source schema"}
	}
	if err := json.Unmarshal([]byte(targetColl.Schema), &targetSchema); err != nil {
		return nil, &bulkOpError{message: "failed to parse target schema"}
	}

	currentFields := make(map[string]any)
	if err := json.Unmarshal([]byte(item.Fields), &currentFields); err != nil {
		currentFields = make(map[string]any)
	}

	// SameWorkspace is a property of the endpoint, not a guess: a move
	// changes the item's COLLECTION and cannot change its workspace — the
	// cross-workspace path is the copy endpoint. So the repo/member
	// context around the item is unchanged and referential system
	// metadata still describes something true (BUG-2674).
	result := items.MigrateFields(currentFields, sourceSchema.Fields, targetSchema.Fields, items.SameWorkspace)
	// `status` here is CALLER INPUT, and the relation classifier below has to
	// be told so. The path merges exactly one field and used to pass
	// `supplied=nil` on the grounds that it carries no per-field overrides —
	// true of every field except this one. A destination schema is free to
	// declare `status` as a relation, and then a value the caller typed was
	// classified as carried: silently dropped instead of refused, and never
	// checked for visibility (codex round 11).
	suppliedByCaller := map[string]any{}
	if req.Status != "" {
		// BUG-3154: the same refusal bulkFieldUpdate makes for a status-only
		// move. MigrateFields keeps only target-declared fields, but this
		// override is merged after it and validation walks only declared
		// fields, so a target with no `status` field stored an orphan here.
		// Checked against the stripped schema, the one the override is
		// validated against below and the one BUG-2379's move check uses.
		if undeclared := items.UndeclaredOverrideKeys(map[string]any{"status": req.Status}, items.SchemaForMigratedFields(targetSchema).Fields); len(undeclared) > 0 {
			return nil, &bulkOpError{
				code:    "validation_error",
				message: fmt.Sprintf("collection %q has no %q field, so this operation does not apply to this item", targetColl.Slug, undeclared[0]),
			}
		}
		result.Fields["status"] = req.Status
		suppliedByCaller["status"] = req.Status
	}
	// Filtered against what the CALLER supplied, because result.Errors is
	// computed by MigrateFields BEFORE any override exists (migrate.go:62).
	//
	// This is the defect PLAN-2357 DR-12 fixed at the SINGLE move door and
	// nobody swept to this one: an override that SATISFIED a required
	// destination field still 400'd. Reachable here since round 11 made
	// `status` caller input — move an item whose source `status` is a select
	// value into a destination whose `status` is a required relation, supply
	// a perfectly good referent, and the move was refused for a field the
	// request had just filled (codex round 19).
	//
	// Filtering rather than adopting the single door's "validate the merged
	// map" shape, deliberately: ValidateFields below already covers the
	// merged map, and switching this check to it would change the error CODE
	// for a genuinely-missing required field from missing_required_fields to
	// validation_error — a compatibility break for callers reading the code,
	// to fix a defect that does not need it.
	if remaining := requiredErrorsUnsatisfiedBy(result.Errors, suppliedByCaller); len(remaining) > 0 {
		return nil, &bulkOpError{
			message: "required fields missing: " + strings.Join(remaining, ", "),
			code:    "missing_required_fields",
		}
	}
	// Validate the final field map (including any status override)
	// against the TARGET schema — MigrateFields validates migrated
	// values but an override can smuggle in a value the target schema
	// doesn't allow (e.g. a status not in the target's options).
	// Coerce strings to their declared types before validating (BUG-2850).
	result.Fields = items.CoerceFields(result.Fields, items.SchemaForMigratedFields(targetSchema))
	// BUG-3028: an override is a value this move SETS; a blank one is removed
	// before validation so a required target field is refused. Blanks carried
	// from the source are removed after validation.
	items.DropBlankRelations(result.Fields, items.SchemaForMigratedFields(targetSchema), func(k string) bool {
		_, set := suppliedByCaller[k]
		return !set
	})
	// Relation referents on a bulk move (TASK-2878). Same door class as the
	// single-item move: within the workspace, so a valid relation survives and
	// only an unresolvable one is dropped. `req` carries no per-field
	// overrides on this path — only `status`, merged above — so every relation
	// value here is CARRIED, and nothing on this door can refuse. Passing nil
	// for `supplied` says that rather than leaving it implied.
	// The supplied half owes the visibility check, exactly as at the single
	// move door. Round 11 made `status` supplied so the store classifier
	// would REFUSE an unresolvable value rather than drop it; it did not
	// carry across the other half of the supplied contract, which the store
	// resolver structurally cannot provide — see
	// refuseInvisibleRelationOverrides. This was the only one of the three
	// MigrateRelationReferents call sites without it, so a caller who cannot
	// see the target collection could name a live item in it and have the
	// value stored (codex round 16).
	if invisible, err := s.refuseInvisibleRelationOverrides(
		r, workspaceID, workspaceRole(r), items.SchemaForMigratedFields(targetSchema),
		suppliedByCaller); err != nil {
		return nil, &bulkOpError{message: "Failed to resolve relation references", code: "internal_error"}
	} else if len(invisible) > 0 {
		return nil, &bulkOpError{message: relationIssuesMessage(invisible), code: "validation_error"}
	}
	relRefusals, relDropped, relErr := s.store.MigrateRelationReferents(
		s.relationVisibility(r, workspaceRole(r)),
		workspaceID, items.SchemaForMigratedFields(targetSchema), result.Fields,
		suppliedByCaller, store.CarriedSourceValues(currentFields, result.Dropped),
		store.RelationCarryWithinWorkspace)
	if relErr != nil {
		return nil, &bulkOpError{message: "Failed to resolve relation references", code: "internal_error"}
	}
	if len(relRefusals) > 0 {
		// Reachable since round 11: `status` is caller input, so a destination
		// schema declaring it as a relation makes this the ordinary write
		// refusal. It was written as unreachable-but-kept, which is why the
		// misclassification survived — a branch nobody can reach is a branch
		// nobody checks.
		return nil, &bulkOpError{message: relationIssuesMessage(relRefusals), code: "validation_error"}
	}
	for _, ri := range relDropped {
		result.Dropped = append(result.Dropped, ri.Key)
	}
	// Carried unique collisions: dropped and reported, as handleMoveItem does
	// (BUG-2367).
	notUnique, nuErr := s.store.DropCarriedUniqueCollisionsQ(s.store.Q(),
		s.relationVisibility(r, workspaceRole(r)), workspaceID, targetColl,
		targetSchema.Fields, result.Fields, func(k string) bool {
			_, set := suppliedByCaller[k]
			return set
		})
	if nuErr != nil {
		return nil, &bulkOpError{message: "failed to check unique fields", code: "internal_error"}
	}
	for _, d := range notUnique {
		result.Dropped = append(result.Dropped, d.Key)
	}
	// Snapshot AFTER the pass above and BEFORE validation: what this needs to
	// identify is exactly what VALIDATION adds — which includes a default the
	// pass just deleted as unresolvable and validation puts straight back.
	// Snapshotting before the pass would treat that key as already examined
	// and skip it, which is the arrangement that hid it.
	relBefore := store.RelationKeysPresent(items.SchemaForMigratedFields(targetSchema), result.Fields)
	if err := items.ValidateFields(result.Fields, items.SchemaForMigratedFields(targetSchema)); err != nil {
		return nil, &bulkOpError{message: err.Error(), code: "validation_error"}
	}
	items.DropBlankRelations(result.Fields, items.SchemaForMigratedFields(targetSchema), nil)
	// Relation defaults ValidateFields just injected (codex round 2). After
	// validation for the reason ResolveLateRelationDefaults documents.
	lateDropped, lateErr := s.store.ResolveLateRelationDefaults(
		s.relationVisibility(r, workspaceRole(r)),
		workspaceID, items.SchemaForMigratedFields(targetSchema), result.Fields, relBefore)
	if lateErr != nil {
		return nil, &bulkOpError{message: "Failed to resolve relation references", code: "internal_error"}
	}
	if cerr := s.collapseInvisibleRelationIssues(r, workspaceID, workspaceRole(r), lateDropped); cerr != nil {
		return nil, &bulkOpError{message: "Failed to resolve relation references", code: "internal_error"}
	}
	// A REQUIRED relation whose default did not resolve cannot be left as a
	// drop: the key is deleted AFTER validation passed, so nothing re-checks
	// it and the item would land with a required field absent, reported valid
	// (codex round 3). Re-running validation is not the answer — it would
	// re-inject the same broken default. There is no valid value, so this
	// refuses.
	if req := store.RequiredRelationIssues(items.SchemaForMigratedFields(targetSchema), lateDropped); len(req) > 0 {
		return nil, &bulkOpError{
			message: "required fields missing: " + relationIssuesMessage(req),
			code:    "missing_required_fields",
		}
	}
	invisibleDefaults, invErr := s.dropInvisibleRelationDefaults(r, workspaceID, workspaceRole(r),
		items.SchemaForMigratedFields(targetSchema), result.Fields,
		notDefaultKeys(suppliedByCaller, store.CarriedSourceValues(currentFields, result.Dropped)))
	if invErr != nil {
		return nil, &bulkOpError{message: "Failed to resolve relation references", code: "internal_error"}
	}
	if req := store.RequiredRelationIssues(items.SchemaForMigratedFields(targetSchema), invisibleDefaults); len(req) > 0 {
		return nil, &bulkOpError{
			message: "required fields missing: " + relationIssuesMessage(req),
			code:    "missing_required_fields",
		}
	}
	for _, ri := range append(lateDropped, invisibleDefaults...) {
		result.Dropped = append(result.Dropped, ri.Key)
	}
	// Hand the discarded keys back so the caller's activity row can name them
	// (BUG-2674, which fixed only the SINGLE-item move). `result.Dropped` has
	// been populated on this path since MigrateFields existed and NOTHING read
	// it — a bulk move discarded field values with no record anywhere, which is
	// the same silence that convention closed one door over. Filtered against
	// the FINAL map for the reason handleMoveItem filters: a key MigrateFields
	// listed may have been re-supplied or re-defaulted since, and reporting
	// that would be a confident falsehood about data sitting on the item.
	// Same uniqueness rule, check and sentence as handleMoveItem (BUG-2367):
	// before it, a collision on invocation_slug surfaced as the raw SQL error
	// text in `failed[]`, and one on an unindexed unique field was stored.
	if conflicts, uerr := s.store.UniqueFieldConflictsQ(s.store.Q(), targetColl.ID, item.ID, targetSchema.Fields, result.Fields); uerr != nil {
		return nil, &bulkOpError{message: "failed to check unique fields", code: "internal_error"}
	} else if len(conflicts) > 0 {
		return nil, &bulkOpError{message: store.UniqueFieldConflictsMessage(conflicts), code: "conflict"}
	}
	if droppedFields != nil {
		*droppedFields = items.StillDropped(result.Dropped, result.Fields)
	}

	fieldsJSON, err := json.Marshal(result.Fields)
	if err != nil {
		return nil, &bulkOpError{message: "failed to serialize fields"}
	}

	// Open-children guard (unless force), classified against the
	// DESTINATION schema — same as handleMoveItem. A collection move
	// that also sets a terminal status would otherwise mark a parent
	// terminal while it still has open children. Routing every
	// collection move through MoveItemWithPreCheck closes the bypass
	// Codex flagged (a status-only move already ran the guard).
	var precheck func(tx *sql.Tx, existing *models.Item) error
	if !req.Force {
		var destSettings models.CollectionSettings
		if targetColl.Settings != "" {
			_ = json.Unmarshal([]byte(targetColl.Settings), &destSettings)
		}
		guestFull, guestGranted, gerr := s.guestResourceFilter(r, workspaceID)
		if gerr != nil {
			return nil, &bulkOpError{message: gerr.Error()}
		}
		mgctx := openChildrenGuardContext{
			r:                    r,
			workspaceID:          workspaceID,
			itemID:               item.ID,
			parentSchema:         targetSchema,
			parentSettings:       destSettings,
			newFieldMap:          result.Fields,
			visibleCollectionIDs: visibleIDs,
			guestFullCollIDs:     guestFull,
			guestGrantedItemIDs:  guestGranted,
		}
		precheck = func(tx *sql.Tx, existing *models.Item) error {
			txCtx := mgctx
			txCtx.currentFieldsJS = existing.Fields
			details, derr := s.runOpenChildrenGuard(tx, txCtx)
			if derr != nil {
				return derr
			}
			if details != nil {
				return &openChildrenGuardError{details: details}
			}
			return nil
		}
	}

	moved, err := s.store.MoveItemWithPreCheck(item.ID, targetColl.ID, string(fieldsJSON), precheck, store.WithEventBatch(batchID))
	if err != nil {
		if details, ok := asOpenChildrenGuardError(err); ok {
			raw, _ := json.Marshal(details)
			return nil, &bulkOpError{
				message: "cannot mark item terminal while it has open children",
				code:    "open_children",
				details: raw,
			}
		}
		return nil, bulkStoreError(err)
	}
	if len(notUnique) > 0 {
		if moved.Warnings == nil {
			moved.Warnings = &models.ItemWriteWarnings{}
		}
		moved.Warnings.NotUnique = notUnique
	}
	return moved, nil
}

// publishBulkItemsEvent emits one ItemsBulkUpdated SSE event for the
// slice of a batch mutation that landed in `collection` (TASK-1668).
// Collection is set so the SSE visibility filter routes it like any
// collection-scoped event; the payload carries the verb, the
// per-collection count, and the max seq as the reconcile cursor — but
// no per-item IDs (see events.Event doc for why).
func (s *Server) publishBulkItemsEvent(workspaceID, op, collection string, count int, actor, actorName, source string, maxSeq int64) {
	if s.events == nil {
		return
	}
	s.publishActivityEvent(events.Event{
		Type:        sseItemsBulk,
		WorkspaceID: workspaceID,
		Collection:  collection,
		Op:          op,
		Count:       count,
		Actor:       actor,
		ActorName:   actorName,
		Source:      source,
		Seq:         maxSeq,
	})
}

// bulkEventDelta is the SHARED delta of a bulk operation — the change every
// member received, as opposed to each member's post-mutation snapshot.
//
// Only the handler knows this. By the time the drain sees the member rows they
// carry full snapshots, and a diff of a snapshot against nothing is not a
// delta. SPEC-3's item_batch payload names the shared delta as a field, which
// is why it has to be captured here rather than reconstructed later.
//
// archive and restore return nil: the operation IS the delta, it is already on
// the envelope as `op`, and inventing a {"deleted": true} field would put a
// key on the public wire that no mutation actually wrote.
func bulkEventDelta(req *bulkItemsRequest) map[string]any {
	switch req.Op {
	case "set-priority":
		return map[string]any{"priority": req.Priority}
	case "tag", "untag":
		// NORMALIZED THE SAME WAY THE MUTATION DOES (codex round 7). bulkTagUpdate
		// trims each added tag and skips the ones that go empty, so a request
		// carrying "  " changed nothing while a raw delta would announce it.
		// Untag matches on the raw value — it never trims — so only the add
		// side normalizes here, exactly as in the mutation.
		// De-duplicate for BOTH verbs, and trim only for `tag` — which is
		// exactly what the mutation does. bulkTagUpdate skips a tag already in
		// its `seen` set and trims each ADDED tag, while untag builds a removal
		// SET from the raw values, so duplicates and untrimmed strings behave
		// differently on the two sides. A delta echoing the request advertised
		// two changes where one happened (codex rounds 8-9).
		tags := make([]string, 0, len(req.Tags))
		seen := map[string]bool{}
		for _, t := range req.Tags {
			if req.Op == "tag" {
				t = strings.TrimSpace(t)
				if t == "" {
					continue
				}
			}
			if seen[t] {
				continue
			}
			seen[t] = true
			tags = append(tags, t)
		}
		return map[string]any{"tags": tags}
	case "move":
		// BOTH, when both were sent. bulkMoveCollection applies req.Status as
		// a field override on the migrated set, so a move-with-status changes
		// two things and a delta naming only the collection under-reports it
		// (codex round 1).
		delta := map[string]any{}
		if req.Collection != "" {
			delta["collection"] = req.Collection
		}
		if req.Status != "" {
			delta["status"] = req.Status
		}
		return delta
	case "assign":
		// SAME PRECEDENCE AS THE STORE (codex round 7): a non-empty id WINS
		// over the clear flag, and an explicit empty string clears exactly as
		// the flag does (see the assignment SET clauses in items.go — BUG-2566).
		// The delta had the flag winning, so a request carrying both announced
		// a clear while the row was assigned.
		delta := map[string]any{}
		if id := req.AssignedUserID; id != nil && *id != "" {
			delta["assigned_user_id"] = *id
		} else if req.ClearAssignedUser || (id != nil && *id == "") {
			delta["assigned_user_id"] = nil
		}
		if id := req.AgentRoleID; id != nil && *id != "" {
			delta["agent_role_id"] = *id
		} else if req.ClearAgentRole || (id != nil && *id == "") {
			delta["agent_role_id"] = nil
		}
		return delta
	}
	return nil
}

// requiredErrorsUnsatisfiedBy drops the required-field errors MigrateFields
// raised for keys the caller then supplied a value for.
//
// MigrateFields formats these as `required field %q has no value`, so the
// match is against that exact rendering for each supplied key rather than a
// substring of the message — a substring test would also match a key whose
// name contains another key's name.
func requiredErrorsUnsatisfiedBy(errs []string, supplied map[string]any) []string {
	if len(errs) == 0 || len(supplied) == 0 {
		return errs
	}
	satisfied := make(map[string]bool, len(supplied))
	for k, v := range supplied {
		if v == nil {
			continue
		}
		if str, isStr := v.(string); isStr && strings.TrimSpace(str) == "" {
			continue
		}
		satisfied[fmt.Sprintf("required field %q has no value", k)] = true
	}
	out := errs[:0:0]
	for _, e := range errs {
		if satisfied[e] {
			continue
		}
		out = append(out, e)
	}
	return out
}

// fieldsPatchFromMerge returns the field-level patch that turns `stored` into
// `merged`: every key whose value differs (added or changed), plus an explicit
// nil for every key `stored` had and `merged` does not, which
// store.mergeFieldsPatch treats as a delete.
//
// BUG-3049. The point is what it OMITS: a key present in both with an equal
// value is left out entirely, so a write built from a snapshot read outside the
// write transaction can no longer revert a concurrent change to a key it never
// meant to touch.
//
// Equality is reflect.DeepEqual over the decoded JSON values. Two values that
// are semantically equal but decode differently (a number stored as a string,
// then coerced) compare unequal and stay in the patch — see the call site's
// note on that residual.
func fieldsPatchFromMerge(stored, merged map[string]any) map[string]any {
	patch := make(map[string]any, len(merged))
	for k, v := range merged {
		if old, ok := stored[k]; ok && reflect.DeepEqual(old, v) {
			continue
		}
		patch[k] = v
	}
	for k := range stored {
		if _, ok := merged[k]; !ok {
			patch[k] = nil
		}
	}
	return patch
}
