package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/links"
)

// BUG-3750: converting legacy per-collection numbering to per-workspace
// numbering changed item numbers and nothing else, so a stored [[IDEA-1]]
// whose item became IDEA-2 resolved, through GetItemByRef's workspace-number
// fallback, to TASK-1.
func TestBUG3750_RenumberingKeepsStoredReferencesOnTheirItems(t *testing.T) {
	path := bug3664Path(t)
	db := openRaw(t, path)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	names, err := readMigrationNames(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "056" {
			break
		}
		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := applySQLiteMigration(db, name, string(data)); err != nil {
			t.Fatalf("pre-056 migration %s: %v", name, err)
		}
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX idx_items_collection_number ON items(collection_id, item_number)`); err != nil {
		t.Fatal(err)
	}
	seedTwoNumberOnes(t, db) // TASK-1 (i1), IDEA-1 (i2): i2 becomes IDEA-2
	body := "Links: [[IDEA-1]], [[IDEA-1|the idea]], and [[TASK-1]]."
	if _, err := db.Exec(`INSERT INTO items (id, workspace_id, collection_id, title, slug, item_number, content, created_at, updated_at)
		VALUES ('i3', 'w1', 'c1', 'Doc', 'doc', 2, ?, '2026-01-01T00:00:03Z', '2026-01-01T00:00:03Z')`, body); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO comments (id, item_id, workspace_id, body, created_at, updated_at)
		VALUES ('cm1', 'i3', 'w1', 'Comment on [[IDEA-1]].', '2026-01-01T00:00:04Z', '2026-01-01T00:00:04Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertWorkspaceNumbering(t, s)

	var content string
	if err := s.db.QueryRow(`SELECT content FROM items WHERE id = 'i3'`).Scan(&content); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"IDEA": "i2", "TASK": "i1"}
	n := 0
	for _, l := range links.ExtractWikiLinks(content) {
		if l.Kind != links.WikiLinkKindRef {
			continue
		}
		n++
		prefix, number, ok := splitRef(l.Ref)
		if !ok {
			t.Fatalf("instrument: %q is not a ref", l.Ref)
		}
		item, err := s.GetItemByRef("w1", prefix, number)
		if err != nil || item == nil {
			t.Fatalf("resolve %s: %v", l.Ref, err)
		}
		if item.ID != want[prefix] {
			t.Errorf("%s in %q resolves to %s, want %s", l.Ref, content, item.ID, want[prefix])
		}
	}
	if n != 3 {
		t.Fatalf("instrument: %d reference links in %q, want 3", n, content)
	}
	var comment string
	if err := s.db.QueryRow(`SELECT body FROM comments WHERE id = 'cm1'`).Scan(&comment); err != nil {
		t.Fatal(err)
	}
	if comment != "Comment on [[IDEA-2]]." {
		t.Errorf("comment = %q, want its reference moved to IDEA-2", comment)
	}
	if !strings.Contains(content, "|the idea]]") {
		t.Errorf("the alias was lost: %q", content)
	}
}
