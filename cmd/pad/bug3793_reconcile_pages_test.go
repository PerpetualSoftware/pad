package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3793 (CONVE-19): pad reconcile reads EVERY page of the workspace list.
// It used to ask for one 500-row page and reconcile only that, silently.
func TestBUG3793_ReconcileReadsEveryPage(t *testing.T) {
	const total, page = 5, 2
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		to := from + page
		if to < total {
			w.Header().Set("X-Pad-Has-More", "true")
			w.Header().Set("X-Pad-Next-Offset", strconv.Itoa(to))
		} else {
			to = total
			w.Header().Set("X-Pad-Has-More", "false")
		}
		var items []models.Item
		for i := from; i < to; i++ {
			items = append(items, models.Item{ID: fmt.Sprintf("it-%d", i), CodeContext: &models.ItemCodeContext{Provider: "github"}})
		}
		_ = json.NewEncoder(w).Encode(items)
	}))
	defer ts.Close()
	t.Setenv("HOME", t.TempDir())
	got, err := reconcileTargetItems(cli.NewClientFromURL(ts.URL), "ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != total {
		t.Fatalf("reconciled %d items, want all %d", len(got), total)
	}
}

// BUG-3793 (CONVE-19): pad github status scans every page of each collection.
func TestBUG3793_GitHubStatusScansEveryPage(t *testing.T) {
	const total, page = 5, 2
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/workspaces/ws/collections" {
			_ = json.NewEncoder(w).Encode([]models.Collection{{Slug: "tasks"}})
			return
		}
		from, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		to := from + page
		if to < total {
			w.Header().Set("X-Pad-Has-More", "true")
			w.Header().Set("X-Pad-Next-Offset", strconv.Itoa(to))
		} else {
			to = total
			w.Header().Set("X-Pad-Has-More", "false")
		}
		var items []models.Item
		for i := from; i < to; i++ {
			items = append(items, models.Item{ID: fmt.Sprintf("it-%d", i)})
		}
		_ = json.NewEncoder(w).Encode(items)
	}))
	defer ts.Close()
	t.Setenv("HOME", t.TempDir())
	got, err := githubStatusScanItems(cli.NewClientFromURL(ts.URL), "ws")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != total {
		t.Fatalf("scanned %d items, want all %d", len(got), total)
	}
}

// BUG-3793 (Codex r1): the completed-work report (changelog, standup) reads
// newest-first pages until one reaches past the cutoff. It read one page as
// every match, so a status with more matches than the page lost the older
// ones; and it must not keep reading once a page is past the cutoff.
func TestBUG3793_CompletedWorkPagesToTheCutoff(t *testing.T) {
	now := time.Now()
	cutoff := now.Add(-10 * time.Hour)
	// Seven done items, newest first: five after the cutoff, two before.
	updated := []time.Time{}
	for i := 0; i < 7; i++ {
		updated = append(updated, now.Add(-time.Duration(2*i+1)*time.Hour))
	}
	var offsets []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws/collections", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id": "c1", "slug": "tasks", "name": "Tasks",
			"schema": `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"]}]}`,
		}})
	})
	mux.HandleFunc("/api/v1/workspaces/ws/collections/tasks/items", func(w http.ResponseWriter, r *http.Request) {
		offsets = append(offsets, r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		from, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		to := min(from+limit, len(updated))
		if to < len(updated) {
			w.Header().Set("X-Pad-Has-More", "true")
			w.Header().Set("X-Pad-Next-Offset", strconv.Itoa(to))
		} else {
			w.Header().Set("X-Pad-Has-More", "false")
		}
		var items []map[string]any
		for i := from; i < to; i++ {
			items = append(items, map[string]any{
				"id": fmt.Sprintf("i%d", i), "collection_id": "c1", "fields": `{"status":"done"}`,
				"updated_at": updated[i].UTC().Format(time.RFC3339),
			})
		}
		_ = json.NewEncoder(w).Encode(items)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	got, _ := listCompletedWorkSince(cli.NewClientFromURL(srv.URL), "ws", cutoff, 2)
	if len(got) != 5 {
		t.Fatalf("got %d items after the cutoff, want 5", len(got))
	}
	// Pages of 2: [0,1] [2,3] [4,5]; row 5 is past the cutoff, so no page 4.
	if fmt.Sprint(offsets) != "[ 2 4]" {
		t.Fatalf("offsets asked %q, want [\"\" 2 4]", offsets)
	}
}
