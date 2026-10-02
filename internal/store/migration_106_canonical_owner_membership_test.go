package store

import (
	"os"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3355 repair: migration 106 / PG 080 makes every live workspace's
// canonical owner an owner member, only ever adding or raising.
func TestMigration106_CanonicalOwnerMembership(t *testing.T) {
	s := testStore(t)
	path := "migrations/106_canonical_owner_membership.sql"
	if s.dialect.Driver() == DriverPostgres {
		path = "pgmigrations/080_canonical_owner_membership.sql"
	}
	repair, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}

	mkWS := func(owner *models.User, name string) *models.Workspace {
		t.Helper()
		ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: name, OwnerID: owner.ID})
		if err != nil {
			t.Fatalf("CreateWorkspace %s: %v", name, err)
		}
		return ws
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(s.q(q), args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	roleOf := func(wsID, userID string) string {
		t.Helper()
		m, err := s.GetWorkspaceMember(wsID, userID)
		if err != nil {
			t.Fatalf("GetWorkspaceMember: %v", err)
		}
		if m == nil {
			return ""
		}
		return m.Role
	}

	alice := createTestUser(t, s, "alice@test.com", "Alice", "password123")
	bob := createTestUser(t, s, "bob@test.com", "Bob", "password123")

	// demoted: canonical owner holds an editor membership, a co-owner exists.
	demoted := mkWS(alice, "Demoted")
	exec(`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, 'editor')`, demoted.ID, alice.ID)
	exec(`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, 'owner')`, demoted.ID, bob.ID)
	// missing: canonical owner has no membership at all; bob is a viewer.
	missing := mkWS(alice, "Missing")
	exec(`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, 'viewer')`, missing.ID, bob.ID)
	// consistent: nothing to change.
	consistent := mkWS(bob, "Consistent")
	exec(`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, 'owner')`, consistent.ID, bob.ID)
	exec(`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, 'viewer')`, consistent.ID, alice.ID)
	// deleted: a soft-deleted workspace is left as it is.
	deleted := mkWS(alice, "Deleted")
	exec(`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, 'viewer')`, deleted.ID, alice.ID)
	exec(`UPDATE workspaces SET deleted_at = '2026-01-01T00:00:00Z' WHERE id = ?`, deleted.ID)

	for run := 1; run <= 2; run++ { // the second run proves idempotence
		if _, err := s.db.Exec(string(repair)); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		checks := []struct {
			ws, user, want, what string
		}{
			{demoted.ID, alice.ID, "owner", "demoted canonical owner raised"},
			{demoted.ID, bob.ID, "owner", "co-owner untouched"},
			{missing.ID, alice.ID, "owner", "missing canonical owner added"},
			{missing.ID, bob.ID, "viewer", "other member untouched"},
			{consistent.ID, bob.ID, "owner", "consistent owner untouched"},
			{consistent.ID, alice.ID, "viewer", "never raises a non-canonical member"},
			{deleted.ID, alice.ID, "viewer", "soft-deleted workspace untouched"},
		}
		for _, c := range checks {
			if got := roleOf(c.ws, c.user); got != c.want {
				t.Errorf("run %d, %s: role %q, want %q", run, c.what, got, c.want)
			}
		}
	}
}
