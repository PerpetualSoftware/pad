package server

import (
	"database/sql"
	"net/http"
	"testing"
	"time"
)

// BUG-4836: an approved CLI sign-in code must stop handing out its minted
// session token once expires_at has passed, and both expiry sweeps must revoke
// the minted session before deleting the row.

func approveCLIAuthForBUG4836(t *testing.T, srv *Server) (code, mintedToken string) {
	t.Helper()
	browserToken := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	code = createCLIAuthSessionFrom(t, srv, "pad-cli/1.0")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/cli/sessions/"+code+"/approve", nil, browserToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}

	if err := srv.store.DB().QueryRow(`SELECT token FROM cli_auth_sessions WHERE code = ?`, code).Scan(&mintedToken); err != nil || mintedToken == "" {
		t.Fatalf("read minted token: token=%q err=%v", mintedToken, err)
	}
	info, err := srv.store.ValidateSession(mintedToken)
	if err != nil || info == nil {
		t.Fatalf("minted session should validate before expiry: info=%+v err=%v", info, err)
	}
	return code, mintedToken
}

func backdateCLIAuthExpiry(t *testing.T, srv *Server, code string) {
	t.Helper()
	past := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(`UPDATE cli_auth_sessions SET expires_at = ? WHERE code = ?`, past, code); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}
}

func TestBUG4836_PollAfterExpiryOfApprovedCodeReturnsNoToken(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	code, mintedToken := approveCLIAuthForBUG4836(t, srv)
	backdateCLIAuthExpiry(t, srv, code)

	status, body := pollCLIAuth(t, srv, code)
	if status != http.StatusOK {
		t.Fatalf("poll status = %d, want 200 (%v)", status, body)
	}
	if body["status"] != "expired" {
		t.Fatalf("poll body status = %v, want expired (%v)", body["status"], body)
	}
	if _, hasToken := body["token"]; hasToken {
		t.Fatalf("poll body unexpectedly contains token key: %v", body)
	}

	info, err := srv.store.ValidateSession(mintedToken)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if info != nil {
		t.Fatalf("minted session still validates after expired poll: %+v", info)
	}

	var dbStatus string
	var dbToken sql.NullString
	if err := srv.store.DB().QueryRow(`SELECT status, token FROM cli_auth_sessions WHERE code = ?`, code).Scan(&dbStatus, &dbToken); err != nil {
		t.Fatalf("read cli_auth_sessions row: %v", err)
	}
	if dbStatus != "expired" || dbToken.Valid {
		t.Fatalf("cli_auth_sessions row = (status=%q, token=%+v), want (status=\"expired\", token=NULL)", dbStatus, dbToken)
	}
}

func TestBUG4836_PollInsideTTLStillReturnsToken(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	code, mintedToken := approveCLIAuthForBUG4836(t, srv)

	status, body := pollCLIAuth(t, srv, code)
	if status != http.StatusOK {
		t.Fatalf("poll status = %d, want 200 (%v)", status, body)
	}
	if body["status"] != "approved" {
		t.Fatalf("poll body status = %v, want approved (%v)", body["status"], body)
	}
	gotToken, ok := body["token"].(string)
	if !ok || gotToken == "" {
		t.Fatalf("poll body missing token: %v", body)
	}
	if gotToken != mintedToken {
		t.Fatalf("poll returned unexpected token: got %q, want %q", gotToken, mintedToken)
	}

	info, err := srv.store.ValidateSession(gotToken)
	if err != nil || info == nil {
		t.Fatalf("returned token does not validate: info=%+v err=%v", info, err)
	}
}

func TestBUG4836_ReaperRevokesUncollectedApprovedToken(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	code, mintedToken := approveCLIAuthForBUG4836(t, srv)
	backdateCLIAuthExpiry(t, srv, code)

	if err := srv.store.CleanExpiredCLIAuthSessions(); err != nil {
		t.Fatalf("CleanExpiredCLIAuthSessions: %v", err)
	}

	sess, err := srv.store.GetCLIAuthSession(code)
	if err != nil {
		t.Fatalf("GetCLIAuthSession: %v", err)
	}
	if sess != nil {
		t.Fatalf("expired row still exists after reaper: %+v", sess)
	}

	info, err := srv.store.ValidateSession(mintedToken)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if info != nil {
		t.Fatalf("minted session still validates after reaper: %+v", info)
	}
}

func TestBUG4836_CreateSweepRevokesUncollectedApprovedToken(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	code, mintedToken := approveCLIAuthForBUG4836(t, srv)
	backdateCLIAuthExpiry(t, srv, code)

	// Creating a new CLI auth session runs the opportunistic expiry sweep.
	_ = createCLIAuthSessionFrom(t, srv, "pad-cli/1.0")

	sess, err := srv.store.GetCLIAuthSession(code)
	if err != nil {
		t.Fatalf("GetCLIAuthSession: %v", err)
	}
	if sess != nil {
		t.Fatalf("expired row still exists after create sweep: %+v", sess)
	}

	info, err := srv.store.ValidateSession(mintedToken)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if info != nil {
		t.Fatalf("minted session still validates after create sweep: %+v", info)
	}
}
