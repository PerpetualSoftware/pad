package server

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

// BUG-3301, through the real /oauth/token: what each refresh leaves
// behind, and what the sweep may and may not take from a live chain.

func sweepRefresh(t *testing.T, srv *Server, clientID, refresh string) (*httptestResult, string) {
	t.Helper()
	rr := postOAuthForm(srv, "/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {clientID},
		"audience":      {testCanonicalAudience},
	})
	res := &httptestResult{code: rr.Code, body: rr.Body.String()}
	if rr.Code != http.StatusOK {
		return res, ""
	}
	var resp map[string]any
	parseJSON(t, rr, &resp)
	next, _ := resp["refresh_token"].(string)
	return res, next
}

type httptestResult struct {
	code int
	body string
}

func sweepCount(t *testing.T, srv *Server, table, where string, args ...any) int {
	t.Helper()
	var n int
	if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func sweepAge(t *testing.T, srv *Server, table, where string, age time.Duration, args ...any) {
	t.Helper()
	at := time.Now().UTC().Add(-age).Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(`UPDATE `+table+` SET requested_at = ? WHERE `+where, append([]any{at}, args...)...); err != nil {
		t.Fatalf("age %s: %v", table, err)
	}
}

func TestOAuthSweep_LiveChainAndReplay(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	user, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	// Chain A: a grant and three refreshes, a second apart so each
	// rotation has its own requested_at (second precision).
	tokens := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-sweep-a-quick-brown-fox-1234567890-abc")
	refresh := []string{tokens["refresh_token"].(string)}
	for i := 1; i <= 3; i++ {
		time.Sleep(1100 * time.Millisecond)
		res, next := sweepRefresh(t, srv, clientID, refresh[i-1])
		if res.code != http.StatusOK {
			t.Fatalf("refresh %d: %d %s", i, res.code, res.body)
		}
		refresh = append(refresh, next)
	}
	// The growth BUG-3301 measured: one access and one refresh row per
	// refresh, and only the newest pair active.
	if a, r := sweepCount(t, srv, "oauth_access_tokens", "1=1"), sweepCount(t, srv, "oauth_refresh_tokens", "1=1"); a != 4 || r != 4 {
		t.Fatalf("after 3 refreshes: access %d, refresh %d rows, want 4 and 4", a, r)
	}
	var chainA string
	if err := srv.store.DB().QueryRow(`SELECT request_id FROM oauth_refresh_tokens LIMIT 1`).Scan(&chainA); err != nil {
		t.Fatal(err)
	}
	var sigs []string
	rows, err := srv.store.DB().Query(`SELECT signature FROM oauth_refresh_tokens ORDER BY requested_at`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, s)
	}
	rows.Close()
	if len(sigs) != 4 {
		t.Fatalf("refresh signatures = %d, want 4", len(sigs))
	}

	// Chain B: a second grant for the same user, about to be aged out
	// whole, active pair included: a chain whose refresh token expired.
	runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-sweep-b-quick-brown-fox-1234567890-abc")
	liveBefore, err := srv.store.CountLiveOAuthConnections()
	if err != nil {
		t.Fatal(err)
	}
	listed, err := srv.store.ListUserOAuthConnections(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if liveBefore != 2 || len(listed) != 2 {
		t.Fatalf("before the sweep: live %d, listed %d, want 2 and 2", liveBefore, len(listed))
	}

	// Age chain A: its two oldest refresh rows past the refresh cutoff
	// (30 d + 24 h), the third rotated row young, and its inactive access
	// rows past the access cutoff (1 h + 24 h). Age chain B entirely.
	sweepAge(t, srv, "oauth_refresh_tokens", "signature IN (?, ?)", 32*24*time.Hour, sigs[0], sigs[1])
	sweepAge(t, srv, "oauth_refresh_tokens", "signature = ?", 24*time.Hour, sigs[2])
	sweepAge(t, srv, "oauth_access_tokens", "request_id = ? AND active = 0", 26*time.Hour, chainA)
	for _, tbl := range []string{"oauth_access_tokens", "oauth_refresh_tokens", "oauth_authorization_codes"} {
		sweepAge(t, srv, tbl, "request_id <> ?", 32*24*time.Hour, chainA)
	}

	srv.sweepExpiredOAuthRows()

	// Chain A keeps the young rotated row and the live pair.
	if n := sweepCount(t, srv, "oauth_refresh_tokens", "request_id = ?", chainA); n != 2 {
		t.Errorf("chain A refresh rows = %d, want 2 (the young rotated row and the live one)", n)
	}
	for _, sig := range sigs[:2] {
		if sweepCount(t, srv, "oauth_refresh_tokens", "signature = ?", sig) != 0 {
			t.Errorf("an aged-out rotated refresh row survived")
		}
	}
	if n := sweepCount(t, srv, "oauth_access_tokens", "request_id = ?", chainA); n != 1 {
		t.Errorf("chain A access rows = %d, want only the live one", n)
	}
	// Chain B is gone, and Connected Apps and the resume count drop it.
	for _, tbl := range []string{"oauth_access_tokens", "oauth_refresh_tokens", "oauth_authorization_codes"} {
		if n := sweepCount(t, srv, tbl, "request_id <> ?", chainA); n != 0 {
			t.Errorf("chain B: %d rows left in %s", n, tbl)
		}
	}
	liveAfter, _ := srv.store.CountLiveOAuthConnections()
	listed, _ = srv.store.ListUserOAuthConnections(user.ID)
	if liveAfter != 1 || len(listed) != 1 {
		t.Errorf("after the sweep: live %d, listed %d, want 1 and 1", liveAfter, len(listed))
	}

	// The live chain still refreshes.
	res, r4 := sweepRefresh(t, srv, clientID, refresh[3])
	if res.code != http.StatusOK {
		t.Fatalf("live chain refresh after the sweep: %d %s", res.code, res.body)
	}
	// A swept (expired) rotated token is refused, and, its row being gone,
	// does not revoke the family: the documented trade-off.
	if res, _ := sweepRefresh(t, srv, clientID, refresh[0]); res.code == http.StatusOK {
		t.Fatalf("a swept refresh token was accepted")
	}
	res, r5 := sweepRefresh(t, srv, clientID, r4)
	if res.code != http.StatusOK {
		t.Fatalf("the family was revoked by a replay of a swept token: %d %s", res.code, res.body)
	}
	// A retained rotated token still triggers family revocation.
	if res, _ := sweepRefresh(t, srv, clientID, refresh[2]); res.code == http.StatusOK {
		t.Fatalf("a replayed rotated refresh token was accepted")
	}
	if res, _ := sweepRefresh(t, srv, clientID, r5); res.code == http.StatusOK {
		t.Fatalf("replaying a retained rotated token did not revoke the family")
	}
}

// The cutoffs follow the running server's lifespans, each padded by the
// grace; codes add the code lifespan to the refresh lifespan, since the
// first refresh token starts at the exchange (BUG-3301 retention).
func TestOAuthSweepCutoffs(t *testing.T) {
	srv, o := oauthEnabledTestServer(t)
	l := o.Lifespans()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	c := srv.oauthSweepCutoffs(now)
	want := map[string]time.Time{
		"access":  now.Add(-(l.AccessToken + oauthSweepGrace)),
		"refresh": now.Add(-(l.RefreshToken + oauthSweepGrace)),
		"codes":   now.Add(-(l.AuthorizeCode + l.RefreshToken + oauthSweepGrace)),
		"pkce":    now.Add(-(l.AuthorizeCode + oauthSweepGrace)),
	}
	got := map[string]time.Time{"access": c.AccessTokens, "refresh": c.RefreshTokens, "codes": c.AuthorizationCodes, "pkce": c.PKCERequests}
	for k := range want {
		if !got[k].Equal(want[k]) {
			t.Errorf("%s cutoff = %v, want %v", k, got[k], want[k])
		}
	}
	// Without an OAuth server the package defaults apply.
	srv.oauthServer = nil
	if c := srv.oauthSweepCutoffs(now); c.RefreshTokens.IsZero() || c.AccessTokens.IsZero() {
		t.Errorf("no OAuth server: cutoffs %+v, want the defaults", c)
	}
}
