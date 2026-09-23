package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// bug2640FakeServer answers the calls standup and changelog make: two
// collections (done field `stage` via board_group_by, and a status control),
// one completed item in each, an empty dashboard and no in-progress items.
func bug2640FakeServer(t *testing.T) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws/collections", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "c-stg", "slug": "stages", "name": "Stages",
				"schema":   `{"fields":[{"key":"stage","type":"select","options":["todo","complete"],"terminal_options":["complete"]}]}`,
				"settings": `{"board_group_by":"stage"}`},
			{"id": "c-tsk", "slug": "tasks", "name": "Tasks",
				"schema": `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"]}]}`},
		})
	})
	item := func(w http.ResponseWriter, id, coll, slug, prefix, fields string) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id": id, "collection_id": coll, "collection_slug": slug, "collection_prefix": prefix,
			"item_number": 1, "slug": id, "title": id, "fields": fields, "updated_at": now,
		}})
	}
	mux.HandleFunc("/api/v1/workspaces/ws/collections/stages/items", func(w http.ResponseWriter, r *http.Request) {
		item(w, "staged", "c-stg", "stages", "STG", `{"stage":"complete"}`)
	})
	mux.HandleFunc("/api/v1/workspaces/ws/collections/tasks/items", func(w http.ResponseWriter, r *http.Request) {
		item(w, "task", "c-tsk", "tasks", "TASK", `{"status":"done"}`)
	})
	mux.HandleFunc("/api/v1/workspaces/ws/dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/api/v1/workspaces/ws/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	setTempHomeMain(t)
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_testtoken")
	origWS, origFormat := workspaceFlag, formatFlag
	t.Cleanup(func() { workspaceFlag, formatFlag = origWS, origFormat })
	workspaceFlag, formatFlag = "ws", "json"
}

func bug2640AssertStatuses(t *testing.T, got map[string]string, out string) {
	t.Helper()
	if len(got) != 2 {
		t.Fatalf("premise: expected both completed items, got %v\n%s", got, out)
	}
	for ref, want := range map[string]string{"STG-1": "complete", "TASK-1": "done"} {
		if got[ref] != want {
			t.Errorf("%s status = %q, want %q (all %v)", ref, got[ref], want, got)
		}
	}
}

// BUG-2640, the CLI copy, driven through the real commands: a completed item
// from a collection whose done field is `stage` must render its stage value,
// not a blank read from the literal "status". The tasks item is the control.
func TestChangelogCommandShowsTheDoneFieldValue(t *testing.T) {
	bug2640FakeServer(t)
	cmd := changelogCmd()
	cmd.SetArgs([]string{})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("changelog: %v", err)
		}
	})
	var resp struct {
		Groups []struct {
			Items []struct {
				Ref    string `json:"ref"`
				Status string `json:"status"`
			} `json:"items"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, g := range resp.Groups {
		for _, it := range g.Items {
			got[it.Ref] = it.Status
		}
	}
	bug2640AssertStatuses(t, got, out)
}

func TestStandupCommandShowsTheDoneFieldValue(t *testing.T) {
	bug2640FakeServer(t)
	cmd := standupCmd()
	cmd.SetArgs([]string{})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("standup: %v", err)
		}
	})
	var resp struct {
		Completed []struct {
			Ref    string `json:"ref"`
			Status string `json:"status"`
		} `json:"completed"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, it := range resp.Completed {
		got[it.Ref] = it.Status
	}
	bug2640AssertStatuses(t, got, out)
}

// The CLI copy's fallback to "status" for a collection the map does not know.
func TestCompletedWorkValueFallsBackToStatus(t *testing.T) {
	item := models.Item{CollectionID: "unknown", Fields: `{"status":"done","stage":"complete"}`}
	if got := completedWorkValue(item, map[string]string{"other": "stage"}); got != "done" {
		t.Errorf("unmapped collection: got %q, want the status value", got)
	}
}
