package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

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
