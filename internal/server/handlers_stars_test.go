package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestStarUnstar_ReturnsStructuredJSON pins BUG-1081's fix: pre-fix
// both endpoints returned 204 No Content with an empty body, which
// the MCP HTTPHandlerDispatcher surfaced as an empty tool result —
// agents had no signal whether the operation landed. Post-fix both
// return 200 OK with `{ref, starred}` so MCP clients see a non-empty
// response that confirms the toggle.
//
// Pin both directions of the toggle (star + unstar) so a future PR
// that re-introduces 204 (or drops the JSON branch) regresses the
// test rather than the user.
func TestStarUnstar_ReturnsStructuredJSON(t *testing.T) {
	srv := testServer(t)

	// Seed workspace + item via the no-users-exist path (no auth
	// required — the workspace creation handler accepts requests in
	// that mode and grants implicit owner). Star handlers DO require
	// a real user, so we mint one + an API token after seeding,
	// then use Bearer auth for the star calls (Bearer bypasses
	// CSRF — see middleware_csrf.go:73).
	slug := createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
		map[string]interface{}{"title": "Star me", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)
	if item.Ref == "" {
		t.Fatalf("seed item has no ref")
	}

	user, err := srv.store.CreateUser(models.UserCreate{
		Email:    "star-test@example.com",
		Name:     "Star Tester",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Tokens are workspace-scoped at the schema level — pull the
	// workspace ID out of the seeded slug.
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	tok, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
		Name:        "star-test",
		WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	starCall := func(method string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/star", nil)
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		req.RemoteAddr = "127.0.0.1:0"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// Star: expect 200 + {ref, starred:true}, NOT 204 with empty body.
	rr = starCall("POST")
	if rr.Code != http.StatusOK {
		t.Fatalf("star: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() == 0 {
		t.Fatal("star: body is empty — BUG-1081 regression (was 204 No Content pre-fix)")
	}
	var starResp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &starResp); err != nil {
		t.Fatalf("parse star response: %v (body: %s)", err, rr.Body.String())
	}
	if got := starResp["ref"]; got != item.Ref {
		t.Errorf("star: expected ref=%q, got %v", item.Ref, got)
	}
	if got := starResp["starred"]; got != true {
		t.Errorf("star: expected starred=true, got %v", got)
	}

	// Unstar: same shape, with starred:false.
	rr = starCall("DELETE")
	if rr.Code != http.StatusOK {
		t.Fatalf("unstar: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() == 0 {
		t.Fatal("unstar: body is empty — BUG-1081 regression")
	}
	var unstarResp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &unstarResp); err != nil {
		t.Fatalf("parse unstar response: %v (body: %s)", err, rr.Body.String())
	}
	if got := unstarResp["ref"]; got != item.Ref {
		t.Errorf("unstar: expected ref=%q, got %v", item.Ref, got)
	}
	if got := unstarResp["starred"]; got != false {
		t.Errorf("unstar: expected starred=false, got %v", got)
	}

	// Sanity — content-type should be JSON. If a future change drops
	// the writeJSON helper for a bare WriteHeader, this catches it.
	for _, op := range []string{"POST", "DELETE"} {
		rr = starCall(op)
		ct := rr.Header().Get("Content-Type")
		if !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s /star: expected application/json content-type, got %q", op, ct)
		}
	}

	// Quiet the unused import warning in case `bytes` ever drops.
	_ = bytes.NewReader
}
