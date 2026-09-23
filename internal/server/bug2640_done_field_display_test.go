package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2640: standup and changelog rendered a completed item's Status from the
// literal "status" field, so an item from a collection whose done field is
// something else (here `stage`, via board_group_by) was correctly INCLUDED and
// shown with a blank Status. The row must carry the value that closed it. The
// tasks item is the control: a status collection renders exactly as before.
func TestBUG2640_CompletedRowsShowTheDoneFieldValue(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug := createWSWithCollections(t, srv)
		if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]any{
			"name": "Stages", "slug": "stages", "prefix": "STG",
			"schema":   `{"fields":[{"key":"stage","type":"select","options":["todo","complete"],"terminal_options":["complete"],"default":"todo"}]}`,
			"settings": `{"board_group_by":"stage"}`,
		}); rr.Code != http.StatusCreated {
			t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
		}
		staged := createItem(t, srv, slug, "stages", map[string]any{"title": "Staged", "fields": `{"stage":"complete"}`})
		task := createItem(t, srv, slug, "tasks", map[string]any{"title": "Task", "fields": `{"status":"done"}`})
		want := map[string]string{staged.Ref: "complete", task.Ref: "done"}

		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/standup?days=1", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("standup: %d %s", rr.Code, rr.Body.String())
		}
		var su StandupResponse
		parseJSON(t, rr, &su)
		got := map[string]string{}
		for _, it := range su.Completed {
			got[it.Ref] = it.Status
		}
		for ref, v := range want {
			if got[ref] != v {
				t.Errorf("standup %s status = %q, want %q (all: %v)", ref, got[ref], v, got)
			}
		}

		rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/changelog", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("changelog: %d %s", rr.Code, rr.Body.String())
		}
		var cl ChangelogResponse
		parseJSON(t, rr, &cl)
		got = map[string]string{}
		for _, g := range cl.Groups {
			for _, it := range g.Items {
				got[it.Ref] = it.Status
			}
		}
		for ref, v := range want {
			if got[ref] != v {
				t.Errorf("changelog %s status = %q, want %q (all: %v)", ref, got[ref], v, got)
			}
		}
	})
}

// completedWorkValue falls back to "status" when the item's collection is not
// in the map (codex round 1 on BUG-2640 named this as an uncovered branch).
func TestBUG2640_CompletedWorkValueFallsBackToStatus(t *testing.T) {
	item := models.Item{CollectionID: "unknown", Fields: `{"status":"done","stage":"complete"}`}
	if got := completedWorkValue(item, map[string]string{"other": "stage"}); got != "done" {
		t.Errorf("unmapped collection: got %q, want the status value", got)
	}
	if got := completedWorkValue(models.Item{CollectionID: "c", Fields: item.Fields}, map[string]string{"c": "stage"}); got != "complete" {
		t.Errorf("mapped collection: got %q, want the stage value", got)
	}
}
