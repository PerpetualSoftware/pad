package store

import (
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3678: once a grant is revoked, no refresh mints a usable token, and two
// concurrent refreshes of one refresh token yield at most one success.
//
// Fosite's refresh runs RotateRefreshToken, CreateAccessTokenSession and
// CreateRefreshTokenSession as separate calls after validating the presented
// token, so a revoke can land before any of them.

type bug3678Grant struct {
	s       *Store
	user    string
	client  string
	request string
}

func bug3678Fixture(t *testing.T) bug3678Grant {
	t.Helper()
	s := testStore(t)
	c := newTestClient(t, s)
	g := bug3678Grant{s: s, user: "user-123", client: c.ID, request: "req-3678"}
	if err := s.CreateAccessToken(g.req("a1")); err != nil {
		t.Fatal(err)
	}
	r1 := g.req("r1")
	r1.AccessTokenSignature = "a1"
	if err := s.CreateRefreshToken(r1); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g bug3678Grant) req(sig string) models.OAuthRequest {
	return newTestRequest(g.client, sig, g.request)
}

// refresh replays fosite's storage calls for one refresh of r1, minting the
// given signatures, and reports the first error.
func (g bug3678Grant) refresh(access, refresh string) error {
	if err := g.s.RotateRefreshToken(g.request, "r1"); err != nil {
		return err
	}
	if err := g.s.CreateAccessToken(g.req(access)); err != nil {
		return err
	}
	r := g.req(refresh)
	r.AccessTokenSignature = access
	return g.s.CreateRefreshToken(r)
}

func (g bug3678Grant) activeTokens(t *testing.T) int {
	t.Helper()
	var n int
	if err := g.s.db.QueryRow(g.s.q(`
		SELECT (SELECT COUNT(*) FROM oauth_access_tokens WHERE request_id = ? AND active = ?)
		     + (SELECT COUNT(*) FROM oauth_refresh_tokens WHERE request_id = ? AND active = ?)
	`), g.request, true, g.request, true).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBUG3678_RevokeBeforeRotateMintsNothing(t *testing.T) {
	g := bug3678Fixture(t)
	if err := g.s.RevokeUserOAuthConnection(g.user, g.request); err != nil {
		t.Fatal(err)
	}
	if err := g.refresh("a2", "r2"); err == nil {
		t.Error("a refresh validated before the revoke completed after it")
	}
	if n := g.activeTokens(t); n != 0 {
		t.Fatalf("%d active tokens on a revoked grant, want 0", n)
	}
}

// The revoke lands after the rotation and before the new pair is written:
// only an issuance-time check can refuse it.
func TestBUG3678_RevokeAfterRotateMintsNothing(t *testing.T) {
	g := bug3678Fixture(t)
	if err := g.s.RotateRefreshToken(g.request, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := g.s.RevokeUserOAuthConnection(g.user, g.request); err != nil {
		t.Fatal(err)
	}
	if err := g.s.CreateAccessToken(g.req("a2")); err == nil {
		t.Error("an access token was minted on a revoked grant")
	}
	r2 := g.req("r2")
	r2.AccessTokenSignature = "a2"
	if err := g.s.CreateRefreshToken(r2); err == nil {
		t.Error("a refresh token was minted on a revoked grant")
	}
	if n := g.activeTokens(t); n != 0 {
		t.Fatalf("%d active tokens on a revoked grant, want 0", n)
	}
}

// A claim ends every grant its account held, and a refresh in flight must
// not bring one back.
func TestBUG3678_ClaimEndsGrantsForGood(t *testing.T) {
	g := bug3678Fixture(t)
	u := createUnverifiedUser(t, g.s, "claim-3678@example.com")
	g.user = u.ID
	g.request = "req-3678-claim"
	mk := func(sig string) models.OAuthRequest {
		r := g.req(sig)
		r.Subject = u.ID
		r.SessionData = `{"subject":"` + u.ID + `"}`
		return r
	}
	if err := g.s.CreateRefreshToken(mk("cr1")); err != nil {
		t.Fatal(err)
	}
	vTok, err := g.s.CreateEmailVerification(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.ClaimAccountByVerification(vTok); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := g.s.CreateAccessToken(mk("ca2")); err == nil {
		t.Error("a pre-claim grant minted an access token after the claim")
	}
	if n := g.activeTokens(t); n != 0 {
		t.Fatalf("%d active tokens on a claimed account's old grant, want 0", n)
	}
	// The claimant's own new grants are unaffected.
	fresh := mk("ca3")
	fresh.RequestID = "req-3678-after-claim"
	if err := g.s.CreateAccessToken(fresh); err != nil {
		t.Fatalf("a new grant after the claim: %v", err)
	}
}

func TestBUG3678_SecondRotationOfOneTokenFails(t *testing.T) {
	g := bug3678Fixture(t)
	if err := g.refresh("a2", "r2"); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if err := g.refresh("a3", "r3"); err == nil {
		t.Error("the same refresh token rotated twice")
	}
	if n := g.activeTokens(t); n != 2 {
		t.Fatalf("%d active tokens, want 2 (one pair)", n)
	}
}

// The replacement refresh token rotates in its turn: spending the presented
// token conditionally does not strand the pair it minted.
func TestBUG3678_ReplacementTokenRotates(t *testing.T) {
	g := bug3678Fixture(t)
	if err := g.refresh("a2", "r2"); err != nil {
		t.Fatal(err)
	}
	if err := g.s.RotateRefreshToken(g.request, "r2"); err != nil {
		t.Fatalf("rotating the replacement: %v", err)
	}
}

func TestBUG3678_ConcurrentRefreshesYieldOnePair_Postgres(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("Postgres-only: concurrent writers")
	}
	for i := 0; i < 20; i++ {
		g := bug3678Fixture(t)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for k := 0; k < 2; k++ {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				suffix := string(rune('a' + k))
				errs[k] = g.refresh("a2"+suffix, "r2"+suffix)
			}(k)
		}
		wg.Wait()
		if errs[0] == nil && errs[1] == nil {
			t.Fatalf("round %d: both concurrent refreshes succeeded", i)
		}
		if n := g.activeTokens(t); n > 2 {
			t.Fatalf("round %d: %d active tokens, want at most one pair", i, n)
		}
	}
}

func TestBUG3678_RevokeRacingRefreshLeavesNothingActive_Postgres(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("Postgres-only: concurrent writers")
	}
	for i := 0; i < 20; i++ {
		g := bug3678Fixture(t)
		var wg sync.WaitGroup
		var revokeErr error
		wg.Add(2)
		go func() { defer wg.Done(); _ = g.refresh("a2", "r2") }()
		go func() { defer wg.Done(); revokeErr = g.s.RevokeUserOAuthConnection(g.user, g.request) }()
		wg.Wait()
		if revokeErr != nil {
			t.Fatalf("round %d: revoke: %v", i, revokeErr)
		}
		if n := g.activeTokens(t); n != 0 {
			t.Fatalf("round %d: %d active tokens after a revoke, want 0", i, n)
		}
	}
}

// An installed app's grant is minted through the install barrier, not the
// ordinary insert: a revoke through fosite (RFC 7009 or a refresh replay)
// ends it the way RevokeUserAppGrant does, with the binding tombstoned
// (codex rounds 1 and 2 on BUG-3678).
func TestBUG3678_RevokedInstallGrantMintsNothing(t *testing.T) {
	f := task3399Fixture(t, "inst-3678", "write")
	task3399Delegated(t, f, "req-3678-app")
	if err := f.s.RevokeOAuthGrant("req-3678-app"); err != nil {
		t.Fatal(err)
	}
	r := task3399Req(f.clientID, f.person.ID, "req-3678-app", "read", 1)
	if err := f.s.CreateRefreshToken(r); err == nil {
		t.Fatal("a revoked installed-app grant minted a refresh token")
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM oauth_refresh_tokens WHERE signature = ?`, r.Signature); n != 0 {
		t.Error("the refused refresh token was stored")
	}
	var revokedAt sql.NullString
	if err := f.s.db.QueryRow(f.s.q(`SELECT revoked_at FROM app_token_bindings WHERE request_id = ?`), "req-3678-app").Scan(&revokedAt); err != nil || !revokedAt.Valid {
		t.Errorf("the binding was not tombstoned: %v %v", revokedAt, err)
	}
}

// A grant whose consent is recorded but whose first tokens are not yet
// written is still revoked for good: its connection row names the person
// to lock, and the record refuses the first issuance (codex round 3).
func TestBUG3678_GrantRevokedBeforeFirstIssuanceMintsNothing(t *testing.T) {
	s := testStore(t)
	c := newTestClient(t, s)
	if err := s.CreateOAuthConnection(OAuthConnection{RequestID: "req-3678-first", UserID: "user-123", Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeOAuthGrant("req-3678-first"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAccessToken(newTestRequest(c.ID, "first-a", "req-3678-first")); !errors.Is(err, ErrOAuthGrantRevoked) {
		t.Fatalf("first issuance on a revoked grant: %v, want ErrOAuthGrantRevoked", err)
	}
}

// The revoke of a grant with no tokens yet waits on the person's row that a
// first issuance share-locks, so it cannot finish between that issuance's
// check and its commit.
func TestBUG3678_TokenlessRevokeWaitsOnIssuanceLock_Postgres(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("Postgres-only: row-lock ordering")
	}
	s := testStore(t)
	newTestClient(t, s)
	if err := s.CreateOAuthConnection(OAuthConnection{RequestID: "req-3678-wait", UserID: "user-123", Name: "x"}); err != nil {
		t.Fatal(err)
	}
	issuance, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = issuance.Rollback() }()
	if err := s.requireActiveUserTx(issuance, "user-123"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.RevokeOAuthGrant("req-3678-wait") }()
	waiting := false
	for i := 0; i < 100 && !waiting; i++ {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%FOR NO KEY UPDATE%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		waiting = n > 0
		if !waiting {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !waiting {
		t.Fatal("the revoke did not wait on the issuance's share lock")
	}
	if err := issuance.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("revoke: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the revoke never finished")
	}
}
