package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3069: a workspace that does not resolve for the caller and a missing
// sub-resource inside a readable workspace both answer 404 not_found, and
// they demand opposite client reactions (stop asking about the workspace vs
// one item is gone). The workspace refusal now carries
// details.scope="workspace"; status, code and message are unchanged so every
// consumer keyed on `not_found` sees what it always saw.

type errEnvelope struct {
	Error struct {
		Code    string                 `json:"code"`
		Message string                 `json:"message"`
		Details map[string]interface{} `json:"details"`
	} `json:"error"`
}

func decodeErrEnvelope(t *testing.T, rr *httptest.ResponseRecorder) errEnvelope {
	t.Helper()
	var env errEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, rr.Body.String())
	}
	return env
}

// assertWorkspaceNotFound checks the full workspace-refusal contract: the
// unchanged status/code/message AND the marker.
func assertWorkspaceNotFound(t *testing.T, rr *httptest.ResponseRecorder, wantMessage string) {
	t.Helper()
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	env := decodeErrEnvelope(t, rr)
	if env.Error.Code != "not_found" {
		t.Errorf("code = %q, want not_found (the code is deliberately unchanged)", env.Error.Code)
	}
	if env.Error.Message != wantMessage {
		t.Errorf("message = %q, want %q (the message is deliberately unchanged)", env.Error.Message, wantMessage)
	}
	if got := env.Error.Details["scope"]; got != "workspace" {
		t.Errorf("details.scope = %v, want \"workspace\"; body=%s", got, rr.Body.String())
	}
}

// assertUnmarkedNotFound is the control: a 404 about something INSIDE a
// readable workspace must not carry the marker, or a client would purge a
// workspace it can read.
func assertUnmarkedNotFound(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	env := decodeErrEnvelope(t, rr)
	if _, ok := env.Error.Details["scope"]; ok {
		t.Errorf("a sub-resource 404 carries details.scope; body=%s", rr.Body.String())
	}
}

func TestBUG3069_WorkspaceRefusalIsMarkedThroughTheRouter(t *testing.T) {
	srv := testServer(t)
	owner := mustCreateUser(t, srv, "owner-3069@example.com", "Owner", "admin")
	outsider := mustCreateUser(t, srv, "outsider-3069@example.com", "Outsider", "member")
	ws := mustCreateOwnedWorkspace(t, srv, "Marked", owner)
	if err := srv.store.SeedCollectionsFromTemplate(ws.ID, ""); err != nil {
		t.Fatalf("seed collections: %v", err)
	}

	session := func(u *models.User) string {
		t.Helper()
		tok, err := srv.store.CreateSession(u.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return tok
	}
	ownerTok, outsiderTok := session(owner), session(outsider)

	// An outsider's reads, at every depth, are refused by
	// RequireWorkspaceAccess and must carry the marker — including the
	// item-by-ref depth the web client's path rule can never classify.
	// (Addressed by UUID the same outsider gets 403 forbidden instead:
	// resolveWorkspace resolves a UUID globally and the membership gate
	// refuses it. That is a different status the client already handles
	// on its own path, and it is out of this marker's scope.)
	for _, path := range []string{
		"/api/v1/workspaces/" + ws.Slug,
		"/api/v1/workspaces/" + ws.Slug + "/collections",
		"/api/v1/workspaces/" + ws.Slug + "/items/TASK-1",
	} {
		t.Run("outsider "+path, func(t *testing.T) {
			assertWorkspaceNotFound(t, doRequestWithCookie(srv, http.MethodGet, path, nil, outsiderTok), "Workspace not found")
		})
	}

	t.Run("a workspace that does not exist at all", func(t *testing.T) {
		assertWorkspaceNotFound(t, doRequestWithCookie(srv, http.MethodGet, "/api/v1/workspaces/no-such-ws-3069/items", nil, ownerTok), "Workspace not found")
	})

	// CONTROLS: the same member, the same workspace, a missing THING inside
	// it. Without these, a marker written on every 404 would pass the legs
	// above.
	t.Run("member, missing item", func(t *testing.T) {
		assertUnmarkedNotFound(t, doRequestWithCookie(srv, http.MethodGet, "/api/v1/workspaces/"+ws.Slug+"/items/TASK-999999", nil, ownerTok))
	})
	t.Run("member, missing collection", func(t *testing.T) {
		assertUnmarkedNotFound(t, doRequestWithCookie(srv, http.MethodGet, "/api/v1/workspaces/"+ws.Slug+"/collections/no-such-collection", nil, ownerTok))
	})
}

func TestBUG3069_SSEWorkspaceRefusalIsMarked(t *testing.T) {
	srv := testServerWithEvents(t)
	owner := mustCreateUser(t, srv, "owner-sse-3069@example.com", "Owner", "admin")
	outsider := mustCreateUser(t, srv, "outsider-sse-3069@example.com", "Outsider", "member")
	ws := mustCreateOwnedWorkspace(t, srv, "SSE Marked", owner)
	tok, err := srv.store.CreateSession(outsider.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Both entry branches of handleSSE: the slug form fails resolution, the
	// UUID form resolves globally and fails the membership gate.
	for _, param := range []string{ws.Slug, ws.ID} {
		t.Run(param, func(t *testing.T) {
			rr := doRequestWithCookie(srv, http.MethodGet, "/api/v1/events?workspace="+param, nil, tok)
			assertWorkspaceNotFound(t, rr, "Workspace not found")
		})
	}
}

func TestBUG3069_CollabWorkspaceRefusalCarriesTheMarker(t *testing.T) {
	// authorizeCollabAccess's vanished-workspace branch returns a typed
	// error that handleCollab writes; the item's workspace id names nothing.
	// A direct call, because an item whose workspace row is gone cannot be
	// reached through the item lookup that precedes it on the route.
	srv := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/collab/x", nil)
	_, err := srv.authorizeCollabAccess(req, &models.Item{WorkspaceID: "00000000-0000-0000-0000-000000003069"})
	sErr, ok := err.(*statusError)
	if !ok {
		t.Fatalf("err = %T %v, want *statusError", err, err)
	}
	if sErr.code != http.StatusNotFound || sErr.kind != "not_found" || sErr.message != "Workspace not found" {
		t.Errorf("statusError = %d %q %q, want 404 not_found \"Workspace not found\"", sErr.code, sErr.kind, sErr.message)
	}
	if sErr.details["scope"] != "workspace" {
		t.Errorf("details = %v, want scope=workspace", sErr.details)
	}
}

func TestBUG3069_GuestHiddenItemIsNotMarked(t *testing.T) {
	// A guest (item grant, no membership) RESOLVES the workspace, so an item
	// outside their grant is an item-level 404 and must not carry the
	// marker: a client that purged on it would lock the guest out of the
	// item they were granted.
	srv := testServer(t)
	owner := mustCreateUser(t, srv, "owner-guest-3069@example.com", "Owner", "admin")
	guest := mustCreateUser(t, srv, "guest-3069@example.com", "Guest", "member")
	ws := mustCreateOwnedWorkspace(t, srv, "Guest Marked", owner)
	if err := srv.store.SeedCollectionsFromTemplate(ws.ID, ""); err != nil {
		t.Fatalf("seed collections: %v", err)
	}
	tasks, err := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || tasks == nil {
		t.Fatalf("get tasks collection: %v", err)
	}
	granted, err := srv.store.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: "Granted", Fields: `{}`})
	if err != nil {
		t.Fatalf("CreateItem granted: %v", err)
	}
	hidden, err := srv.store.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: "Hidden", Fields: `{}`})
	if err != nil {
		t.Fatalf("CreateItem hidden: %v", err)
	}
	if _, err := srv.store.CreateItemGrant(ws.ID, granted.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	tok, err := srv.store.CreateSession(guest.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Precondition: the grant works, so the workspace resolves for this guest.
	if rr := doRequestWithCookie(srv, http.MethodGet, "/api/v1/workspaces/"+ws.Slug+"/items/"+granted.Slug, nil, tok); rr.Code != http.StatusOK {
		t.Fatalf("granted item: status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	assertUnmarkedNotFound(t, doRequestWithCookie(srv, http.MethodGet, "/api/v1/workspaces/"+ws.Slug+"/items/"+hidden.Slug, nil, tok))
}
