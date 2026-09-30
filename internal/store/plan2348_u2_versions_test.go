package store

import (
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-2348 U2: a version row carries who wrote it, the line counts of the
// change its write recorded, and whether it was written by create; the diff
// pair for a row is the change that row records.

type u2Fixture struct {
	s      *Store
	wsID   string
	collID string
	user   *models.User
}

func newU2Fixture(t *testing.T) u2Fixture {
	t.Helper()
	s := testStore(t)
	user, err := s.CreateUser(models.UserCreate{Email: "u2-" + newID()[:8] + "@test.com", Name: "Dana Writer", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "U2 " + newID()[:8]})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	coll := createTestCollection(t, s, ws.ID, "Notes")
	return u2Fixture{s: s, wsID: ws.ID, collID: coll.ID, user: user}
}

func (f u2Fixture) update(t *testing.T, itemID, body, actor, source string) *models.Item {
	t.Helper()
	it, err := f.s.UpdateItem(itemID, models.ItemUpdate{Content: &body, LastModifiedBy: actor, VersionSource: source, ActorUserID: f.user.ID})
	if err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	return it
}

func intIs(p *int, want int) bool { return p != nil && *p == want }

func TestItemVersions_CarryWriterCountsAndCreateMarker(t *testing.T) {
	t.Parallel()
	f := newU2Fixture(t)
	item, err := f.s.CreateItem(f.wsID, f.collID, models.ItemCreate{
		Title: "Doc", Content: "a\nb\n", CreatedBy: "user", Source: "web", ActorUserID: f.user.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	// A different actor, so the throttle writes a row.
	f.update(t, item.ID, "a\nB\nc\n", "agent", "cli")

	vs, err := f.s.ListItemVersions(item.ID)
	if err != nil {
		t.Fatalf("ListItemVersions: %v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("precondition: want 2 version rows, got %d", len(vs))
	}
	upd, crt := vs[0], vs[1]

	if !crt.IsCreate || upd.IsCreate {
		t.Fatalf("is_create: create row %v, update row %v", crt.IsCreate, upd.IsCreate)
	}
	if !intIs(crt.LinesAdded, 2) || !intIs(crt.LinesRemoved, 0) {
		t.Errorf("create row counts +%v −%v, want +2 −0", crt.LinesAdded, crt.LinesRemoved)
	}
	// a\nb\n → a\nB\nc\n: b replaced by B, c added.
	if !intIs(upd.LinesAdded, 2) || !intIs(upd.LinesRemoved, 1) {
		t.Errorf("update row counts +%v −%v, want +2 −1", upd.LinesAdded, upd.LinesRemoved)
	}
	for _, v := range vs {
		if v.UserID != f.user.ID || v.ActorName != "Dana Writer" {
			t.Errorf("row %s: user %q name %q, want %q / Dana Writer", v.ID, v.UserID, v.ActorName, f.user.ID)
		}
	}
}

func TestItemVersions_ThrottledBodyEditIsMarked(t *testing.T) {
	t.Parallel()
	f := newU2Fixture(t)
	item := createTestItem(t, f.s, f.wsID, f.collID, "Doc", "one\n")
	first := f.update(t, item.ID, "two\n", "agent", "cli")
	if first.BodyEditedWithoutVersion {
		t.Fatal("precondition: the first agent edit writes a version row and must not be marked")
	}
	second := f.update(t, item.ID, "three\n", "agent", "cli")
	if !second.BodyEditedWithoutVersion {
		t.Fatal("a body edit the throttle wrote no row for must be marked")
	}
	same := f.update(t, item.ID, "three\n", "agent", "cli")
	if same.BodyEditedWithoutVersion {
		t.Fatal("CONTROL: an update that leaves the body unchanged is not a body edit")
	}
}

func TestGetItemVersionDiff_PairsEachRowWithItsOwnChange(t *testing.T) {
	t.Parallel()
	f := newU2Fixture(t)
	item, err := f.s.CreateItem(f.wsID, f.collID, models.ItemCreate{Title: "Doc", Content: "v1\n", CreatedBy: "user", Source: "web"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	f.update(t, item.ID, "v2\n", "agent", "cli") // row: before v1
	f.update(t, item.ID, "v3\n", "agent", "mcp") // row: before v2
	vs, err := f.s.ListItemVersions(item.ID)
	if err != nil || len(vs) != 3 {
		t.Fatalf("precondition: want 3 rows, got %d (%v)", len(vs), err)
	}
	newest, middle, create := vs[0], vs[1], vs[2]

	for _, c := range []struct {
		name, id, before, after string
	}{
		{"create row: empty → as created", create.ID, "", "v1\n"},
		{"middle row: its body → the next row's body", middle.ID, "v1\n", "v2\n"},
		{"newest row: its body → current content", newest.ID, "v2\n", "v3\n"},
	} {
		d, err := f.s.GetItemVersionDiff(item.ID, c.id, "v3\n")
		if err != nil || d == nil {
			t.Fatalf("%s: GetItemVersionDiff: %v %v", c.name, d, err)
		}
		if d.Before != c.before || d.After != c.after {
			t.Errorf("%s: %q → %q, want %q → %q", c.name, d.Before, d.After, c.before, c.after)
		}
	}
	if d, err := f.s.GetItemVersionDiff(item.ID, "no-such-version", "v3\n"); err != nil || d != nil {
		t.Errorf("CONTROL: an unknown version answers nil, got %v %v", d, err)
	}
}

// The migration's backfill marks a legacy create row: the first row sharing
// the item's created_at. Run against rows whose marker was cleared.
func TestMigration103_BackfillsTheCreateMarker(t *testing.T) {
	t.Parallel()
	f := newU2Fixture(t)
	created := createTestItem(t, f.s, f.wsID, f.collID, "Doc", "x\n")
	f.update(t, created.ID, "y\n", "agent", "cli")
	if _, err := f.s.db.Exec(f.s.q(`UPDATE item_versions SET is_create = ?`), f.s.dialect.BoolToInt(false)); err != nil {
		t.Fatalf("clear: %v", err)
	}

	file := "migrations/103_item_version_history_data.sql"
	if f.s.dialect.Driver() == DriverPostgres {
		file = "pgmigrations/077_item_version_history_data.sql"
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	backfill := raw[strings.Index(string(raw), "UPDATE item_versions"):]
	if _, err := f.s.db.Exec(string(backfill)); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	vs, _ := f.s.ListItemVersions(created.ID)
	if len(vs) != 2 || !vs[1].IsCreate || vs[0].IsCreate {
		t.Fatalf("after backfill: rows %d, create row marked %v, update row marked %v", len(vs), len(vs) == 2 && vs[1].IsCreate, len(vs) > 0 && vs[0].IsCreate)
	}
}
