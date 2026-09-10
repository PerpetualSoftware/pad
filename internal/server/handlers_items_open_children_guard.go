package server

// IDEA-1494 — Refuse to mark an item terminal while it still has
// non-terminal children. The guard fires server-side so it covers
// every interface (CLI, MCP, web UI) that hits handleUpdateItem.
//
// Trigger conditions (all must hold):
//  1. The PATCH supplies a fields update.
//  2. The new value for the parent collection's resolved done-field
//     key is in TerminalValuesForDoneField.
//  3. The CURRENT value for that key is NOT in the terminal set.
//     (no-op terminal → terminal and terminal-to-terminal transitions
//     bypass the guard — only entering the terminal set is gated.)
//  4. The parent item has at least one non-deleted child whose own
//     collection schema reports the child as non-terminal.
//
// The caller can override the guard with `--force` (CLI) / `force: true`
// (MCP body field). The override still records the status change.
//
// ── Visibility (Codex round 2 P1) ─────────────────────────────────────
// The invariant itself is a DATA-INTEGRITY gate, not a visibility
// gate: we evaluate it against ALL children (so a caller with reduced
// visibility can't close a parent that has children they don't see).
// The 409 response payload, however, is sanitized — only children the
// caller is allowed to see appear in `details.open_children`. When
// hidden children contributed to the rejection (in part or in full)
// the payload carries a separate `hidden_blocker_count` so MCP-driven
// agents can distinguish:
//
//   - len(open_children)==0 + hidden_blocker_count==0 → would not have
//     rejected (no path here, but the shape is unambiguous);
//   - len(open_children)==N + hidden_blocker_count==0 → caller can see
//     every blocker;
//   - len(open_children)==N + hidden_blocker_count==M → caller sees N
//     blockers + M additional that they can't access; recovery requires
//     either coordination with someone who can or `--force` if their
//     role permits.
//
// ── Atomicity (Codex round 2 P2) ──────────────────────────────────────
// The guard query runs INSIDE the same store transaction as the
// UPDATE, so a concurrent child insert / child status flip can't slip
// between the read and the write. See Store.UpdateItemWithPreCheck +
// Store.AcquireParentChildrenLocks for the locking shape.
//
// ── Response (HTTP 409 Conflict) ──────────────────────────────────────
//
//	{
//	  "error": {
//	    "code": "open_children",
//	    "message": "cannot mark TASK-5 completed: ...",
//	    "details": {
//	      "open_children": [
//	        {"ref":"TASK-7","title":"...","status":"open","collection_slug":"tasks"},
//	        ...
//	      ],
//	      "hidden_blocker_count": 0,
//	      "done_field": "status",
//	      "attempted_value": "completed"
//	    }
//	  }
//	}
//
// `details.open_children` is the canonical machine-readable list — the
// CLI renders the human message FROM the same list (plus the hidden
// count) so the two paths agree by construction.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// errOpenChildrenGuard is the sentinel the precheck returns to the
// store layer when the guard fires. The handler unwraps it via
// errors.As to lift the structured details back out for the 409
// response. Using a typed sentinel (rather than a generic error)
// keeps the store/handler boundary clean — the store just sees "the
// precheck rejected" and rolls back the tx.
type openChildrenGuardError struct {
	details *openChildrenDetails
}

func (e *openChildrenGuardError) Error() string {
	return fmt.Sprintf("open-children guard: %d visible + %d hidden blocker(s)",
		len(e.details.OpenChildren), e.details.HiddenBlockerCount)
}

// itemRefOrSlug formats an item's issue ref (e.g. "TASK-5") when its
// collection prefix + item number are populated, falling back to its
// slug. Avoids pulling internal/cli into the server package for one
// helper.
func itemRefOrSlug(it models.Item) string {
	if it.CollectionPrefix != "" && it.ItemNumber != nil {
		return fmt.Sprintf("%s-%d", it.CollectionPrefix, *it.ItemNumber)
	}
	return it.Slug
}

// openChildEntry is the per-child payload returned in the structured
// error. Mirrors what `pad item list --parent X --status non-terminal`
// would surface so MCP-driven agents can self-recover (e.g. ship the
// children, then retry).
type openChildEntry struct {
	Ref            string `json:"ref"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	CollectionSlug string `json:"collection_slug"`
}

// openChildrenDetails is the structured details payload returned in
// the 409 error body. `OpenChildren` is filtered for caller visibility;
// `HiddenBlockerCount` reports the number of additional blockers that
// would also need to clear (or be force-overridden) but that the
// caller can't see.
type openChildrenDetails struct {
	OpenChildren       []openChildEntry `json:"open_children"`
	HiddenBlockerCount int              `json:"hidden_blocker_count"`
	DoneField          string           `json:"done_field"`
	AttemptedValue     string           `json:"attempted_value"`
}

// openChildrenGuardContext bundles the read-only inputs the precheck
// closure needs from the handler. Kept as a struct so the closure
// signature stays narrow.
type openChildrenGuardContext struct {
	r               *http.Request
	workspaceID     string
	itemID          string
	parentSchema    models.CollectionSchema
	parentSettings  models.CollectionSettings
	newFieldMap     map[string]any
	currentFieldsJS string
	// visibility filters, all pre-computed once by the handler.
	visibleCollectionIDs []string // nil = unrestricted
	guestFullCollIDs     []string
	guestGrantedItemIDs  []string
}

// runOpenChildrenGuard executes the guard logic inside the
// store-layer transaction. Returns:
//
//   - (nil, nil) when the guard doesn't apply (no transition, no
//     children, or all children terminal) — the update should proceed.
//   - (details, nil) when the guard fires — the caller wraps these in
//     openChildrenGuardError so the store rolls back. `details` is
//     already sanitized for visibility.
//   - (nil, err) on infrastructure errors (DB read failed, etc.).
func (s *Server) runOpenChildrenGuard(tx *sql.Tx, ctx openChildrenGuardContext) (*openChildrenDetails, error) {
	doneKey, terminalValues := models.TerminalValuesForDoneField(ctx.parentSchema, ctx.parentSettings)

	// Trigger condition #1: the patch must set the resolved done-field
	// key to a terminal value.
	rawNew, ok := ctx.newFieldMap[doneKey]
	if !ok {
		return nil, nil
	}
	newStr, ok := rawNew.(string)
	if !ok || newStr == "" {
		return nil, nil
	}
	if !valueInSet(newStr, terminalValues) {
		return nil, nil
	}
	// Trigger condition #3: the current value must NOT already be
	// terminal. terminal → terminal and no-op terminal transitions
	// bypass.
	currentVal := extractFieldString(ctx.currentFieldsJS, doneKey)
	if currentVal != "" && valueInSet(currentVal, terminalValues) {
		return nil, nil
	}

	children, err := s.store.GetChildItemsTx(tx, ctx.itemID)
	if err != nil {
		return nil, fmt.Errorf("load children for open-children guard: %w", err)
	}
	if len(children) == 0 {
		return nil, nil
	}

	// Per-child done evaluation against the child's OWN collection.
	// Cache schema+settings per child collection. The collection rows
	// don't need to be read from the same tx — schemas don't mutate
	// in the kind of races this guard cares about.
	ctxCache := make(map[string]doneContext)
	// Codex round-5 P3: initialize as empty (not nil) so the
	// hidden-only rejection path serializes `open_children: []`
	// rather than `null`. The contract documents this field as an
	// array; clients (CLI renderer, MCP agents) `range` over it
	// even when hidden_blocker_count > 0.
	open := []openChildEntry{}
	hidden := 0
	for i := range children {
		child := &children[i]
		dc, cached := ctxCache[child.CollectionID]
		if !cached {
			// Codex round-3 P3: include soft-deleted collections so a
			// child still attached to a soft-deleted collection is
			// evaluated against its own done-field schema, not the
			// default-status fallback (which would false-block when
			// the collection's done-field is e.g. `resolution` with
			// custom terminal_options). Mirrors the inclusion rule
			// childrenDoneFiltersForParent uses (items.go ≈2165).
			// Through the guard's OWN transaction (BUG-2778): this runs
			// inside the item-update tx, and a pool read from here needs a
			// second connection while that tx holds the first.
			if coll, cerr := s.store.GetCollectionAnyStateTx(tx, child.CollectionID); cerr == nil && coll != nil {
				_ = json.Unmarshal([]byte(coll.Schema), &dc.schema)
				if coll.Settings != "" {
					_ = json.Unmarshal([]byte(coll.Settings), &dc.settings)
				}
			}
			ctxCache[child.CollectionID] = dc
		}
		// INVARIANT check uses ALL children. A restricted caller still
		// gets blocked by a non-terminal child they can't see — the
		// guard's purpose is data integrity, not visibility filtering.
		if isItemDone(child.Fields, child.CollectionID, map[string]doneContext{child.CollectionID: dc}) {
			continue
		}

		// Sanitize the response payload: surface only children this
		// caller has permission to see. Hidden blockers are counted
		// separately so the agent knows blocking state exists without
		// learning ref/title/status of items they can't access.
		if !s.openChildrenGuardChildVisible(ctx, child) {
			hidden++
			continue
		}
		childDoneKey, _ := models.TerminalValuesForDoneField(dc.schema, dc.settings)
		open = append(open, openChildEntry{
			Ref:            itemRefOrSlug(*child),
			Title:          child.Title,
			Status:         extractFieldString(child.Fields, childDoneKey),
			CollectionSlug: child.CollectionSlug,
		})
	}
	if len(open) == 0 && hidden == 0 {
		return nil, nil
	}
	return &openChildrenDetails{
		OpenChildren:       open,
		HiddenBlockerCount: hidden,
		DoneField:          doneKey,
		AttemptedValue:     newStr,
	}, nil
}

// openChildrenGuardChildVisible mirrors the visibility check used by
// the per-parent progress endpoint (handlers_items.go around
// `progVisIDs` / `isCollectionVisible` / `isItemVisibleToGuest`).
// Returns true when the caller is unrestricted or when the child
// passes both the collection-level and item-level guest filters.
func (s *Server) openChildrenGuardChildVisible(gctx openChildrenGuardContext, child *models.Item) bool {
	// Unrestricted (admin / owner / no grant filtering in play): nil
	// visibleCollectionIDs means "see everything." Matches the
	// progress handler's convention.
	if gctx.visibleCollectionIDs == nil {
		return true
	}
	if !isCollectionVisible(child.CollectionID, gctx.visibleCollectionIDs) {
		return false
	}
	return s.isItemVisibleToGuest(gctx.r, gctx.workspaceID, child, gctx.guestFullCollIDs, gctx.guestGrantedItemIDs)
}

// writeOpenChildrenError emits the 409 response with both a human
// message AND the structured details payload. `parentRef` is the
// already-formatted parent ref (e.g. "PLAN-12"); the message names the
// parent + child count and points at --force. The phrasing splits on
// whether any blockers are hidden so the human-readable line carries
// the same signal `hidden_blocker_count` does for machines.
func writeOpenChildrenError(w http.ResponseWriter, parentRef string, details *openChildrenDetails) {
	visible := len(details.OpenChildren)
	hidden := details.HiddenBlockerCount
	var msg string
	switch {
	case visible > 0 && hidden > 0:
		msg = fmt.Sprintf("cannot mark %s %s: %d open child(ren) still in a non-terminal state, plus %d additional you don't have access to. Pass --force to override.",
			parentRef, details.AttemptedValue, visible, hidden)
	case visible > 0:
		noun := "child"
		if visible != 1 {
			noun = "children"
		}
		msg = fmt.Sprintf("cannot mark %s %s: %d open %s still in a non-terminal state. Pass --force to override.",
			parentRef, details.AttemptedValue, visible, noun)
	default:
		// hidden > 0 only — the caller can't see any blocking children.
		noun := "child"
		if hidden != 1 {
			noun = "children"
		}
		msg = fmt.Sprintf("cannot mark %s %s: blocked by %d open %s you don't have access to. Pass --force to override (if your role permits).",
			parentRef, details.AttemptedValue, hidden, noun)
	}

	writeJSON(w, http.StatusConflict, map[string]any{
		"error": map[string]any{
			"code":    "open_children",
			"message": msg,
			"details": details,
		},
	})
}

// writeUpdateConflictError emits the pad-structured-error/v1 conflict
// envelope (HTTP 409, code "update_conflict") when an optimistic-concurrency
// update loses the race (TASK-2022). `ref` is the already-formatted item ref
// (e.g. "TASK-5"). The details carry both timestamps so a client can decide
// whether to re-read + retry or surface the collision to the user.
func writeUpdateConflictError(w http.ResponseWriter, ref string, conflict *store.UpdateConflictError) {
	writeUpdateConflictEnvelope(w, ref, conflict.ExpectedUpdatedAt, conflict.ActualUpdatedAt)
}

// writeUpdateConflictEnvelope is the shared writer for the
// pad-structured-error/v1 update_conflict envelope. Both the item path
// (writeUpdateConflictError) and the collection path
// (writeCollectionUpdateConflictError, BUG-2265) emit the IDENTICAL wire shape
// through this helper so clients branch on one `code` + `details` contract
// regardless of which resource conflicted.
func writeUpdateConflictEnvelope(w http.ResponseWriter, ref, expectedUpdatedAt string, actualUpdatedAt time.Time) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": map[string]any{
			"code": "update_conflict",
			"message": fmt.Sprintf(
				"%s was modified by another writer since you last read it; re-read and retry.",
				ref),
			"details": map[string]any{
				"ref": ref,
				// expected_updated_at is echoed verbatim (the exact string the
				// caller sent). actual_updated_at MUST use full RFC3339Nano
				// precision so a client can round-trip it back as the token on
				// retry: collection tokens are now sub-second (BUG-2265), and
				// truncating to whole seconds (time.RFC3339) would hand back a
				// token that never matches, 409-looping forever. Item tokens are
				// second-precision (zero nanoseconds), so RFC3339Nano emits no
				// fractional part for them — byte-identical to the old output,
				// and the item path parses/compares via time.Equal regardless.
				"expected_updated_at": expectedUpdatedAt,
				"actual_updated_at":   actualUpdatedAt.UTC().Format(time.RFC3339Nano),
			},
		},
	})
}

// contentNotAppliedRetryAfterSeconds is the Retry-After hint on a room_settling
// refusal. One second: the wait that preceded it already covered the measured
// anchoring window with an order of magnitude to spare, so a room still unsettled
// after it is waiting on something slower than replay — a slow network, a wedged
// conn, a store under load — and a sub-second retry would just re-refuse.
const contentNotAppliedRetryAfterSeconds = 1

// writeContentNotAppliedError emits the pad-structured-error/v1 envelope for the
// outcome the write-first-apply-second ordering creates (PLAN-2975 decision 2): the
// row write COMMITTED and the content did not reach the collaborative document.
//
// It is a 409 rather than a 200-with-a-warning, and that is the ruled shape rather
// than a stylistic choice. The two failure modes are not symmetric: a client that
// ignores an advisory on a 200 believes the content landed and loses the information
// silently, while a client that meets a non-2xx retries — the fields it re-sends hit
// optimistic concurrency and converge, and the content it re-sends applies. The
// property being bought is that a response after which the content is not in the
// document is never readable as success.
//
// `landedFields` names what the row write did store, so the caller can tell that the
// non-content half of its PATCH is done and must not be re-sent blind.
// `actualUpdatedAt` is the post-write value: a content-only retry that echoes it back
// as expected_updated_at will not trip the OCC check on a timestamp this very request
// moved. `reason` carries the underlying apply failure so an operator can tell a
// timed-out applier from an evicted one.
//
// `contentOutcome` is the part a first draft of this got WRONG, and the reason it is
// a parameter rather than a constant false (codex round 1, this unit). An apply that
// TIMED OUT is not the same as one that never happened: ApplyExternalContent only
// returns ErrAllAppliersTimedOut after an applier_request has actually gone out on
// the wire, and the elected peer may have applied the markdown and persisted its ops
// while the ack was lost or merely late. Answering `content_landed: false` there
// states as fact something the server cannot know — the same overclaim the ruling
// avoided by leaving applier_ambiguous alone. So the envelope reports what the server
// can actually distinguish, and the discriminator already exists upstream:
// ErrNoActiveRoom / ErrNoApplierAvailable mean nothing ever reached a peer
// (anyWriteSucceeded == false), while ErrAllAppliersTimedOut means something did.
func writeContentNotAppliedError(w http.ResponseWriter, ref string, landedFields []string, actualUpdatedAt time.Time, contentOutcome, reason string) {
	if landedFields == nil {
		landedFields = []string{}
	}
	msg := fmt.Sprintf(
		"%s was updated, but its content could not be applied to the live collaborative document; retry the content on its own.",
		ref)
	details := map[string]any{
		"ref":           ref,
		"landed_fields": landedFields,
		// Full RFC3339Nano for the same reason writeUpdateConflictEnvelope uses it:
		// this value is meant to be round-tripped back as the caller's
		// expected_updated_at token.
		"actual_updated_at": actualUpdatedAt.UTC().Format(time.RFC3339Nano),
		"apply_reason":      reason,
		"content_outcome":   contentOutcome,
	}
	switch contentOutcome {
	case contentOutcomeNotApplied:
		details["content_landed"] = false
	default: // contentOutcomeUnknown
		// content_landed is DELIBERATELY ABSENT rather than false: the request went
		// out and may have been applied. A caller that retries converges either way
		// — a re-applied identical markdown is a no-op diff — but a caller that
		// reads content_landed:false may take an action premised on the content
		// being gone, and that premise would be unfounded.
		msg = fmt.Sprintf(
			"%s was updated, but the outcome of applying its content to the live collaborative document is unknown; retry the content on its own.",
			ref)
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": map[string]any{
			"code":    "content_not_applied",
			"message": msg,
			"details": details,
		},
	})
}

// contentOutcome values for writeContentNotAppliedError's details.
const (
	// contentOutcomeNotApplied — no applier_request ever reached a peer, so the
	// content demonstrably did not land.
	contentOutcomeNotApplied = "not_applied"
	// contentOutcomeUnknown — a request went out and was not acked in time. The
	// peer may have applied it.
	contentOutcomeUnknown = "unknown"
)

// writeRoomSettlingError emits the pad-structured-error/v1 envelope for a room that
// is neither settled enough to elect an applier nor empty enough to write directly
// (PLAN-2975 decision 2, standoff clause).
//
// That state is real and is not a race the server can resolve by trying harder:
// PruneAndApply blocks on any conn with canWrite, while election additionally
// requires the conn to be unfrozen and past its replay. A room whose only writer has
// joined but not finished replaying satisfies the first and fails the second, so the
// direct path refuses and the applier path has nobody to elect. Only the conn
// anchoring resolves it, which this request does not control.
//
// The predecessor behaviour was to give up after three attempts and write
// items.content directly past that live peer — a write the peer's next flush
// overwrites. A refusal the caller can retry is strictly better than a write that is
// silently lost, which is why this refusal replaces it on the applier route. NOTHING
// has been written when this fires: PruneAndApply returns before it calls applyFn.
func writeRoomSettlingError(w http.ResponseWriter, ref string) {
	w.Header().Set("Retry-After", strconv.Itoa(contentNotAppliedRetryAfterSeconds))
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": map[string]any{
			"code": "room_settling",
			"message": fmt.Sprintf(
				"%s has a collaborator connecting right now; nothing was changed. Retry in a moment.",
				ref),
			"details": map[string]any{
				"ref":                 ref,
				"retry_after_seconds": contentNotAppliedRetryAfterSeconds,
			},
		},
	})
}

// asUpdateConflictError reports whether err is (or wraps) a
// store.UpdateConflictError and returns it. Handlers use it to branch the
// generic upstream-error path into the structured 409 above.
func asUpdateConflictError(err error) (*store.UpdateConflictError, bool) {
	var conflict *store.UpdateConflictError
	if errors.As(err, &conflict) {
		return conflict, true
	}
	return nil, false
}

// asOpenChildrenGuardError unwraps a store-layer error that may carry
// an openChildrenGuardError sentinel. Returns the sanitized details
// and true when the error is the guard rejecting the precheck; nil +
// false for any other error (the handler returns those via the
// generic upstream-error path).
func asOpenChildrenGuardError(err error) (*openChildrenDetails, bool) {
	var sentinel *openChildrenGuardError
	if errors.As(err, &sentinel) {
		return sentinel.details, true
	}
	return nil, false
}

// valueInSet does a case-insensitive membership check.
func valueInSet(v string, set []string) bool {
	low := strings.ToLower(strings.TrimSpace(v))
	for _, s := range set {
		if strings.ToLower(s) == low {
			return true
		}
	}
	return false
}

// extractFieldString reads a top-level scalar string from a JSON-encoded
// fields map. Returns "" when the JSON is empty, malformed, the key is
// missing, or the value isn't a string.
func extractFieldString(fieldsJSON, key string) string {
	if fieldsJSON == "" || fieldsJSON == "{}" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &m); err != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}
