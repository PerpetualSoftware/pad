package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/items"
	"github.com/PerpetualSoftware/pad/internal/models"
)

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
// code/details (currently only open_children).
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

	// Validate the verb + its required params up front so a malformed
	// request fails fast before touching any rows.
	switch req.Op {
	case "archive":
	case "move":
		if req.Status == "" && req.Collection == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "move requires status or collection")
			return
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

	for _, ref := range req.IDs {
		item, err := s.store.ResolveItem(workspaceID, ref)
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
		visible, verr := s.checkItemVisible(workspaceID, item, user, role)
		if verr != nil {
			resp.Failed = append(resp.Failed, bulkItemFailure{Ref: ref, Error: verr.Error()})
			continue
		}
		if !visible {
			resp.Failed = append(resp.Failed, bulkItemFailure{Ref: ref, Error: "item not found"})
			continue
		}

		updated, opErr := s.applyBulkOp(r, workspaceID, item, &req, actor, source, visibleIDs)
		if opErr != nil {
			resp.Failed = append(resp.Failed, bulkItemFailure{
				Ref:     itemRefOrSlug(*item),
				Error:   opErr.message,
				Code:    opErr.code,
				Details: opErr.details,
			})
			continue
		}

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
		case req.Op == "move" && req.Collection != "" && req.Collection != item.CollectionSlug:
			action = "moved"
			meta["from_collection"] = item.CollectionSlug
			meta["to_collection"] = req.Collection
		}
		s.logActivityWithMeta(workspaceID, item.ID, action, r, auditMeta(meta))

		resp.Updated = append(resp.Updated, bulkItemOutcome{Ref: itemRefOrSlug(*item), ID: item.ID})
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

	// One SSE batch event per affected collection + ONE webhook for the
	// whole batch (only when something actually changed). The core of
	// the task: a whole-lane bulk action must not emit N per-item events.
	// Per-collection (not fully per-item) keeps SSE visibility routing
	// correct while still collapsing a lane action to a single event.
	if len(affectedIDs) > 0 {
		for collSlug, b := range batches {
			s.publishBulkItemsEvent(workspaceID, req.Op, collSlug, b.count, actor, actorName, source, b.maxSeq)
		}
		// The webhook is a trusted workspace integration (not
		// visibility-scoped per subscriber), so it keeps the full id
		// list for the whole batch.
		s.dispatchWebhook(workspaceID, "item.bulk_updated", map[string]any{
			"op":       req.Op,
			"count":    len(affectedIDs),
			"item_ids": affectedIDs,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// applyBulkOp dispatches one verb against one item, reusing the same
// store paths as the single-item handlers. Returns the post-mutation
// item (for seq) on success, or a structured per-row error.
func (s *Server) applyBulkOp(r *http.Request, workspaceID string, item *models.Item, req *bulkItemsRequest, actor, source string, visibleIDs []string) (*models.Item, *bulkOpError) {
	switch req.Op {
	case "archive":
		if err := s.store.DeleteItem(item.ID); err != nil {
			return nil, &bulkOpError{message: err.Error()}
		}
		// DeleteItem bumps seq; re-read so the batch event carries the
		// post-archive cursor. Falls back to the pre-delete row on a
		// lookup miss (downstream backfills on a stale/zero seq).
		if d, derr := s.store.GetItemIncludeDeleted(item.ID); derr == nil && d != nil {
			return d, nil
		}
		return item, nil

	case "move":
		if req.Collection != "" {
			return s.bulkMoveCollection(r, workspaceID, item, req, visibleIDs)
		}
		// Status-only move = a field update on the same collection.
		return s.bulkFieldUpdate(r, workspaceID, item, map[string]any{"status": req.Status}, req.Force, visibleIDs, actor, source)

	case "set-priority":
		return s.bulkFieldUpdate(r, workspaceID, item, map[string]any{"priority": req.Priority}, req.Force, visibleIDs, actor, source)

	case "tag":
		return s.bulkTagUpdate(item, req.Tags, true, actor, source)

	case "untag":
		return s.bulkTagUpdate(item, req.Tags, false, actor, source)

	case "assign":
		input := models.ItemUpdate{
			AssignedUserID:    req.AssignedUserID,
			AgentRoleID:       req.AgentRoleID,
			ClearAssignedUser: req.ClearAssignedUser,
			ClearAgentRole:    req.ClearAgentRole,
			LastModifiedBy:    actor,
			Source:            source,
		}
		updated, err := s.store.UpdateItem(item.ID, input)
		if err != nil {
			return nil, &bulkOpError{message: err.Error()}
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
func (s *Server) bulkFieldUpdate(r *http.Request, workspaceID string, item *models.Item, changes map[string]any, force bool, visibleIDs []string, actor, source string) (*models.Item, *bulkOpError) {
	coll, err := s.store.GetCollection(item.CollectionID)
	if err != nil || coll == nil {
		return nil, &bulkOpError{message: "failed to load collection"}
	}
	var schema models.CollectionSchema
	if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
		return nil, &bulkOpError{message: "failed to parse collection schema"}
	}

	fieldMap := make(map[string]any)
	if item.Fields != "" && item.Fields != "{}" {
		_ = json.Unmarshal([]byte(item.Fields), &fieldMap)
	}
	for k, v := range changes {
		fieldMap[k] = v
	}

	if err := items.ValidateFields(fieldMap, schema); err != nil {
		return nil, &bulkOpError{message: err.Error(), code: "validation_error"}
	}
	if err := s.checkUniqueFields(workspaceID, item.CollectionID, item.ID, schema, fieldMap); err != nil {
		return nil, &bulkOpError{message: err.Error(), code: "conflict"}
	}
	autoPopulateDates(fieldMap, item.Fields, schema)

	var precheck func(tx *sql.Tx, existing *models.Item) error
	if !force {
		var settings models.CollectionSettings
		if coll.Settings != "" {
			_ = json.Unmarshal([]byte(coll.Settings), &settings)
		}
		guestFull, guestGranted, gerr := s.guestResourceFilter(r, workspaceID)
		if gerr != nil {
			return nil, &bulkOpError{message: gerr.Error()}
		}
		gctx := openChildrenGuardContext{
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
		precheck = func(tx *sql.Tx, existing *models.Item) error {
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

	fieldsJSON, err := json.Marshal(fieldMap)
	if err != nil {
		return nil, &bulkOpError{message: "failed to marshal fields"}
	}
	fieldsStr := string(fieldsJSON)
	input := models.ItemUpdate{
		Fields:         &fieldsStr,
		LastModifiedBy: actor,
		Source:         source,
	}

	updated, err := s.store.UpdateItemWithPreCheck(item.ID, input, precheck)
	if err != nil {
		if details, ok := asOpenChildrenGuardError(err); ok {
			raw, _ := json.Marshal(details)
			return nil, &bulkOpError{
				message: "cannot mark item terminal while it has open children",
				code:    "open_children",
				details: raw,
			}
		}
		return nil, &bulkOpError{message: err.Error()}
	}
	if updated == nil {
		return nil, &bulkOpError{message: "item not found"}
	}
	return updated, nil
}

// bulkTagUpdate adds (add=true) or removes (add=false) the given tags
// from the item's tag set, preserving existing order and de-duplicating.
func (s *Server) bulkTagUpdate(item *models.Item, tags []string, add bool, actor, source string) (*models.Item, *bulkOpError) {
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
	})
	if err != nil {
		return nil, &bulkOpError{message: err.Error()}
	}
	return updated, nil
}

// bulkMoveCollection moves one item into req.Collection, migrating its
// fields between schemas — the same core as handleMoveItem, applied
// per row. A status override (req.Status) lands as a field override on
// the migrated set.
func (s *Server) bulkMoveCollection(r *http.Request, workspaceID string, item *models.Item, req *bulkItemsRequest, visibleIDs []string) (*models.Item, *bulkOpError) {
	targetColl, err := s.store.GetCollectionBySlug(workspaceID, req.Collection)
	if err != nil || targetColl == nil {
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

	result := items.MigrateFields(currentFields, sourceSchema.Fields, targetSchema.Fields)
	if req.Status != "" {
		result.Fields["status"] = req.Status
	}
	if len(result.Errors) > 0 {
		return nil, &bulkOpError{
			message: "required fields missing: " + strings.Join(result.Errors, ", "),
			code:    "missing_required_fields",
		}
	}
	// Validate the final field map (including any status override)
	// against the TARGET schema — MigrateFields validates migrated
	// values but an override can smuggle in a value the target schema
	// doesn't allow (e.g. a status not in the target's options).
	if err := items.ValidateFields(result.Fields, targetSchema); err != nil {
		return nil, &bulkOpError{message: err.Error(), code: "validation_error"}
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

	moved, err := s.store.MoveItemWithPreCheck(item.ID, targetColl.ID, string(fieldsJSON), precheck)
	if err != nil {
		if details, ok := asOpenChildrenGuardError(err); ok {
			raw, _ := json.Marshal(details)
			return nil, &bulkOpError{
				message: "cannot mark item terminal while it has open children",
				code:    "open_children",
				details: raw,
			}
		}
		return nil, &bulkOpError{message: err.Error()}
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
	s.events.Publish(events.Event{
		Type:        events.ItemsBulkUpdated,
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
