package store

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3751: Postgres migration 054 backfills version_seq for rows that
// predate it. Same-second versions are ordered by id, never by ctid, which an
// UPDATE moves. This reruns 054's backfill on rows whose physical (ctid)
// order differs from their id order and checks the numbering follows id.
func TestBUG3751_Migration054OrdersSameSecondTiesByID(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("054 is the Postgres migration; ctid exists only there")
	}
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-3751")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Ties", Content: "c0", Source: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"c1", "c2", "c3"} {
		c := c
		if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &c, LastModifiedBy: "user", Source: "web", ForceVersion: true}); err != nil {
			t.Fatal(err)
		}
	}

	db := s.DB()
	if _, err := db.Exec(`UPDATE item_versions SET created_at = '2026-01-01T00:00:00Z', version_seq = 0 WHERE item_id = $1`, item.ID); err != nil {
		t.Fatal(err)
	}
	ids := versionIDs(t, s, item.ID, "id")
	if len(ids) < 3 {
		t.Fatalf("%d versions; want at least 3", len(ids))
	}
	// Move the lowest id to the physical end, so ctid order and id order
	// disagree on which comes first.
	if _, err := db.Exec(`UPDATE item_versions SET version_seq = 0 WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if byCtid := versionIDs(t, s, item.ID, "ctid"); byCtid[0] == ids[0] {
		t.Fatalf("precondition: ctid order %v still starts with the lowest id %s", byCtid, ids[0])
	}

	data, err := pgMigrationsFS.ReadFile("pgmigrations/054_item_version_seq.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(data)
	start := strings.Index(sql, "UPDATE item_versions AS v")
	end := strings.Index(sql, "WHERE v.id = sub.id;")
	if start < 0 || end < start {
		t.Fatal("054's backfill UPDATE not found")
	}
	if _, err := db.Exec(sql[start : end+len("WHERE v.id = sub.id")]); err != nil {
		t.Fatalf("054 backfill: %v", err)
	}

	bySeq := versionIDs(t, s, item.ID, "version_seq")
	if strings.Join(bySeq, ",") != strings.Join(ids, ",") {
		t.Fatalf("054 numbered %v; want id order %v", bySeq, ids)
	}
}

// versionIDs answers the item's version ids ordered by the given column.
func versionIDs(t *testing.T, s *Store, itemID, orderBy string) []string {
	t.Helper()
	rows, err := s.DB().Query(`SELECT id FROM item_versions WHERE item_id = $1 ORDER BY `+orderBy, itemID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if orderBy == "id" && !sort.StringsAreSorted(ids) {
		// Postgres may collate text differently from Go; the migration's
		// ORDER BY id is what counts, so trust the database's order.
		t.Logf("database id order %v differs from Go's byte order", ids)
	}
	return ids
}
