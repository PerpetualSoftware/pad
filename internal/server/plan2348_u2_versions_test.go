package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-2348 U2 through the routes: a body PATCH's version row names its user,
// GET /versions/{id}/diff answers that row's own change, and a throttled body
// edit's marker never renders as an empty card.

func listVersions(t *testing.T, srv *Server, token, ws, slug string) []models.Version {
	t.Helper()
	rr := authedAgentRequest(t, srv, token, "", "GET", "/api/v1/workspaces/"+ws+"/items/"+slug+"/versions", nil)
	var vs []models.Version
	decodeAttributionBody(t, rr, &vs)
	return vs
}

func TestItemVersionRoutes_WriterAndDiff(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, slug := debounceFixture(t, srv)
	var me struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	decodeAttributionBody(t, authedAgentRequest(t, srv, token, "", "GET", "/api/v1/auth/me", nil), &me)

	authedAgentRequest(t, srv, token, "", "PATCH", "/api/v1/workspaces/"+ws+"/items/"+slug, map[string]any{"content": "first\n"})
	authedAgentRequest(t, srv, token, "wren", "PATCH", "/api/v1/workspaces/"+ws+"/items/"+slug, map[string]any{"content": "second\nline\n"})

	vs := listVersions(t, srv, token, ws, slug)
	if len(vs) == 0 {
		t.Fatal("precondition: the body edits wrote no version row")
	}
	newest := vs[0]
	if newest.UserID != me.ID || newest.ActorName != me.Name {
		t.Fatalf("newest row: user %q name %q, want %q %q", newest.UserID, newest.ActorName, me.ID, me.Name)
	}
	if newest.LinesAdded == nil || newest.LinesRemoved == nil {
		t.Fatalf("newest row carries no line counts: %+v", newest)
	}

	rr := authedAgentRequest(t, srv, token, "", "GET", "/api/v1/workspaces/"+ws+"/items/"+slug+"/versions/"+newest.ID+"/diff", nil)
	var d models.ItemVersionDiff
	decodeAttributionBody(t, rr, &d)
	if d.Before != "first\n" || d.After != "second\nline\n" {
		t.Fatalf("diff %q → %q, want the newest row's own change first → second", d.Before, d.After)
	}

	missing := doRequestWithBearer(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+slug+"/versions/nope/diff", token, nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("CONTROL: an unknown version answered %d, want 404", missing.Code)
	}
}

// The marker is the only record of a throttled body edit, so the timeline
// must serve it (PLAN-2348 U3 flipped the U2 rule that hid it).
func TestThrottledBodyEdit_MarkerReachesTimeline(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, slug := debounceFixture(t, srv)
	var item models.Item
	decodeAttributionBody(t, authedAgentRequest(t, srv, token, "wren", "PATCH", "/api/v1/workspaces/"+ws+"/items/"+slug,
		map[string]any{"content": "one\n"}), &item)
	authedAgentRequest(t, srv, token, "wren", "PATCH", "/api/v1/workspaces/"+ws+"/items/"+slug, map[string]any{"content": "two\n"})

	acts, err := srv.store.ListDocumentActivity(item.ID, models.ActivityListParams{Action: "updated"})
	if err != nil {
		t.Fatalf("list activity: %v", err)
	}
	marked := false
	for _, a := range acts {
		var m map[string]string
		_ = json.Unmarshal([]byte(a.Metadata), &m)
		if m["body_edited"] == "true" {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("precondition: the throttled second edit wrote no body_edited marker: %+v", acts)
	}
	served := false
	for _, e := range updatedActivityEntries(fetchTimelineAuthed(t, srv, token, ws, slug).Entries) {
		var m map[string]string
		_ = json.Unmarshal([]byte(e.Activity.Metadata), &m)
		if m["body_edited"] == "true" {
			served = true
		}
	}
	if !served {
		t.Fatalf("the timeline dropped the body_edited row, the only record of the throttled edit")
	}
}
