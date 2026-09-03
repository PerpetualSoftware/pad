package mcp

// BUG-2850, codex round 7 boundary + the lead's condition on it.
//
// The remote door's half of the same-name-duplicate contract;
// cmd/pad/item_same_name_duplicate_precedence_test.go is the stdio door's.
// Both assert the SAME outcome, which is the point: the round-7 decision not
// to refuse `status` + `field:["status=…"]` rests entirely on the two doors
// resolving it identically, and nothing enforced that until these two tests
// existed. Reorder either overlay and exactly one of them goes red.
//
// parent+plan is the case that IS refused — two names for one target, which a
// caller can collide without knowing. A same-name duplicate is visibly a
// duplicate, so last-write-wins is a resolution the caller can predict.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestDispatchItemUpdate_SameNameDuplicateResolvesFieldWins(t *testing.T) {
	captured := newRequestCapture()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items/TASK-5", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"TASK-5","fields":"{}"}`))
		case http.MethodPatch:
			captured.ServeHTTP(w, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"TASK-5"}`))
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "caller"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp",
		"ref":       "TASK-5",
		"status":    "open",
		"field":     []any{"status=done"},
	})
	res, err := d.Dispatch(ctx, []string{"item", "update"}, nil)
	if err != nil || res.IsError {
		t.Fatalf("a same-name duplicate must resolve, not refuse: err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(captured.lastBody), &body); err != nil {
		t.Fatalf("decode body: %v\n%s", err, captured.lastBody)
	}
	patch, ok := body["fields_patch"].(map[string]any)
	if !ok {
		t.Fatalf("fields_patch not an object in body: %v", body)
	}
	if got := patch["status"]; got != "done" {
		t.Fatalf("status = %v, want %q — the field entry overlays the named param on this door too", got, "done")
	}
}
