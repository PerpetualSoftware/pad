package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3447 (lead): during a real onboarding the agent creates COLLECTIONS
// minutes before its first item, so the launchpad's "Agent connected" step
// must tick on a collection an agent created. A collection now records its
// source the way an item does (from the request's auth shape, never the body),
// and has_agent_activity counts an agent-created collection the caller can see.
func TestDashboardHasAgentActivityFromAgentCollection(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	sessionToken := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	webWorkspace := func(t *testing.T, name string) string {
		t.Helper()
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": name}, sessionToken)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create web ws: %d %s", rr.Code, rr.Body.String())
		}
		var ws models.Workspace
		parseJSON(t, rr, &ws)
		return ws.Slug
	}
	hasAgent := func(t *testing.T, slug string) bool {
		t.Helper()
		rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+slug+"/dashboard", nil, sessionToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("dashboard: %d %s", rr.Code, rr.Body.String())
		}
		var resp DashboardResponse
		parseJSON(t, rr, &resp)
		return resp.HasAgentActivity
	}

	t.Run("a collection an agent created ticks it before any item", func(t *testing.T) {
		slug := webWorkspace(t, "Agent Collection WS")
		if hasAgent(t, slug) {
			t.Fatal("control: a web-created workspace with nothing in it is not agent-connected")
		}
		rr := doRequestWithHeaders(srv, "POST", "/api/v1/workspaces/"+slug+"/collections",
			map[string]string{"name": "Bugs"},
			map[string]string{"Authorization": "Bearer " + sessionToken})
		if rr.Code != http.StatusCreated {
			t.Fatalf("agent collection create: %d %s", rr.Code, rr.Body.String())
		}
		if !hasAgent(t, slug) {
			t.Fatal("an agent-created collection did not mark the workspace agent-connected")
		}
	})

	t.Run("a collection created in the web UI does not", func(t *testing.T) {
		slug := webWorkspace(t, "Web Collection WS")
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+slug+"/collections",
			map[string]string{"name": "Bugs"}, sessionToken)
		if rr.Code != http.StatusCreated {
			t.Fatalf("web collection create: %d %s", rr.Code, rr.Body.String())
		}
		if hasAgent(t, slug) {
			t.Fatal("a web-created collection marked the workspace agent-connected")
		}
	})

	t.Run("the body cannot claim an agent source", func(t *testing.T) {
		slug := webWorkspace(t, "Spoof Collection WS")
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+slug+"/collections",
			map[string]string{"name": "Bugs", "source": "cli"}, sessionToken)
		if rr.Code != http.StatusCreated {
			t.Fatalf("collection create: %d %s", rr.Code, rr.Body.String())
		}
		if hasAgent(t, slug) {
			t.Fatal("a body-supplied source marked the workspace agent-connected")
		}
	})
}
