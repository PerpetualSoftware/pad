package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3975: a CLI token minted by approving from a web session gets its own
// lifetime (30-day slide, 90-day cap from the approval), not the approving
// session's remaining age.

func within(t *testing.T, what string, got, want time.Time, tol time.Duration) {
	t.Helper()
	if d := got.Sub(want); d > tol || d < -tol {
		t.Errorf("%s = %v, want %v +/- %v", what, got, want, tol)
	}
}

func sessionExpiry(t *testing.T, srv *Server, token string) time.Time {
	t.Helper()
	var s string
	sum := sha256.Sum256([]byte(token))
	if err := srv.store.DB().QueryRow(`SELECT expires_at FROM sessions WHERE token_hash = ?`, hex.EncodeToString(sum[:])).Scan(&s); err != nil {
		t.Fatal(err)
	}
	exp, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return exp
}

func TestTASK3975_CLIApprovalFromAnEightyFiveDayWebSession_GetsFullWindow(t *testing.T) {
	srv, u, token := oauthOnlyCloud(t)
	ageAllSessions(t, srv, u.ID, 85*24*time.Hour)
	pending, err := srv.store.CreateCLIAuthSession()
	if err != nil {
		t.Fatal(err)
	}
	if rr := doRequestWithCookieFrom(srv, "POST", "/api/v1/auth/cli/sessions/"+pending.Code+"/approve", nil, token, "198.51.100.7:5555"); rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
	approved, err := srv.store.GetCLIAuthSession(pending.Code)
	if err != nil || approved == nil || approved.Token == "" {
		t.Fatalf("approved CLI session: %+v %v", approved, err)
	}
	info, err := srv.store.ValidateSession(approved.Token)
	if err != nil || info == nil {
		t.Fatalf("validate: %v", err)
	}
	now := time.Now()
	within(t, "created_at", info.CreatedAt, now, time.Minute)
	within(t, "expires_at", sessionExpiry(t, srv, approved.Token), now.Add(30*24*time.Hour), time.Minute)

	// Sliding renewal reaches the full 30 days, not the approver's remaining 5.
	if _, err := srv.store.DB().Exec(`UPDATE sessions SET expires_at = ? WHERE user_id = ? AND kind = ?`,
		now.Add(10*24*time.Hour).UTC().Format(time.RFC3339), u.ID, store.SessionKindCLI); err != nil {
		t.Fatal(err)
	}
	newExp, renewed, err := srv.store.RenewSessionIfStale(approved.Token)
	if err != nil || !renewed {
		t.Fatalf("renew: renewed=%v err=%v", renewed, err)
	}
	within(t, "renewed expiry", newExp, now.Add(30*24*time.Hour), time.Minute)
}

// Dating the CLI token from the approval must not make it a fresh sign-in
// for the confirm-only delete recency check (BUG-3336).
func TestTASK3975_CLITokenFromOldSessionStillCannotConfirmDelete(t *testing.T) {
	srv, u, token := oauthOnlyCloud(t)
	ageAllSessions(t, srv, u.ID, 85*24*time.Hour)
	pending, _ := srv.store.CreateCLIAuthSession()
	if rr := doRequestWithCookieFrom(srv, "POST", "/api/v1/auth/cli/sessions/"+pending.Code+"/approve", nil, token, "198.51.100.7:5555"); rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
	approved, _ := srv.store.GetCLIAuthSession(pending.Code)
	rr := doRequestWithBearer(srv, "POST", "/api/v1/auth/delete-account", approved.Token, map[string]any{"confirm": true})
	if rr.Code != http.StatusForbidden || errorCode(t, rr) != "reauth_required" {
		t.Fatalf("confirm-only with a fresh CLI token: %d %s, want 403 reauth_required", rr.Code, rr.Body.String())
	}
	stillThere(t, srv, u.ID)
}

// A credential-change rotation REPLACES the session, so it keeps the
// replaced session's cap (it must not extend a stolen session's life).
func TestTASK3975_RotationKeepsParentCap(t *testing.T) {
	srv, u, _ := oauthOnlyCloud(t)
	// A 7-day web session, so the 90-day cap (5 days away) binds rather than
	// the session's own window, which a rotation keeps (BUG-3978).
	token, err := srv.store.CreateSession(u.ID, "web", "198.51.100.7", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	ageAllSessions(t, srv, u.ID, 85*24*time.Hour)
	req := httptest.NewRequest("POST", "/api/v1/auth/totp/enable", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	rotated, ok := srv.rotateSessionsAfterCredentialChange(httptest.NewRecorder(), req, u)
	if !ok {
		t.Fatal("rotation failed")
	}
	info, err := srv.store.ValidateSession(rotated)
	if err != nil || info == nil {
		t.Fatalf("rotated session: %v", err)
	}
	within(t, "rotated created_at", info.CreatedAt, time.Now().Add(-85*24*time.Hour), time.Minute)
	within(t, "rotated expiry", sessionExpiry(t, srv, rotated), time.Now().Add(5*24*time.Hour), time.Minute)
}

// TASK-3975 r2: only a browser session approves. A CLI token that could
// approve would mint its own successor with a fresh 90-day cap, and a chain
// of them would never expire. An 85-day-old CLI bearer is refused and mints
// nothing.
func TestTASK3975_CLIBearerCannotApprove(t *testing.T) {
	t.Parallel()
	srv, u, _ := oauthOnlyCloud(t)
	cli, err := srv.store.CreateSession(u.ID, "cli-browser-auth", "198.51.100.7", "", 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ageAllSessions(t, srv, u.ID, 85*24*time.Hour)
	countSessions := func() int {
		var n int
		if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, u.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countSessions()
	pending, err := srv.store.CreateCLIAuthSession()
	if err != nil {
		t.Fatal(err)
	}
	rr := doRequestWithBearer(srv, "POST", "/api/v1/auth/cli/sessions/"+pending.Code+"/approve", cli, nil)
	if rr.Code != http.StatusForbidden || errorCode(t, rr) != "session_required" {
		t.Fatalf("approve with a CLI bearer: %d %s, want 403 session_required", rr.Code, rr.Body.String())
	}
	if after := countSessions(); after != before {
		t.Fatalf("a refused approval left %d sessions, want %d", after, before)
	}
	got, err := srv.store.GetCLIAuthSession(pending.Code)
	if err != nil || got == nil || got.Status != "pending" || got.Token != "" {
		t.Fatalf("the CLI auth session after a refused approval: %+v %v, want pending with no token", got, err)
	}
}
