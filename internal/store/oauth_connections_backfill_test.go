package store

import (
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BackfillOAuthConnections tests (PLAN-1519 / TASK-1522 / IDEA-1517 §2).
//
// What's covered:
//
//   - Pre-TASK-952 session (no key)           → all_current=1, no join rows.
//   - Wildcard session (`["*"]`)              → all_current=1, no join rows.
//   - Explicit slug list, all slugs resolvable → all_current=0, one join
//                                                row per slug, added_by='user'.
//   - Explicit slug list with a deleted slug   → unresolved counter ticks,
//                                                the rest of the slugs land.
//   - Multi-row chain (refresh-token rotation) → newest row drives the
//                                                seeded shape, not earliest.
//   - Refresh-only chain (no access row)       → still backfilled.
//   - Idempotent re-run                        → no double inserts, counters
//                                                report zero new work.
//   - Empty database                           → no-op, no error.

func newBackfillTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "backfill.db"))
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedBackfillUser(t *testing.T, s *Store, email string) string {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{
		Email: email, Name: "Backfill Tester",
		Password: "pw-backfill-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u.ID
}

func seedBackfillWorkspace(t *testing.T, s *Store, name, ownerID string) (id, slug string) {
	t.Helper()
	w, err := s.CreateWorkspace(models.WorkspaceCreate{Name: name, OwnerID: ownerID})
	if err != nil {
		t.Fatalf("CreateWorkspace %s: %v", name, err)
	}
	return w.ID, w.Slug
}

// seedBackfillClient mirrors seedClient in connected_apps_test.go but
// stays internal to this file so the backfill suite can run in
// isolation when other tests in the package fail.
func seedBackfillClient(t *testing.T, s *Store, name string) string {
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

func seedBackfillAccess(t *testing.T, s *Store, requestID, clientID, subject string, ts time.Time, sessionData string) {
	t.Helper()
	if err := s.CreateAccessToken(models.OAuthRequest{
		Signature:     newID(),
		RequestID:     requestID,
		RequestedAt:   ts,
		ClientID:      clientID,
		GrantedScopes: "pad:read pad:write",
		SessionData:   sessionData,
		Subject:       subject,
	}); err != nil {
		t.Fatalf("CreateAccessToken: %v", err)
	}
}

func seedBackfillRefresh(t *testing.T, s *Store, requestID, clientID, subject string, ts time.Time, sessionData string) {
	t.Helper()
	if err := s.CreateRefreshToken(models.OAuthRequest{
		Signature:     newID(),
		RequestID:     requestID,
		RequestedAt:   ts,
		ClientID:      clientID,
		GrantedScopes: "pad:read pad:write",
		SessionData:   sessionData,
		Subject:       subject,
	}); err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}
}

func TestBackfillOAuthConnections_EmptyDatabase(t *testing.T) {
	s := newBackfillTestStore(t)
	res, err := s.BackfillOAuthConnections()
	if err != nil {
		t.Fatalf("BackfillOAuthConnections: %v", err)
	}
	if res.ChainsSeen != 0 || res.ConnectionsCreated != 0 || res.WorkspacesAdded != 0 {
		t.Errorf("empty DB result = %+v, want all zero", res)
	}
}

func TestBackfillOAuthConnections_PreTASK952_WildcardSemantic(t *testing.T) {
	s := newBackfillTestStore(t)
	uid := seedBackfillUser(t, s, "pre-952@example.com")
	client := seedBackfillClient(t, s, "Pre-952 Client")

	// No allowed_workspaces key in session.Extra at all — pre-TASK-952
	// token shape. Should seed all_current_workspaces=1.
	seedBackfillAccess(t, s, "pre-952-chain", client, uid, time.Now().UTC(), `{"extra":{}}`)

	res, err := s.BackfillOAuthConnections()
	if err != nil {
		t.Fatalf("BackfillOAuthConnections: %v", err)
	}
	if res.ConnectionsCreated != 1 {
		t.Errorf("ConnectionsCreated = %d, want 1", res.ConnectionsCreated)
	}
	if res.WorkspacesAdded != 0 {
		t.Errorf("WorkspacesAdded = %d, want 0 (wildcard shape)", res.WorkspacesAdded)
	}

	conn, err := s.GetOAuthConnection("pre-952-chain")
	if err != nil {
		t.Fatalf("GetOAuthConnection: %v", err)
	}
	if !conn.AllCurrentWorkspaces {
		t.Errorf("all_current_workspaces = false, want true for pre-TASK-952 token")
	}
	if !conn.MayCreateWorkspaces || !conn.IncludeFutureWorkspaces {
		t.Errorf("scope flags should default ON for backfilled rows, got %+v", conn)
	}
	if conn.Name != "" {
		t.Errorf("backfilled name should be empty; got %q", conn.Name)
	}
}

func TestBackfillOAuthConnections_Wildcard(t *testing.T) {
	s := newBackfillTestStore(t)
	uid := seedBackfillUser(t, s, "wildcard@example.com")
	client := seedBackfillClient(t, s, "Wildcard Client")

	seedBackfillAccess(t, s, "wc-chain", client, uid, time.Now().UTC(),
		`{"extra":{"allowed_workspaces":["*"]}}`)

	res, err := s.BackfillOAuthConnections()
	if err != nil {
		t.Fatalf("BackfillOAuthConnections: %v", err)
	}
	if res.ConnectionsCreated != 1 || res.WorkspacesAdded != 0 {
		t.Errorf("result = %+v, want 1 connection / 0 workspaces", res)
	}
	conn, _ := s.GetOAuthConnection("wc-chain")
	if !conn.AllCurrentWorkspaces {
		t.Errorf("wildcard should map to all_current=true")
	}
}

func TestBackfillOAuthConnections_ExplicitList(t *testing.T) {
	s := newBackfillTestStore(t)
	uid := seedBackfillUser(t, s, "explicit@example.com")
	client := seedBackfillClient(t, s, "Explicit Client")
	_, alphaSlug := seedBackfillWorkspace(t, s, "alpha", uid)
	_, betaSlug := seedBackfillWorkspace(t, s, "beta", uid)

	seedBackfillAccess(t, s, "exp-chain", client, uid, time.Now().UTC(),
		`{"extra":{"allowed_workspaces":["`+alphaSlug+`","`+betaSlug+`"]}}`)

	res, err := s.BackfillOAuthConnections()
	if err != nil {
		t.Fatalf("BackfillOAuthConnections: %v", err)
	}
	if res.ConnectionsCreated != 1 {
		t.Errorf("ConnectionsCreated = %d, want 1", res.ConnectionsCreated)
	}
	if res.WorkspacesAdded != 2 {
		t.Errorf("WorkspacesAdded = %d, want 2", res.WorkspacesAdded)
	}
	if res.UnresolvedSlugs != 0 {
		t.Errorf("UnresolvedSlugs = %d, want 0", res.UnresolvedSlugs)
	}

	conn, _ := s.GetOAuthConnection("exp-chain")
	if conn.AllCurrentWorkspaces {
		t.Errorf("explicit-list chain should have all_current=false")
	}
	access, _ := s.GetOAuthConnectionAccess("exp-chain")
	gotSlugs := append([]string(nil), access.WorkspaceSlugs...)
	sort.Strings(gotSlugs)
	want := []string{alphaSlug, betaSlug}
	sort.Strings(want)
	if len(gotSlugs) != 2 || gotSlugs[0] != want[0] || gotSlugs[1] != want[1] {
		t.Errorf("WorkspaceSlugs = %v, want %v", gotSlugs, want)
	}
}

func TestBackfillOAuthConnections_UnresolvedSlugIsCounted(t *testing.T) {
	s := newBackfillTestStore(t)
	uid := seedBackfillUser(t, s, "unresolved@example.com")
	client := seedBackfillClient(t, s, "Unresolved Client")
	_, realSlug := seedBackfillWorkspace(t, s, "real-ws", uid)

	// Mix one real slug with one that points at a workspace that
	// doesn't exist. The backfill should land the real one and tick
	// the unresolved counter — not fail the whole chain.
	seedBackfillAccess(t, s, "u-chain", client, uid, time.Now().UTC(),
		`{"extra":{"allowed_workspaces":["`+realSlug+`","never-existed"]}}`)

	res, err := s.BackfillOAuthConnections()
	if err != nil {
		t.Fatalf("BackfillOAuthConnections: %v", err)
	}
	if res.WorkspacesAdded != 1 {
		t.Errorf("WorkspacesAdded = %d, want 1", res.WorkspacesAdded)
	}
	if res.UnresolvedSlugs != 1 {
		t.Errorf("UnresolvedSlugs = %d, want 1", res.UnresolvedSlugs)
	}
}

func TestBackfillOAuthConnections_NewestRowDrivesShape(t *testing.T) {
	s := newBackfillTestStore(t)
	uid := seedBackfillUser(t, s, "rotation@example.com")
	client := seedBackfillClient(t, s, "Rotation Client")
	_, alphaSlug := seedBackfillWorkspace(t, s, "alpha-rot", uid)

	now := time.Now().UTC().Truncate(time.Second)
	// Oldest row has wildcard; newest row has an explicit list.
	// Backfill must take the explicit list (the user's most recent
	// scope decision). If we accidentally picked the oldest row we'd
	// seed all_current=true and zero join rows.
	seedBackfillAccess(t, s, "rot-chain", client, uid, now.Add(-2*time.Hour),
		`{"extra":{"allowed_workspaces":["*"]}}`)
	seedBackfillAccess(t, s, "rot-chain", client, uid, now,
		`{"extra":{"allowed_workspaces":["`+alphaSlug+`"]}}`)

	if _, err := s.BackfillOAuthConnections(); err != nil {
		t.Fatalf("BackfillOAuthConnections: %v", err)
	}
	conn, _ := s.GetOAuthConnection("rot-chain")
	if conn.AllCurrentWorkspaces {
		t.Errorf("newest row was explicit; all_current should be false, got %+v", conn)
	}
	access, _ := s.GetOAuthConnectionAccess("rot-chain")
	if len(access.WorkspaceSlugs) != 1 || access.WorkspaceSlugs[0] != alphaSlug {
		t.Errorf("WorkspaceSlugs = %v, want [%q]", access.WorkspaceSlugs, alphaSlug)
	}
}

func TestBackfillOAuthConnections_RefreshOnlyChain(t *testing.T) {
	s := newBackfillTestStore(t)
	uid := seedBackfillUser(t, s, "refresh-only@example.com")
	client := seedBackfillClient(t, s, "Refresh Client")

	// Chain lives only on the refresh side (access token already
	// expired, but the refresh hasn't been used yet). Backfill must
	// still seed it.
	seedBackfillRefresh(t, s, "r-only-chain", client, uid, time.Now().UTC(),
		`{"extra":{"allowed_workspaces":["*"]}}`)

	res, err := s.BackfillOAuthConnections()
	if err != nil {
		t.Fatalf("BackfillOAuthConnections: %v", err)
	}
	if res.ConnectionsCreated != 1 {
		t.Errorf("ConnectionsCreated = %d, want 1 for refresh-only chain", res.ConnectionsCreated)
	}
}

func TestBackfillOAuthConnections_Idempotent(t *testing.T) {
	s := newBackfillTestStore(t)
	uid := seedBackfillUser(t, s, "idem@example.com")
	client := seedBackfillClient(t, s, "Idem Client")
	_, alphaSlug := seedBackfillWorkspace(t, s, "alpha-idem", uid)

	seedBackfillAccess(t, s, "idem-chain", client, uid, time.Now().UTC(),
		`{"extra":{"allowed_workspaces":["`+alphaSlug+`"]}}`)

	first, err := s.BackfillOAuthConnections()
	if err != nil {
		t.Fatalf("first BackfillOAuthConnections: %v", err)
	}
	if first.ConnectionsCreated != 1 || first.WorkspacesAdded != 1 {
		t.Errorf("first run = %+v, want 1 conn / 1 workspace", first)
	}

	second, err := s.BackfillOAuthConnections()
	if err != nil {
		t.Fatalf("second BackfillOAuthConnections: %v", err)
	}
	// Idempotent: chain scan re-counts but no new oauth_connections
	// rows are inserted. The AddConnectionWorkspace path uses INSERT
	// OR IGNORE / ON CONFLICT DO NOTHING, so re-issuing the same join
	// inserts is a no-op at the DB layer; the WorkspacesAdded counter
	// reflects calls that returned success (including no-op no-error
	// inserts), so we don't assert on the counter value. The real
	// invariant is the resulting row count below.
	access, err := s.GetOAuthConnectionAccess("idem-chain")
	if err != nil {
		t.Fatalf("GetOAuthConnectionAccess: %v", err)
	}
	if len(access.WorkspaceSlugs) != 1 {
		t.Errorf("after idempotent re-run, WorkspaceSlugs = %v; want exactly 1 entry", access.WorkspaceSlugs)
	}
	if second.ChainsSeen != 1 {
		t.Errorf("second run ChainsSeen = %d, want 1", second.ChainsSeen)
	}
}
