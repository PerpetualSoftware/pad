package store

import (
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestSystemCollectionAccessBackfill runs migration 108 (SQLite) / 082
// (Postgres) against a manufactured pre-upgrade state (TASK-3376).
//
// Before TASK-3376 a restricted member saw every system collection without a
// member_collection_access row. The migration lists them, so the upgrade
// changes nobody's access. Ordinary tests cannot reach it: migrations run
// before any member exists. Runs on whichever dialect testStore provides.
func TestSystemCollectionAccessBackfill(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Backfill 3376")
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	coll := func(slug string) *models.Collection {
		t.Helper()
		c, err := s.GetCollectionBySlug(ws.ID, slug)
		if err != nil || c == nil {
			t.Fatalf("no %s collection: %v", slug, err)
		}
		return c
	}
	conv, pb, tasks := coll("conventions"), coll("playbooks"), coll("tasks")

	// A system collection that was soft-deleted must not be listed.
	gone, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Old System", IsSystem: true})
	if err != nil {
		t.Fatalf("create system collection: %v", err)
	}
	if err := s.DeleteCollection(gone.ID, ""); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	member := func(email, mode string, ids []string) string {
		t.Helper()
		u := createTestUser(t, s, email, email, "password123")
		if err := s.AddWorkspaceMember(ws.ID, u.ID, "editor"); err != nil {
			t.Fatalf("add member: %v", err)
		}
		if err := s.SetMemberCollectionAccess(ws.ID, u.ID, mode, ids); err != nil {
			t.Fatalf("set access: %v", err)
		}
		return u.ID
	}
	restricted := member("restricted@example.com", "specific", []string{tasks.ID})
	// Already lists conventions: the backfill must not fail on the conflict.
	partly := member("partly@example.com", "specific", []string{tasks.ID, conv.ID})
	unrestricted := member("all@example.com", "all", nil)

	// Control: the visibility change is in effect, so the restricted member
	// does NOT see the system collections before the backfill. Without this
	// the assertions below would pass against the old implicit union.
	vis, err := s.VisibleCollectionIDs(ws.ID, restricted)
	if err != nil {
		t.Fatalf("visible: %v", err)
	}
	for _, id := range vis {
		if id == conv.ID || id == pb.ID {
			t.Fatalf("control failed: restricted member sees system collection %s before the backfill", id)
		}
	}

	runSystemAccessBackfill(t, s)
	runSystemAccessBackfill(t, s) // idempotent

	want := func(userID string, ids ...string) {
		t.Helper()
		got, err := s.GetMemberCollectionAccess(ws.ID, userID)
		if err != nil {
			t.Fatalf("get access: %v", err)
		}
		sort.Strings(got)
		sort.Strings(ids)
		if strings.Join(got, ",") != strings.Join(ids, ",") {
			t.Errorf("member %s access = %v, want %v", userID, got, ids)
		}
	}
	want(restricted, tasks.ID, conv.ID, pb.ID)
	want(partly, tasks.ID, conv.ID, pb.ID)
	want(unrestricted) // 'all' members get no rows

	vis, err = s.VisibleCollectionIDs(ws.ID, restricted)
	if err != nil {
		t.Fatalf("visible after: %v", err)
	}
	seen := map[string]bool{}
	for _, id := range vis {
		seen[id] = true
	}
	if !seen[conv.ID] || !seen[pb.ID] {
		t.Errorf("after backfill the restricted member should see conventions and playbooks; visible = %v", vis)
	}
	if seen[gone.ID] {
		t.Errorf("a soft-deleted system collection was listed")
	}
}

func runSystemAccessBackfill(t *testing.T, s *Store) {
	t.Helper()
	name, dir := "migrations/108_system_collection_access_backfill.sql", migrationsFS
	if s.dialect.Driver() == DriverPostgres {
		name = "pgmigrations/082_system_collection_access_backfill.sql"
		dir = pgMigrationsFS
	}
	raw, err := dir.ReadFile(name)
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}
	var sb strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		sb.WriteString(line + "\n")
	}
	stmt := strings.TrimSpace(sb.String())
	if !strings.HasPrefix(strings.ToUpper(stmt), "INSERT") {
		t.Fatalf("migration %s no longer starts with its INSERT; update this test", name)
	}
	if _, err := s.db.Exec(stmt); err != nil {
		t.Fatalf("backfill failed: %v\n%s", err, stmt)
	}
}
