package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// BUG-2639: the list endpoint splits a filter value containing a comma into an
// IN list, so a collection whose completed-work values include both "x" and
// "x,y" returns an item with status x for both per-value queries. The fake
// server answers every per-value query with that item, which is what the real
// one does for it; listCompletedWorkSince must return it once.
func TestListCompletedWorkSinceListsAnItemOnce(t *testing.T) {
	mux := http.NewServeMux()
	queries := 0
	mux.HandleFunc("/api/v1/workspaces/ws/collections", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id": "c1", "slug": "commas", "name": "Commas",
			"schema": `{"fields":[{"key":"status","type":"select","options":["open","x","x,y"],"terminal_options":["x","x,y"]}]}`,
		}})
	})
	mux.HandleFunc("/api/v1/workspaces/ws/collections/commas/items", func(w http.ResponseWriter, r *http.Request) {
		queries++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id": "i1", "slug": "in-two", "title": "In two groups", "fields": `{"status":"x"}`,
			"updated_at": time.Now().UTC().Format(time.RFC3339),
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	got, _ := listCompletedWorkSince(cli.NewClientFromURL(srv.URL), "ws", time.Now().Add(-time.Hour), 20)
	if queries != 2 {
		t.Fatalf("premise: expected one query per completed-work value (2), got %d", queries)
	}
	if len(got) != 1 {
		t.Errorf("item listed %d times, want once", len(got))
	}
}
