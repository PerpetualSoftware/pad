package store

import "testing"

// IsItemSlugConflict recognises exactly the items (workspace_id, slug)
// violation, on both dialects, and not another unique index on items.
func TestTASK3390_IsItemSlugConflict(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Slugs")
	col := createTestCollection(t, s, ws.ID, "Tickets")
	it := createTestItem(t, s, ws.ID, col.ID, "Taken", "")
	ts := now()
	ins := func(slug string, number int) error {
		_, err := s.db.Exec(s.q(`INSERT INTO items (id, workspace_id, collection_id, title, slug, content, fields, tags, created_by, last_modified_by, source, item_number, created_at, updated_at)
			VALUES (?, ?, ?, 'x', ?, '', '{}', '[]', 'user', 'user', 'web', ?, ?, ?)`), newID(), ws.ID, col.ID, slug, number, ts, ts)
		return err
	}
	if err := ins(it.Slug, 9001); err == nil || !IsItemSlugConflict(err) {
		t.Fatalf("slug collision: %v (IsItemSlugConflict=%v)", err, IsItemSlugConflict(err))
	}
	if err := ins("fresh-slug", *it.ItemNumber); err == nil || IsItemSlugConflict(err) {
		t.Fatalf("item_number collision must NOT read as a slug conflict: %v", err)
	}
}
