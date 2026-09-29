package store

import (
	"fmt"
	"testing"
	"time"
)

// BUG-3301: SweepExpiredOAuthRows deletes exactly the rows below each
// table's cutoff, on both dialects, in bounded batches.

func sweepSeed(t *testing.T, s *Store, clientID, table, sig string, at time.Time) {
	t.Helper()
	req := newTestRequest(clientID, sig, "chain-"+sig)
	req.RequestedAt = at
	var err error
	switch table {
	case "oauth_access_tokens":
		err = s.CreateAccessToken(req)
	case "oauth_refresh_tokens":
		err = s.CreateRefreshToken(req)
	case "oauth_authorization_codes":
		err = s.CreateAuthorizationCode(req)
	case "oauth_pkce_requests":
		err = s.CreatePKCERequest(req)
	}
	if err != nil {
		t.Fatalf("seed %s %s: %v", table, sig, err)
	}
}

func sweepSignatures(t *testing.T, s *Store, table string) map[string]bool {
	t.Helper()
	rows, err := s.db.Query(`SELECT signature FROM ` + table)
	if err != nil {
		t.Fatalf("list %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var sig string
		if err := rows.Scan(&sig); err != nil {
			t.Fatal(err)
		}
		out[sig] = true
	}
	return out
}

var sweepTables = []string{"oauth_access_tokens", "oauth_refresh_tokens", "oauth_authorization_codes", "oauth_pkce_requests"}

// Each table gets its own cutoff; a row one second below it goes, a row
// one second above it stays, and an inactive row is judged by age alone.
func TestSweepExpiredOAuthRows_PerTableCutoffs(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	c := newTestClient(t, s)
	now := time.Now().UTC().Truncate(time.Second)
	cut := OAuthSweepCutoffs{
		AccessTokens:       now.Add(-1 * time.Hour),
		RefreshTokens:      now.Add(-2 * time.Hour),
		AuthorizationCodes: now.Add(-3 * time.Hour),
		PKCERequests:       now.Add(-4 * time.Hour),
	}
	cutoffOf := map[string]time.Time{
		"oauth_access_tokens":       cut.AccessTokens,
		"oauth_refresh_tokens":      cut.RefreshTokens,
		"oauth_authorization_codes": cut.AuthorizationCodes,
		"oauth_pkce_requests":       cut.PKCERequests,
	}
	for _, tbl := range sweepTables {
		sweepSeed(t, s, c.ID, tbl, tbl+"-old", cutoffOf[tbl].Add(-time.Second))
		sweepSeed(t, s, c.ID, tbl, tbl+"-young", cutoffOf[tbl].Add(time.Second))
	}
	// An inactive young refresh row (a rotated one) is kept: age decides.
	sweepSeed(t, s, c.ID, "oauth_refresh_tokens", "rotated-young", cut.RefreshTokens.Add(time.Minute))
	if err := s.RevokeRefreshTokenFamily("chain-rotated-young"); err != nil {
		t.Fatal(err)
	}

	res, err := s.SweepExpiredOAuthRows(cut, 500, 10, 0)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.AccessTokens != 1 || res.RefreshTokens != 1 || res.AuthorizationCodes != 1 || res.PKCERequests != 1 || res.Capped {
		t.Fatalf("result = %+v, want one row from each table, not capped", res)
	}
	for _, tbl := range sweepTables {
		got := sweepSignatures(t, s, tbl)
		if got[tbl+"-old"] {
			t.Errorf("%s: the row below the cutoff survived", tbl)
		}
		if !got[tbl+"-young"] {
			t.Errorf("%s: the row above the cutoff was deleted", tbl)
		}
	}
	if !sweepSignatures(t, s, "oauth_refresh_tokens")["rotated-young"] {
		t.Error("an inactive refresh row younger than the cutoff was deleted")
	}
}

// A zero cutoff leaves that table alone.
func TestSweepExpiredOAuthRows_ZeroCutoffSkipsTable(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	c := newTestClient(t, s)
	old := time.Now().UTC().Add(-100 * 24 * time.Hour)
	for _, tbl := range sweepTables {
		sweepSeed(t, s, c.ID, tbl, tbl+"-ancient", old)
	}
	res, err := s.SweepExpiredOAuthRows(OAuthSweepCutoffs{AccessTokens: time.Now().UTC()}, 500, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.AccessTokens != 1 || res.Total() != 1 {
		t.Fatalf("result = %+v, want only the access row", res)
	}
	for _, tbl := range sweepTables[1:] {
		if !sweepSignatures(t, s, tbl)[tbl+"-ancient"] {
			t.Errorf("%s was swept with a zero cutoff", tbl)
		}
	}
}

// Batches: the cap stops one call and reports it; the next call finishes.
func TestSweepExpiredOAuthRows_BatchesAndCap(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	c := newTestClient(t, s)
	old := time.Now().UTC().Add(-48 * time.Hour)
	for i := 0; i < 7; i++ {
		sweepSeed(t, s, c.ID, "oauth_access_tokens", fmt.Sprintf("a%d", i), old)
	}
	sweepSeed(t, s, c.ID, "oauth_access_tokens", "fresh", time.Now().UTC())
	cut := OAuthSweepCutoffs{AccessTokens: time.Now().UTC().Add(-time.Hour)}

	res, err := s.SweepExpiredOAuthRows(cut, 2, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.AccessTokens != 4 || !res.Capped {
		t.Fatalf("first call = %+v, want 4 rows (2 batches of 2) and capped", res)
	}
	res, err = s.SweepExpiredOAuthRows(cut, 2, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.AccessTokens != 3 || res.Capped {
		t.Fatalf("second call = %+v, want the remaining 3, not capped", res)
	}
	if got := sweepSignatures(t, s, "oauth_access_tokens"); len(got) != 1 || !got["fresh"] {
		t.Fatalf("left %v, want only the fresh row", got)
	}
	if _, err := s.SweepExpiredOAuthRows(cut, 0, 1, 0); err == nil {
		t.Error("a zero batch size was accepted")
	}
}
