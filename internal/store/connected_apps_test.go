package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Connected-apps store tests (PLAN-943 TASK-954).
//
// What's covered:
//
//   - ListUserOAuthConnections deduplicates a chain across multiple
//     access-token rows (refresh-token rotation produces siblings).
//   - The list filters on subject — Bob can't see Alice's connections.
//   - Active=FALSE chains drop off the list immediately.
//   - Connected_at is the earliest timestamp in the chain.
//   - GrantedScopes + AllowedWorkspaces hydrate from the newest row's
//     session_data + granted_scopes (revoked rows ignored on read).
//   - classifyCapabilityTier maps scope vocabulary to read_only /
//     read_write / full_access / unknown.
//   - parseAllowedWorkspacesFromSession round-trips both []string
//     and []interface{} (post-JSON) shapes.
//   - RevokeUserOAuthConnection: ownership check returns NotFound
//     for stranger's chains; idempotent for already-revoked.

func newConnectedTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "conn.db"))
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedClient creates an oauth_clients row so chains have something
// to FK against. Returns the client ID.
func seedClient(t *testing.T, s *Store, name string) string {
	t.Helper()
	c, err := s.CreateOAuthClient(models.OAuthClientCreate{
		Name:                    name,
		RedirectURIs:            []string{"https://example.test/cb"},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scopes:                  []string{"pad:read", "pad:write"},
		Public:                  true,
	})
	if err != nil {
		t.Fatalf("CreateOAuthClient: %v", err)
	}
	return c.ID
}

// seedUser creates a user that subsequent oauth tokens can use as
// their `subject`.
func seedUser(t *testing.T, s *Store, email string) string {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{
		Email:    email,
		Name:     "Conn Tester",
		Password: "pw-conn-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u.ID
}

// seedAccess inserts an access-token row in the chain identified by
// requestID. ts is the requested_at timestamp; sessionData + scopes
// land on the row. Returns the signature it minted.
func seedAccess(t *testing.T, s *Store, requestID, clientID, subject string, ts time.Time, sessionData, scopes string, active bool) string {
	t.Helper()
	sig := newID()
	err := s.CreateAccessToken(models.OAuthRequest{
		Signature:     sig,
		RequestID:     requestID,
		RequestedAt:   ts,
		ClientID:      clientID,
		GrantedScopes: scopes,
		SessionData:   sessionData,
		Subject:       subject,
	})
	if err != nil {
		t.Fatalf("CreateAccessToken: %v", err)
	}
	if !active {
		// CreateAccessToken always inserts active=TRUE; flip if needed.
		if err := s.RevokeAccessTokenFamily(requestID); err != nil {
			t.Fatalf("RevokeAccessTokenFamily: %v", err)
		}
	}
	return sig
}

func TestListUserOAuthConnections_DeduplicatesChain(t *testing.T) {
	s := newConnectedTestStore(t)
	uid := seedUser(t, s, "list-dedup@example.com")
	clientID := seedClient(t, s, "Claude Desktop")

	now := time.Now().UTC().Truncate(time.Second)
	// Three access rows in the SAME chain (refresh-token rotation
	// scenario). Different requested_at; same request_id.
	sessionData := `{"extra":{"allowed_workspaces":["docapp"]}}`
	seedAccess(t, s, "chain-1", clientID, uid, now.Add(-3*time.Hour), sessionData, "pad:read pad:write", true)
	seedAccess(t, s, "chain-1", clientID, uid, now.Add(-2*time.Hour), sessionData, "pad:read pad:write", true)
	seedAccess(t, s, "chain-1", clientID, uid, now.Add(-1*time.Hour), sessionData, "pad:read pad:write", true)

	// Workspace + backfill so the rewritten read path (TASK-1522)
	// projects AllowedWorkspaces from oauth_connection_workspaces
	// instead of the retired session.Extra parse. Production wires
	// the same call at startup; tests do it explicitly.
	ownerForWS := seedUser(t, s, "chain-1-ws-owner@example.com")
	if _, wsErr := s.CreateWorkspace(models.WorkspaceCreate{Name: "docapp", OwnerID: ownerForWS}); wsErr != nil {
		t.Fatalf("CreateWorkspace docapp: %v", wsErr)
	}
	if _, bfErr := s.BackfillOAuthConnections(); bfErr != nil {
		t.Fatalf("BackfillOAuthConnections: %v", bfErr)
	}

	conns, err := s.ListUserOAuthConnections(uid)
	if err != nil {
		t.Fatalf("ListUserOAuthConnections: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("got %d connections, want 1 (deduplicated chain)", len(conns))
	}
	c := conns[0]
	if c.RequestID != "chain-1" {
		t.Errorf("RequestID = %q, want chain-1", c.RequestID)
	}
	if c.ClientName != "Claude Desktop" {
		t.Errorf("ClientName = %q, want Claude Desktop", c.ClientName)
	}
	if c.CapabilityTier != models.CapabilityTierReadWrite {
		t.Errorf("CapabilityTier = %q, want read_write", c.CapabilityTier)
	}
	if !c.ConnectedAt.Equal(now.Add(-3 * time.Hour)) {
		t.Errorf("ConnectedAt = %v, want earliest chain timestamp %v", c.ConnectedAt, now.Add(-3*time.Hour))
	}
	if len(c.AllowedWorkspaces) != 1 || c.AllowedWorkspaces[0] != "docapp" {
		t.Errorf("AllowedWorkspaces = %v, want [docapp]", c.AllowedWorkspaces)
	}
}

func TestListUserOAuthConnections_FiltersBySubject(t *testing.T) {
	s := newConnectedTestStore(t)
	alice := seedUser(t, s, "alice-conn@example.com")
	bob := seedUser(t, s, "bob-conn@example.com")
	clientID := seedClient(t, s, "Cursor")

	now := time.Now().UTC()
	seedAccess(t, s, "alice-chain", clientID, alice, now, "", "pad:read", true)
	seedAccess(t, s, "bob-chain", clientID, bob, now, "", "pad:read", true)

	aliceConns, err := s.ListUserOAuthConnections(alice)
	if err != nil {
		t.Fatalf("ListUserOAuthConnections(alice): %v", err)
	}
	if len(aliceConns) != 1 || aliceConns[0].RequestID != "alice-chain" {
		t.Errorf("Alice should see only alice-chain, got %+v", aliceConns)
	}

	bobConns, err := s.ListUserOAuthConnections(bob)
	if err != nil {
		t.Fatalf("ListUserOAuthConnections(bob): %v", err)
	}
	if len(bobConns) != 1 || bobConns[0].RequestID != "bob-chain" {
		t.Errorf("Bob should see only bob-chain, got %+v", bobConns)
	}
}

func TestListUserOAuthConnections_ExcludesInactiveChains(t *testing.T) {
	s := newConnectedTestStore(t)
	uid := seedUser(t, s, "inactive@example.com")
	clientID := seedClient(t, s, "Test Client")
	now := time.Now().UTC()

	seedAccess(t, s, "active-chain", clientID, uid, now, "", "pad:read", true)
	seedAccess(t, s, "revoked-chain", clientID, uid, now, "", "pad:read", false)

	conns, err := s.ListUserOAuthConnections(uid)
	if err != nil {
		t.Fatalf("ListUserOAuthConnections: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("got %d connections, want 1 (revoked chain excluded)", len(conns))
	}
	if conns[0].RequestID != "active-chain" {
		t.Errorf("got RequestID %q, want active-chain", conns[0].RequestID)
	}
}

func TestRevokeUserOAuthConnection_OwnershipCheck(t *testing.T) {
	s := newConnectedTestStore(t)
	alice := seedUser(t, s, "revoke-alice@example.com")
	bob := seedUser(t, s, "revoke-bob@example.com")
	clientID := seedClient(t, s, "Revoke Tester")
	now := time.Now().UTC()

	seedAccess(t, s, "alice-only", clientID, alice, now, "", "pad:read", true)

	// Bob can't revoke Alice's chain — should get NotFound (not 403,
	// to prevent enumeration via 403-vs-404 distinction).
	if err := s.RevokeUserOAuthConnection(bob, "alice-only"); err != ErrConnectionNotFound {
		t.Errorf("Bob revoking Alice's chain: got err=%v, want ErrConnectionNotFound", err)
	}

	// Alice's chain is still active after Bob's failed attempt.
	conns, err := s.ListUserOAuthConnections(alice)
	if err != nil {
		t.Fatalf("ListUserOAuthConnections after failed revoke: %v", err)
	}
	if len(conns) != 1 {
		t.Errorf("after Bob's failed revoke, Alice's chain count = %d, want 1", len(conns))
	}

	// Alice CAN revoke her own.
	if err := s.RevokeUserOAuthConnection(alice, "alice-only"); err != nil {
		t.Errorf("Alice revoking her own chain: %v", err)
	}
	conns, err = s.ListUserOAuthConnections(alice)
	if err != nil {
		t.Fatalf("ListUserOAuthConnections after revoke: %v", err)
	}
	if len(conns) != 0 {
		t.Errorf("after Alice's revoke, chain count = %d, want 0", len(conns))
	}
}

func TestRevokeUserOAuthConnection_Idempotent(t *testing.T) {
	s := newConnectedTestStore(t)
	uid := seedUser(t, s, "idem@example.com")
	clientID := seedClient(t, s, "Idem Tester")
	seedAccess(t, s, "idem-chain", clientID, uid, time.Now().UTC(), "", "pad:read", true)

	if err := s.RevokeUserOAuthConnection(uid, "idem-chain"); err != nil {
		t.Errorf("first revoke: %v", err)
	}
	if err := s.RevokeUserOAuthConnection(uid, "idem-chain"); err != nil {
		t.Errorf("second revoke (idempotent): %v", err)
	}
}

func TestRevokeUserOAuthConnection_UnknownChain(t *testing.T) {
	s := newConnectedTestStore(t)
	uid := seedUser(t, s, "unknown@example.com")
	if err := s.RevokeUserOAuthConnection(uid, "no-such-chain"); err != ErrConnectionNotFound {
		t.Errorf("unknown chain: got err=%v, want ErrConnectionNotFound", err)
	}
}

func TestClassifyCapabilityTier(t *testing.T) {
	cases := []struct {
		scopes string
		want   models.CapabilityTier
	}{
		{"", models.CapabilityTierUnknown},
		{"pad:read", models.CapabilityTierReadOnly},
		{"pad:read pad:write", models.CapabilityTierReadWrite},
		{"pad:write", models.CapabilityTierReadWrite},
		{"pad:read pad:write pad:admin", models.CapabilityTierFullAccess},
		{"pad:admin", models.CapabilityTierFullAccess},
		{"openid email", models.CapabilityTierUnknown},
	}
	for _, tc := range cases {
		if got := classifyCapabilityTier(tc.scopes); got != tc.want {
			t.Errorf("classifyCapabilityTier(%q) = %q, want %q", tc.scopes, got, tc.want)
		}
	}
}

// TestExtractAllowedWorkspacesFromSessionExtra covers the parse helper
// that drives the TASK-1522 backfill from session.Extra into the new
// oauth_connections + oauth_connection_workspaces tables. Replaces the
// retired parseAllowedWorkspacesFromSession test (which was the
// previous read-path's parse helper, removed when the read path
// switched to the new tables).
//
// Shape mapping (per IDEA-1517 §2 backfill spec):
//
//   - missing / malformed / non-array → (hasKey=false, isStar=false, slugs=nil)
//     → seeder writes all_current_workspaces=1, no join rows.
//   - ["*"] singleton                  → (hasKey=true, isStar=true, slugs=nil)
//     → seeder writes all_current_workspaces=1, no join rows.
//   - explicit slug list               → (hasKey=true, isStar=false, slugs=<list>)
//     → seeder writes all_current_workspaces=0, one join row per slug.
func TestExtractAllowedWorkspacesFromSessionExtra(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantKey   bool
		wantStar  bool
		wantSlugs []string
	}{
		{"empty session", "", false, false, nil},
		{"missing extra key", `{"extra":{}}`, false, false, nil},
		{"explicit slug list", `{"extra":{"allowed_workspaces":["alpha","beta"]}}`, true, false, []string{"alpha", "beta"}},
		{"wildcard singleton", `{"extra":{"allowed_workspaces":["*"]}}`, true, true, nil},
		{"non-string elements dropped", `{"extra":{"allowed_workspaces":["alpha",42,"beta"]}}`, true, false, []string{"alpha", "beta"}},
		{"explicit empty list", `{"extra":{"allowed_workspaces":[]}}`, true, false, nil},
		{"wildcard mixed with slug", `{"extra":{"allowed_workspaces":["*","alpha"]}}`, true, false, []string{"*", "alpha"}},
		{"malformed JSON", `not json`, false, false, nil},
		{"non-array value", `{"extra":{"allowed_workspaces":"alpha"}}`, false, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractAllowedWorkspacesFromSessionExtra(tc.in)
			if got.hasKey != tc.wantKey {
				t.Errorf("hasKey = %v, want %v", got.hasKey, tc.wantKey)
			}
			if got.isStar != tc.wantStar {
				t.Errorf("isStar = %v, want %v", got.isStar, tc.wantStar)
			}
			if len(got.slugs) != len(tc.wantSlugs) {
				t.Errorf("slugs len = %d (%v), want %d (%v)", len(got.slugs), got.slugs, len(tc.wantSlugs), tc.wantSlugs)
				return
			}
			for i := range got.slugs {
				if got.slugs[i] != tc.wantSlugs[i] {
					t.Errorf("slugs[%d] = %q, want %q", i, got.slugs[i], tc.wantSlugs[i])
				}
			}
		})
	}
}
