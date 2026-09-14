package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
)

// BUG-3049 — CLI doors that write item fields must name only the keys they own.
//
// Each of these commands did a GET, edited the decoded blob, and PATCHed the
// whole thing back. Anything written to the row between the GET and the PATCH
// was reverted: a network round trip wide for `bulk-update` (per row), and as
// wide as a `gh` API call for `pad project reconcile --apply`.
//
// The fake server below is not a spy — it IMPLEMENTS BOTH WRITE SEMANTICS the
// real server has (`fields` replaces the blob, `fields_patch` merges per key
// with nil deleting) and lands a concurrent write immediately after serving the
// client's GET. So the assertion is the real consequence — did the concurrent
// key survive — and a door that goes back to sending `fields` fails, rather
// than a test that merely reads the payload's shape and agrees with itself.

// fieldsRaceServer serves one item, applies `concurrent` onto its stored fields
// the first time the item is READ, and applies whatever write shape the client
// sends. Returns a func giving the item's stored fields at any point.
func fieldsRaceServer(t *testing.T, stored map[string]any, concurrent map[string]any) (http.Handler, func() map[string]any) {
	t.Helper()
	var mu sync.Mutex
	raced := false

	snapshot := func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]any, len(stored))
		for k, v := range stored {
			out[k] = v
		}
		return out
	}

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			fieldsJSON, _ := json.Marshal(stored)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "item-1", "slug": "race-target", "title": "Race target",
				"collection_slug": "tasks", "collection_prefix": "TASK",
				"item_number": 1, "fields": string(fieldsJSON),
				"schema": `{"fields":[{"key":"status","type":"select"},{"key":"priority","type":"select"},{"key":"category","type":"text"}]}`,
			})
			// The concurrent writer lands AFTER the client has read, which is
			// the window this bug lives in.
			if !raced {
				raced = true
				for k, v := range concurrent {
					stored[k] = v
				}
			}
		case http.MethodPatch:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if blob, ok := body["fields"].(string); ok {
				// Full replace — the pre-fix shape.
				replacement := map[string]any{}
				_ = json.Unmarshal([]byte(blob), &replacement)
				stored = replacement
			}
			if patch, ok := body["fields_patch"].(map[string]any); ok {
				for k, v := range patch {
					if v == nil {
						delete(stored, k)
						continue
					}
					stored[k] = v
				}
			}
			fieldsJSON, _ := json.Marshal(stored)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "item-1", "slug": "race-target", "title": "Race target",
				"collection_slug": "tasks", "collection_prefix": "TASK",
				"item_number": 1, "fields": string(fieldsJSON),
			})
		default:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	})
	return h, snapshot
}

func TestBulkUpdate_DoesNotRevertAConcurrentWriteToAnotherField(t *testing.T) {
	handler, storedNow := fieldsRaceServer(t,
		map[string]any{"status": "open", "priority": "high"},
		map[string]any{"category": "billing"},
	)
	setupPushTest(t, handler)

	cmd := bulkUpdateCmd()
	cmd.SetArgs([]string{"--status", "done", "TASK-1"})
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bulk-update: %v", err)
	}

	got := storedNow()
	// Premise: the command's own change landed. A no-op would satisfy the
	// survival assertion below for the wrong reason.
	if got["status"] != "done" {
		t.Fatalf("bulk-update did not apply its own change: status=%v want done", got["status"])
	}
	if got["category"] != "billing" {
		t.Errorf("BUG-3049: bulk-update reverted a field it did not name: category=%v want billing", got["category"])
	}
	if got["priority"] != "high" {
		t.Errorf("priority lost: got %v want high", got["priority"])
	}
}

func TestGitHubUnlink_DoesNotRevertAConcurrentWriteToAnotherField(t *testing.T) {
	handler, storedNow := fieldsRaceServer(t,
		map[string]any{"status": "open", "github_pr": map[string]any{"number": float64(41)}},
		map[string]any{"category": "billing"},
	)
	setupPushTest(t, handler)

	cmd := githubUnlinkCmd()
	cmd.SetArgs([]string{"TASK-1"})
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err != nil {
		t.Fatalf("github unlink: %v", err)
	}

	got := storedNow()
	// Premise: the unlink actually removed the key it exists to remove.
	if _, still := got["github_pr"]; still {
		t.Fatalf("github unlink did not remove github_pr: %v", got)
	}
	if got["category"] != "billing" {
		t.Errorf("BUG-3049: github unlink reverted a field it did not name: category=%v want billing", got["category"])
	}
	if got["status"] != "open" {
		t.Errorf("status lost: got %v want open", got["status"])
	}
}
