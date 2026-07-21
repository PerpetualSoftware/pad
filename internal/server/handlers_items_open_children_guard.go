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
			if coll, cerr := s.store.GetCollectionAnyState(child.CollectionID); cerr == nil && coll != nil {
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
				"ref":                 ref,
				"expected_updated_at": expectedUpdatedAt,
				"actual_updated_at":   actualUpdatedAt.UTC().Format(time.RFC3339),
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
