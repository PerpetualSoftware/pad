package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3693: a rename whose new title is reference-shaped ("TASK-1") made the
// cascade write [[TASK-1]] into every linker, and the index and the renderer
// resolve a reference before a title, so the links moved to item 1.
func TestBUG3693_RenameToAReferenceShapedTitleKeepsItsLinks(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Refs")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	one := createTestItem(t, s, ws.ID, col.ID, "First", "first")
	two := createTestItem(t, s, ws.ID, col.ID, "Old title", "second")
	one, _ = s.GetItem(one.ID)
	if one.Ref == "" {
		t.Fatal("instrument: item one has no ref")
	}
	doc := createTestItem(t, s, ws.ID, col.ID, "Linker", "See [[Old title]] and [[Old title|the second one]].")

	title := one.Ref // e.g. "TASK-1": a reference to a DIFFERENT item
	if _, err := s.UpdateItem(two.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.db.Query(s.q(`SELECT COALESCE(target_item_id, ''), position FROM item_wiki_links WHERE source_item_id = ? ORDER BY position`), doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var target string
		var pos int
		if err := rows.Scan(&target, &pos); err != nil {
			t.Fatal(err)
		}
		n++
		if target != two.ID {
			after, _ := s.GetItem(doc.ID)
			t.Errorf("link at %d now targets %q, want the renamed item %q (body %q)", pos, target, two.ID, after.Content)
		}
	}
	if n != 2 {
		t.Fatalf("instrument: %d indexed links, want 2", n)
	}
	two, _ = s.GetItem(two.ID)
	after, _ := s.GetItem(doc.ID)
	if want := "See [[" + two.Ref + "|" + title + "]] and [[" + two.Ref + "|the second one]]."; after.Content != want {
		t.Errorf("body = %q, want %q", after.Content, want)
	}
}
