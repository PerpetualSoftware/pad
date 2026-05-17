package server

// IDEA-1494 — Refuse to mark an item terminal while it still has
// non-terminal children. The guard fires server-side so it covers
// every interface (CLI, MCP, web UI) that hits handleUpdateItem.
//
// Trigger conditions (all must hold):
//   1. The PATCH supplies a fields update.
//   2. The new value for the parent collection's resolved done-field
//      key is in TerminalValuesForDoneField.
//   3. The CURRENT value for that key is NOT in the terminal set.
//      (no-op terminal → terminal and terminal-to-terminal transitions
//      bypass the guard — only entering the terminal set is gated.)
//   4. The parent item has at least one non-deleted child whose own
//      collection schema reports the child as non-terminal.
//
// The caller can override the guard with `--force` (CLI) / `force: true`
// (MCP body field). The override still records the status change.
//
// Response shape on rejection (HTTP 409 Conflict):
//
//	{
//	  "error": {
//	    "code": "open_children",
//	    "message": "cannot mark TASK-5 completed: 2 child task(s) still open. Pass --force to override.",
//	    "details": {
//	      "open_children": [
//	        {"ref":"TASK-7","title":"...","status":"open","collection_slug":"tasks"},
//	        ...
//	      ],
//	      "done_field": "status",
//	      "attempted_value": "completed"
//	    }
//	  }
//	}
//
// `details.open_children` is the canonical machine-readable list — the
// CLI renders the human message FROM the same list so the two paths
// agree by construction.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

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

// openChildrenError is the structured details payload.
type openChildrenDetails struct {
	OpenChildren   []openChildEntry `json:"open_children"`
	DoneField      string           `json:"done_field"`
	AttemptedValue string           `json:"attempted_value"`
}

// evaluateOpenChildrenGuard returns a non-nil details payload when the
// proposed update would transition the item into a terminal state and
// the item has at least one non-terminal child. Returns (nil, nil) when
// the guard does not apply (no transition, no children, no open
// children). Errors from store calls propagate.
//
// newFieldMap is the post-validation merged fields map (the value about
// to be persisted). currentFieldsJSON is the item's pre-update fields
// JSON. parentSchema/parentSettings drive done-field resolution for the
// parent item being updated. Each child is evaluated against ITS OWN
// collection's schema + settings (not the parent's).
func (s *Server) evaluateOpenChildrenGuard(
	itemID string,
	currentFieldsJSON string,
	newFieldMap map[string]any,
	parentSchema models.CollectionSchema,
	parentSettings models.CollectionSettings,
) (*openChildrenDetails, error) {
	doneKey, terminalValues := models.TerminalValuesForDoneField(parentSchema, parentSettings)

	// New value must be present in the patch and terminal.
	rawNew, ok := newFieldMap[doneKey]
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

	// Current value must NOT already be terminal — we only gate the
	// non-terminal → terminal edge. terminal → terminal (e.g.
	// completed → archived) and terminal → same-terminal (no-op) both
	// bypass.
	currentVal := extractFieldString(currentFieldsJSON, doneKey)
	if currentVal != "" && valueInSet(currentVal, terminalValues) {
		return nil, nil
	}

	// Cheap exit: no children at all.
	children, err := s.store.GetChildItems(itemID)
	if err != nil {
		return nil, fmt.Errorf("load children for open-children guard: %w", err)
	}
	if len(children) == 0 {
		return nil, nil
	}

	// Per-child done evaluation against the child's OWN collection
	// schema. Cache schema+settings per child collection.
	ctxCache := make(map[string]doneContext)
	var open []openChildEntry
	for i := range children {
		child := &children[i]
		ctx, cached := ctxCache[child.CollectionID]
		if !cached {
			if coll, cerr := s.store.GetCollection(child.CollectionID); cerr == nil && coll != nil {
				_ = json.Unmarshal([]byte(coll.Schema), &ctx.schema)
				if coll.Settings != "" {
					_ = json.Unmarshal([]byte(coll.Settings), &ctx.settings)
				}
			}
			ctxCache[child.CollectionID] = ctx
		}
		if isItemDone(child.Fields, child.CollectionID, map[string]doneContext{child.CollectionID: ctx}) {
			continue
		}
		childDoneKey, _ := models.TerminalValuesForDoneField(ctx.schema, ctx.settings)
		open = append(open, openChildEntry{
			Ref:            itemRefOrSlug(*child),
			Title:          child.Title,
			Status:         extractFieldString(child.Fields, childDoneKey),
			CollectionSlug: child.CollectionSlug,
		})
	}
	if len(open) == 0 {
		return nil, nil
	}
	return &openChildrenDetails{
		OpenChildren:   open,
		DoneField:      doneKey,
		AttemptedValue: newStr,
	}, nil
}

// writeOpenChildrenError emits the 409 response with both a human
// message AND the structured details payload. `parentRef` is the
// already-formatted parent ref (e.g. "PLAN-12"); the message names the
// parent + child count and points at --force.
func writeOpenChildrenError(w http.ResponseWriter, parentRef string, details *openChildrenDetails) {
	noun := "child"
	if len(details.OpenChildren) != 1 {
		noun = "children"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "cannot mark %s %s: %d open %s still in a non-terminal state. Pass --force to override.",
		parentRef, details.AttemptedValue, len(details.OpenChildren), noun)

	writeJSON(w, http.StatusConflict, map[string]any{
		"error": map[string]any{
			"code":    "open_children",
			"message": b.String(),
			"details": details,
		},
	})
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
