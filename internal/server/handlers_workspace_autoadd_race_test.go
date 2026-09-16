package server

import (
	"net/http"
	"testing"
)

// BUG-2792: the creator-connection auto-add must decide on the grant as it
// stands when the allow-list row is written, not as it stood at some earlier
// read. The race is constructed rather than sampled: autoAddPreInsertHook runs
// after the handler has resolved the OAuth identity and immediately before the
// allow-list write, and the revoking leg withdraws creation power there — the
// same store call PATCH /connected-apps/{id}/flags makes.
//
// Both legs assert that the hook actually RAN before asserting anything about
// the allow-list: a hook that never fires leaves "no row" true for the wrong
// reason and "row" true for the old one.
//
// These run on SQLite only (testServer is SQLite-backed), where the
// single-statement insert is atomic under the database write lock. The
// Postgres interleavings — where a statement snapshot can go stale without any
// lock — are pinned at the store layer in oauth_connections_autoadd_test.go.

func TestAutoAddCreatorConnection_RevokedMidFlight_NotAdded(t *testing.T) {
	e := newConsentEnv(t, true)

	ran := false
	e.srv.autoAddPreInsertHook = func(requestID, _ string) {
		ran = true
		if err := e.srv.store.SetScopeFlags(requestID, false, false, false); err != nil {
			t.Errorf("SetScopeFlags (revoke): %v", err)
		}
	}

	rr := e.createWorkspace("Revoked Mid Flight", e.requestID)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	if !ran {
		t.Fatal("autoAddPreInsertHook never ran — the leg measured nothing")
	}
	ws := e.mustExist(t, "Revoked Mid Flight")

	conn, err := e.srv.store.GetOAuthConnection(e.requestID)
	if err != nil {
		t.Fatalf("GetOAuthConnection: %v", err)
	}
	if conn.MayCreateWorkspaces {
		t.Fatal("may_create_workspaces is still true — the revocation did not land, so the leg measured nothing")
	}

	allowed, err := e.srv.store.IsConnectionWorkspaceAllowed(e.requestID, ws.ID)
	if err != nil {
		t.Fatalf("IsConnectionWorkspaceAllowed: %v", err)
	}
	if allowed {
		t.Errorf("workspace %s joined the allow-list of a connection whose creation power "+
			"was revoked before the write — the auto-add decided on a stale read", ws.Slug)
	}
}

// Control: the same seam, with the grant left as it was. Without this leg a
// fix that stopped auto-adding altogether would pass the revoking leg.
func TestAutoAddCreatorConnection_GrantUnchangedMidFlight_Added(t *testing.T) {
	e := newConsentEnv(t, true)

	ran := false
	e.srv.autoAddPreInsertHook = func(string, string) { ran = true }

	rr := e.createWorkspace("Kept Mid Flight", e.requestID)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	if !ran {
		t.Fatal("autoAddPreInsertHook never ran — the leg measured nothing")
	}
	ws := e.mustExist(t, "Kept Mid Flight")

	allowed, err := e.srv.store.IsConnectionWorkspaceAllowed(e.requestID, ws.ID)
	if err != nil {
		t.Fatalf("IsConnectionWorkspaceAllowed: %v", err)
	}
	if !allowed {
		t.Errorf("workspace %s is not on the allow-list of a connection that kept creation power", ws.Slug)
	}
}
