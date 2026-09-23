package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
)

// BUG-3156 (b): `pad item bulk-update` sends refuse_undeclared_fields, so an
// item whose collection declares no status/priority is refused by the SERVER
// into `failed` (validation_error) instead of storing an orphan field. What the
// flag does is tested against the real handler in internal/server; this pins
// that the CLI actually SENDS it (CONVE-19: wiring is a claim).
func TestBulkUpdate_SendsRefuseUndeclaredFields(t *testing.T) {
	var mu sync.Mutex
	var patches []map[string]any
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "item-1", "slug": "a-note", "title": "A note",
				"collection_slug": "notes", "collection_prefix": "NOTE",
				"item_number": 1, "fields": `{"status":"open"}`,
			})
		case http.MethodPatch:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			patches = append(patches, body)
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code":    "validation_error",
				"message": `collection "notes" has no "priority" field, and refuse_undeclared_fields is set, so nothing was written`,
			}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	})
	setupPushTest(t, handler)

	cmd := bulkUpdateCmd()
	cmd.SetArgs([]string{"--priority", "high", "NOTE-1"})
	cmd.SilenceUsage = true
	// A per-row refusal is reported in the row, not as a command error.
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bulk-update: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(patches) != 1 {
		t.Fatalf("PATCH count = %d, want 1", len(patches))
	}
	if patches[0]["refuse_undeclared_fields"] != true {
		t.Fatalf("bulk-update did not send refuse_undeclared_fields: %v", patches[0])
	}
}
