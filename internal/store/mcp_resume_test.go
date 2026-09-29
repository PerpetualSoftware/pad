package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-2310 DR-7 resume counts. A connection is a grant chain (request_id)
// with an active access OR refresh row, the unit the Connected Apps page
// lists. So one chain with several rows counts once, a chain that is only
// refresh-active counts, and a revoked chain does not.
func TestCountLiveOAuthConnections(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	client := seedClient(t, s, "resume-client")
	user := seedUser(t, s, "resume@example.com")
	now := time.Now().UTC()

	// Chain A: two access rows (a refresh rotation), counted once.
	seedAccess(t, s, "chain-a", client, user, now, "{}", "pad:read", true)
	seedAccess(t, s, "chain-a", client, user, now.Add(time.Minute), "{}", "pad:read", true)
	// Chain B: revoked, not counted.
	seedAccess(t, s, "chain-b", client, user, now, "{}", "pad:read", false)
	// Chain C: only an active refresh row (its access token not yet
	// minted or already expired and gone), counted.
	if err := s.CreateRefreshToken(models.OAuthRequest{
		Signature: newID(), RequestID: "chain-c", RequestedAt: now,
		ClientID: client, GrantedScopes: "pad:read", SessionData: "{}", Subject: user,
	}); err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}

	n, err := s.CountLiveOAuthConnections()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("live connections = %d, want 2 (chains a and c)", n)
	}
}

// A PAT counts when /mcp would accept it: it has a user_id and has not
// expired. A legacy workspace token (no user) and an expired user token do
// not count.
func TestCountMCPUsablePATs(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := createTestUser(t, s, "pat-owner@example.com", "PAT Owner", "s3cret")

	if _, err := s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "live"}, 90, 0); err != nil {
		t.Fatal(err)
	}
	expired, err := s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "expired"}, 90, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE api_tokens SET expires_at = ? WHERE id = ?`),
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), expired.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(`INSERT INTO api_tokens (id, workspace_id, user_id, name, token_hash, prefix, scopes, created_at)
		VALUES (?, NULL, NULL, 'legacy', ?, 'pad_legacy', '["*"]', ?)`), newID(), newID(), now()); err != nil {
		t.Fatal(err)
	}

	n, err := s.CountMCPUsablePATs()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("usable PATs = %d, want 1 (live only)", n)
	}
}
