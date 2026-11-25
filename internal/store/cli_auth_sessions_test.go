package store

import (
	"database/sql"
	"testing"
	"time"
)

func TestCLIAuthSessionExpiredApprovalRevokesMintedSession(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := createTestUser(t, s, "cli-auth@test.com", "CLI User", "password123")

	cliSess, err := s.CreateCLIAuthSession()
	if err != nil {
		t.Fatalf("CreateCLIAuthSession: %v", err)
	}
	mintedToken, err := s.CreateSession(u.ID, "cli-browser-auth", "127.0.0.1", "", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.ApproveCLIAuthSession(cliSess.Code, mintedToken, u.ID); err != nil {
		t.Fatalf("ApproveCLIAuthSession: %v", err)
	}

	past := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	if _, err := s.db.Exec(s.q(`UPDATE cli_auth_sessions SET expires_at = ? WHERE code = ?`), past, cliSess.Code); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}

	got, err := s.GetCLIAuthSession(cliSess.Code)
	if err != nil {
		t.Fatalf("GetCLIAuthSession: %v", err)
	}
	if got == nil {
		t.Fatal("GetCLIAuthSession returned nil, want expired session")
	}
	if got.Status != "expired" || got.Token != "" {
		t.Fatalf("GetCLIAuthSession = (status=%q, token=%q), want (expired, empty)", got.Status, got.Token)
	}

	info, err := s.ValidateSession(mintedToken)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if info != nil {
		t.Fatalf("minted session still validates after expired GetCLIAuthSession: %+v", info)
	}

	// Second read sees the already-expired row with empty token.
	got2, err := s.GetCLIAuthSession(cliSess.Code)
	if err != nil || got2 == nil || got2.Status != "expired" || got2.Token != "" {
		t.Fatalf("second GetCLIAuthSession = (%+v, %v), want expired with empty token", got2, err)
	}
}

func TestCLIAuthSessionCleanExpiredRevokesApprovedTokens(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := createTestUser(t, s, "cli-reap@test.com", "CLI Reaper", "password123")

	expiredSess, err := s.CreateCLIAuthSession()
	if err != nil {
		t.Fatalf("CreateCLIAuthSession (expired): %v", err)
	}
	expiredTok, err := s.CreateSession(u.ID, "cli-browser-auth", "127.0.0.1", "", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession (expired): %v", err)
	}
	if err := s.ApproveCLIAuthSession(expiredSess.Code, expiredTok, u.ID); err != nil {
		t.Fatalf("ApproveCLIAuthSession (expired): %v", err)
	}

	activeSess, err := s.CreateCLIAuthSession()
	if err != nil {
		t.Fatalf("CreateCLIAuthSession (active): %v", err)
	}
	activeTok, err := s.CreateSession(u.ID, "cli-browser-auth", "127.0.0.1", "", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession (active): %v", err)
	}
	if err := s.ApproveCLIAuthSession(activeSess.Code, activeTok, u.ID); err != nil {
		t.Fatalf("ApproveCLIAuthSession (active): %v", err)
	}

	past := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	if _, err := s.db.Exec(s.q(`UPDATE cli_auth_sessions SET expires_at = ? WHERE code = ?`), past, expiredSess.Code); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}

	if err := s.CleanExpiredCLIAuthSessions(); err != nil {
		t.Fatalf("CleanExpiredCLIAuthSessions: %v", err)
	}

	if got, err := s.GetCLIAuthSession(expiredSess.Code); err != nil || got != nil {
		t.Fatalf("expired row after clean = (%+v, %v), want nil", got, err)
	}
	if info, err := s.ValidateSession(expiredTok); err != nil || info != nil {
		t.Fatalf("expired minted session still validates: (%+v, %v)", info, err)
	}

	// Unexpired approved row and its minted session remain intact.
	if got, err := s.GetCLIAuthSession(activeSess.Code); err != nil || got == nil || got.Status != "approved" || got.Token != activeTok {
		t.Fatalf("active row after clean = (%+v, %v), want approved with token", got, err)
	}
	if info, err := s.ValidateSession(activeTok); err != nil || info == nil {
		t.Fatalf("active minted session should still validate: (%+v, %v)", info, err)
	}
}

func TestCLIAuthSessionCreateSweepRevokesApprovedTokens(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := createTestUser(t, s, "cli-sweep@test.com", "CLI Sweep", "password123")

	expiredSess, err := s.CreateCLIAuthSession()
	if err != nil {
		t.Fatalf("CreateCLIAuthSession: %v", err)
	}
	expiredTok, err := s.CreateSession(u.ID, "cli-browser-auth", "127.0.0.1", "", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.ApproveCLIAuthSession(expiredSess.Code, expiredTok, u.ID); err != nil {
		t.Fatalf("ApproveCLIAuthSession: %v", err)
	}

	past := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	if _, err := s.db.Exec(s.q(`UPDATE cli_auth_sessions SET expires_at = ? WHERE code = ?`), past, expiredSess.Code); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}

	if _, err := s.CreateCLIAuthSession(); err != nil {
		t.Fatalf("CreateCLIAuthSession (sweep trigger): %v", err)
	}

	if got, err := s.GetCLIAuthSession(expiredSess.Code); err != nil || got != nil {
		t.Fatalf("expired row after create sweep = (%+v, %v), want nil", got, err)
	}
	if info, err := s.ValidateSession(expiredTok); err != nil || info != nil {
		t.Fatalf("expired minted session still validates after create sweep: (%+v, %v)", info, err)
	}
}

func TestCLIAuthSessionCollectTokenOnceInsideTTL(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := createTestUser(t, s, "cli-collect@test.com", "CLI Collector", "password123")

	cliSess, err := s.CreateCLIAuthSession()
	if err != nil {
		t.Fatalf("CreateCLIAuthSession: %v", err)
	}
	mintedToken, err := s.CreateSession(u.ID, "cli-browser-auth", "127.0.0.1", "", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.ApproveCLIAuthSession(cliSess.Code, mintedToken, u.ID); err != nil {
		t.Fatalf("ApproveCLIAuthSession: %v", err)
	}

	if ok, err := s.CollectCLIAuthToken(cliSess.Code, "not-the-token"); err != nil || ok {
		t.Fatalf("collect with a stale token = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := s.CollectCLIAuthToken(cliSess.Code, mintedToken); err != nil || !ok {
		t.Fatalf("first collect = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := s.CollectCLIAuthToken(cliSess.Code, mintedToken); err != nil || ok {
		t.Fatalf("second collect = (%v, %v), want (false, nil)", ok, err)
	}
	if info, err := s.ValidateSession(mintedToken); err != nil || info == nil {
		t.Fatalf("collected session should stay valid: info=%+v err=%v", info, err)
	}
}

func TestCLIAuthSessionCollectTokenRefusedAfterExpiry(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := createTestUser(t, s, "cli-collect-late@test.com", "CLI Late", "password123")

	cliSess, err := s.CreateCLIAuthSession()
	if err != nil {
		t.Fatalf("CreateCLIAuthSession: %v", err)
	}
	mintedToken, err := s.CreateSession(u.ID, "cli-browser-auth", "127.0.0.1", "", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.ApproveCLIAuthSession(cliSess.Code, mintedToken, u.ID); err != nil {
		t.Fatalf("ApproveCLIAuthSession: %v", err)
	}
	past := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	if _, err := s.db.Exec(s.q(`UPDATE cli_auth_sessions SET expires_at = ? WHERE code = ?`), past, cliSess.Code); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}

	if ok, err := s.CollectCLIAuthToken(cliSess.Code, mintedToken); err != nil || ok {
		t.Fatalf("collect after expiry = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestCLIAuthSessionApproveRefusesRowExpiredSinceRead(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := createTestUser(t, s, "cli-late-approve@test.com", "CLI Late Approver", "password123")

	cliSess, err := s.CreateCLIAuthSession()
	if err != nil {
		t.Fatalf("CreateCLIAuthSession: %v", err)
	}
	// An expired code must never end up holding a token. The read inside
	// Approve refuses it first; the write carries the same expiry filter for
	// the gap between that read and the write, which no test here can open.
	past := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	if _, err := s.db.Exec(s.q(`UPDATE cli_auth_sessions SET expires_at = ? WHERE code = ?`), past, cliSess.Code); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}
	mintedToken, err := s.CreateSession(u.ID, "cli-browser-auth", "127.0.0.1", "", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.ApproveCLIAuthSession(cliSess.Code, mintedToken, u.ID); err == nil {
		t.Fatal("approving an expired code succeeded, want an error")
	}
	var token sql.NullString
	if err := s.db.QueryRow(s.q(`SELECT token FROM cli_auth_sessions WHERE code = ?`), cliSess.Code).Scan(&token); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if token.Valid && token.String != "" {
		t.Fatalf("expired row was given a token")
	}
}
