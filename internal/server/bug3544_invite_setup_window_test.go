package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// BUG-3544: before the first account exists (the setup window, where requests
// run without auth), `pad workspace invite` reached CreateInvitation with no
// inviter, failed the inviter foreign key, and answered 500. It now answers
// the setup window's own refusal, 409 setup_required, and writes nothing.
func TestInviteInTheSetupWindowAnswersSetupRequired(t *testing.T) {
	srv := testServer(t)
	if n, err := srv.store.UserCount(); err != nil || n != 0 {
		t.Fatalf("premise: a fresh install has no users; count=%d err=%v", n, err)
	}
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("workspace: %v", err)
	}

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/members/invite",
		map[string]any{"email": "alex@example.com", "role": "editor"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409 (was a 500 on the inviter FK). Body: %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "setup_required" {
		t.Errorf("code %q, want setup_required", env.Error.Code)
	}
	invs, err := srv.store.ListWorkspaceInvitations(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 0 {
		t.Errorf("an invitation was written: %+v", invs)
	}
}
