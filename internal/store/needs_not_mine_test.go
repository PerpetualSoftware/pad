package store

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// "Not mine" (TASK-4643): the store half. Runs on both backends via
// testStore (PAD_TEST_POSTGRES_URL selects Postgres); the migration test runs
// each backend explicitly.

func (f needsFixture) notMine(t *testing.T, u *models.User, m *models.NeedsSnoozeMarker) *models.NeedsNotMine {
	t.Helper()
	nm, err := f.s.MarkNeedsNotMine(u.ID, f.ws.ID, f.item.ID, models.NeedsSourceWeb, m)
	if err != nil {
		t.Fatalf("mark not mine: %v", err)
	}
	if nm == nil {
		t.Fatal("mark not mine returned no row")
	}
	return nm
}

func TestNotMine_MarkerClamp(t *testing.T) {
	t.Parallel()
	f := newNeedsFixture(t)
	c := f.comment(t, f.other, "user", needsT0)
	if _, err := f.s.db.Exec(f.s.q(`UPDATE items SET assigned_user_id = ? WHERE id = ?`), f.me.ID, f.item.ID); err != nil {
		t.Fatal(err)
	}

	// No marker: the item as it is now.
	before := time.Now().UTC().Truncate(time.Second)
	nm := f.notMine(t, f.me, nil)
	got := nm.Marker
	if got.SeenCommentAt != needsT0.Format(time.RFC3339) || len(got.SeenCommentIDs) != 1 || got.SeenCommentIDs[0] != c.ID {
		t.Fatalf("nil marker comment state: %+v", got)
	}
	if got.SeenCollectionID != f.item.CollectionID || got.SeenAssigneeID != f.me.ID {
		t.Fatalf("nil marker collection/assignee: %+v", got)
	}
	if at, err := time.Parse(time.RFC3339, got.SeenAt); err != nil || at.Before(before) || at.After(time.Now()) {
		t.Fatalf("nil marker seen_at %q, want now", got.SeenAt)
	}
	if nm.Source != models.NeedsSourceWeb || nm.WorkspaceID != f.ws.ID || nm.ItemID != f.item.ID {
		t.Fatalf("row: %+v", nm)
	}

	// A future seen_at is held at now; a past one is kept; garbage is "".
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	got = f.notMine(t, f.me, &models.NeedsSnoozeMarker{SeenAt: future}).Marker
	if at, err := time.Parse(time.RFC3339, got.SeenAt); err != nil || at.After(time.Now()) {
		t.Fatalf("future seen_at stored as %q, want at or before now", got.SeenAt)
	}
	got = f.notMine(t, f.me, &models.NeedsSnoozeMarker{SeenAt: "2026-10-01T12:00:00.900+02:00"}).Marker
	if got.SeenAt != "2026-10-01T10:00:00Z" {
		t.Fatalf("past seen_at stored as %q", got.SeenAt)
	}
	if got = f.notMine(t, f.me, &models.NeedsSnoozeMarker{SeenAt: "yesterday"}).Marker; got.SeenAt != "" {
		t.Fatalf("unparseable seen_at stored as %q, want empty", got.SeenAt)
	}

	// A stale collection or assignee is stored as "", a current one kept.
	got = f.notMine(t, f.me, &models.NeedsSnoozeMarker{SeenCollectionID: "some-other-collection", SeenAssigneeID: f.other.ID}).Marker
	if got.SeenCollectionID != "" || got.SeenAssigneeID != "" {
		t.Fatalf("stale collection/assignee kept: %+v", got)
	}
	got = f.notMine(t, f.me, &models.NeedsSnoozeMarker{SeenCollectionID: f.item.CollectionID, SeenAssigneeID: f.me.ID}).Marker
	if got.SeenCollectionID != f.item.CollectionID || got.SeenAssigneeID != f.me.ID {
		t.Fatalf("current collection/assignee dropped: %+v", got)
	}

	// The dismissal fields are clamped as a snooze clamps them.
	got = f.notMine(t, f.me, &models.NeedsSnoozeMarker{SeenCommentAt: "2099-01-01T00:00:00Z", SeenCommentIDs: []string{c.ID, "forged"}, SeenTransitionSeq: 1 << 40}).Marker
	if got.SeenCommentAt != needsT0.Format(time.RFC3339) || len(got.SeenCommentIDs) != 1 || got.SeenCommentIDs[0] != c.ID || got.SeenTransitionSeq > 1<<20 {
		t.Fatalf("dismissal fields not clamped: %+v", got)
	}

	if _, err := f.s.MarkNeedsNotMine(f.me.ID, f.ws.ID, f.item.ID, "mcp-body", nil); !errors.Is(err, ErrInvalidNeedsSource) {
		t.Fatalf("unknown source: %v", err)
	}
}

// Writing either hide state deletes the other (TASK-4643).
func TestNotMine_StoreReplacesSnooze(t *testing.T) {
	t.Parallel()
	f := newNeedsFixture(t)
	f.snooze(t, f.me, models.NeedsSnoozeDismiss, time.Time{}, nil)
	f.notMine(t, f.me, nil)
	if sn, err := f.s.GetNeedsSnooze(f.me.ID, f.item.ID, time.Now()); err != nil || sn != nil {
		t.Fatalf("snooze survived a not-mine: %+v %v", sn, err)
	}
	f.snooze(t, f.me, models.NeedsSnoozeDefer, time.Now().Add(time.Hour), nil)
	if nm, err := f.s.GetNeedsNotMine(f.me.ID, f.item.ID); err != nil || nm != nil {
		t.Fatalf("not-mine survived a snooze: %+v %v", nm, err)
	}
	// Another person's states are untouched by mine.
	f.snooze(t, f.other, models.NeedsSnoozeDismiss, time.Time{}, nil)
	f.notMine(t, f.me, nil)
	if sn, err := f.s.GetNeedsSnooze(f.other.ID, f.item.ID, time.Now()); err != nil || sn == nil {
		t.Fatalf("my not-mine cleared another person's snooze: %v", err)
	}
	if ok, err := f.s.UnmarkNeedsNotMine(f.me.ID, f.item.ID); err != nil || !ok {
		t.Fatalf("unmark: %v %v", ok, err)
	}
	if ok, err := f.s.UnmarkNeedsNotMine(f.me.ID, f.item.ID); err != nil || ok {
		t.Fatalf("second unmark: %v %v", ok, err)
	}
}

// The batch read: scoped to the live item in the workspace, and carrying only
// the transitions after the marker's seq.
func TestNotMine_ForItemsTransitionsAfterMarker(t *testing.T) {
	t.Parallel()
	f := newNeedsFixture(t)
	f.setStatus(t, "doing")
	f.notMine(t, f.me, nil)
	got, err := f.s.NeedsNotMineForItems(f.me.ID, f.ws.ID, []string{f.item.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got[f.item.ID] == nil || len(got[f.item.ID].Transitions) != 0 {
		t.Fatalf("fresh mark: %+v", got[f.item.ID])
	}
	f.setStatus(t, "needs-answer")
	got, err = f.s.NeedsNotMineForItems(f.me.ID, f.ws.ID, []string{f.item.ID})
	if err != nil {
		t.Fatal(err)
	}
	if trs := got[f.item.ID].Transitions; len(trs) != 1 || trs[0].ToStatus != "needs-answer" || trs[0].FieldKey != "status" {
		t.Fatalf("transitions after the marker: %+v", trs)
	}
	if other, err := f.s.NeedsNotMineForItems(f.other.ID, f.ws.ID, []string{f.item.ID}); err != nil || len(other) != 0 {
		t.Fatalf("another person reads my mark: %v %v", other, err)
	}
	if elsewhere, err := f.s.NeedsNotMineForItems(f.me.ID, "another-workspace", []string{f.item.ID}); err != nil || len(elsewhere) != 0 {
		t.Fatalf("read outside the item's workspace: %v %v", elsewhere, err)
	}
	if _, err := f.s.db.Exec(f.s.q(`UPDATE items SET deleted_at = ? WHERE id = ?`), time.Now().UTC().Format(time.RFC3339), f.item.ID); err != nil {
		t.Fatal(err)
	}
	if gone, err := f.s.NeedsNotMineForItems(f.me.ID, f.ws.ID, []string{f.item.ID}); err != nil || len(gone) != 0 {
		t.Fatalf("a deleted item's mark is read: %v %v", gone, err)
	}
}

// Migration 186 / Postgres 145: the table, its keys, CHECK and cascades.
func TestNotMineMigration(t *testing.T) {
	for _, pg := range []bool{false, true} {
		t.Run(map[bool]string{false: "sqlite", true: "postgres"}[pg], func(t *testing.T) {
			var s *Store
			if pg {
				url := os.Getenv("PAD_TEST_POSTGRES_URL")
				if url == "" {
					t.Skip("PAD_TEST_POSTGRES_URL not set")
				}
				s = testStorePostgres(t, url)
			} else {
				s = testStoreSQLite(t)
			}
			// Re-running the migration is a no-op.
			fs, path := migrationsFS, "migrations/186_item_needs_not_mine.sql"
			if pg {
				fs, path = pgMigrationsFS, "pgmigrations/145_item_needs_not_mine.sql"
			}
			body, err := fs.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if pg {
				err = applyPostgresMigration(s.db, "999_not_mine_probe.sql", string(body))
			} else {
				err = applySQLiteMigration(s.db, "999_not_mine_probe.sql", string(body))
			}
			if err != nil {
				t.Fatalf("re-apply: %v", err)
			}

			ws := createTestWorkspace(t, s, "Not mine migration")
			col := createTestCollection(t, s, ws.ID, "Tasks")
			item := createTestItem(t, s, ws.ID, col.ID, "One", "")
			me := needsUser(t, s, "nm-me@test.com")
			ins := func(u *models.User, it *models.Item, source string) error {
				_, err := s.db.Exec(s.q(`INSERT INTO item_needs_not_mine (user_id, item_id, workspace_id, source, created_at) VALUES (?, ?, ?, ?, ?)`),
					u.ID, it.ID, ws.ID, source, "2026-11-18T00:00:00Z")
				return err
			}
			if err := ins(me, item, "web"); err != nil {
				t.Fatal(err)
			}
			if err := ins(me, item, "cli"); err == nil {
				t.Fatal("a second row for one (user, item) was accepted")
			}
			other := createTestItem(t, s, ws.ID, col.ID, "Two", "")
			if err := ins(me, other, "body"); err == nil {
				t.Fatal("an unknown source was accepted")
			}
			var ids, commentAt, coll, assignee, at string
			var seq int64
			if err := s.db.QueryRow(s.q(`SELECT seen_comment_ids, seen_comment_at, seen_transition_seq, seen_collection_id, seen_assignee_id, seen_at FROM item_needs_not_mine WHERE item_id = ?`), item.ID).
				Scan(&ids, &commentAt, &seq, &coll, &assignee, &at); err != nil {
				t.Fatal(err)
			}
			if ids != "[]" || commentAt != "" || seq != 0 || coll != "" || assignee != "" || at != "" {
				t.Fatalf("defaults: ids %q comment_at %q seq %d coll %q assignee %q at %q", ids, commentAt, seq, coll, assignee, at)
			}
			idx := `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`
			if pg {
				idx = `SELECT COUNT(*) FROM pg_indexes WHERE indexname = ?`
			}
			if s.countRows(t, idx, "idx_item_needs_not_mine_item") != 1 {
				t.Fatal("no item_id index")
			}

			// Each foreign key cascades.
			if err := ins(me, other, "cli"); err != nil {
				t.Fatal(err)
			}
			s.mustExec(t, `DELETE FROM items WHERE id = ?`, other.ID)
			if s.countRows(t, `SELECT COUNT(*) FROM item_needs_not_mine WHERE item_id = ?`, other.ID) != 0 {
				t.Fatal("item delete did not cascade")
			}
			you := needsUser(t, s, "nm-you@test.com")
			if err := ins(you, item, "web"); err != nil {
				t.Fatal(err)
			}
			s.mustExec(t, `DELETE FROM users WHERE id = ?`, you.ID)
			if s.countRows(t, `SELECT COUNT(*) FROM item_needs_not_mine WHERE user_id = ?`, you.ID) != 0 {
				t.Fatal("user delete did not cascade")
			}
			// The workspace foreign key cascades too; a workspace delete is
			// blocked here by other tables, so read the key's rule (the purge
			// tests delete through it).
			fk := `SELECT COUNT(*) FROM pragma_foreign_key_list('item_needs_not_mine') WHERE "table" = 'workspaces' AND on_delete = 'CASCADE'`
			if pg {
				fk = `SELECT COUNT(*) FROM information_schema.referential_constraints rc
					JOIN information_schema.table_constraints tc ON tc.constraint_name = rc.constraint_name
					JOIN information_schema.constraint_column_usage cu ON cu.constraint_name = rc.constraint_name
					WHERE tc.table_name = 'item_needs_not_mine' AND cu.table_name = 'workspaces' AND rc.delete_rule = 'CASCADE'`
			}
			if s.countRows(t, fk) != 1 {
				t.Fatal("the workspace foreign key does not cascade")
			}
		})
	}
}
