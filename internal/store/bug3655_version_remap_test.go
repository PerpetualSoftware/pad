package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/diff"
)

// BUG-3655 (codex r1 on the fix): a patch in an item's history that does not
// apply must not leave the rest of the chain on the source ids. A full row
// restarts the walk, so everything below it is still re-encoded, and the
// unresolvable patch has its own text remapped.
func TestBUG3655_BrokenPatchDoesNotStrandTheRestOfTheChain(t *testing.T) {
	oldID, newID := "00000000-0000-0000-0000-0000000000a1", "00000000-0000-0000-0000-0000000000b2"
	ref := "![chart](pad-attachment:" + oldID + ")"
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Broken chain")
	col := createTestCollection(t, s, ws.ID, "Docs")
	item := createTestItem(t, s, ws.ID, col.ID, "Doc", "The current body names no attachment.")

	// Newest first: a patch whose context is nowhere in the current body, so
	// it fails to apply but carries the id whole; a full snapshot with the
	// id; a patch below it, made against that snapshot; the create row.
	full := "A full snapshot.\n\n" + ref
	broken := "@@ -1,12 +1,60 @@\n-Nowhere text\n+" + ref + "\n"
	rows := []struct {
		id, content string
		isDiff      bool
		at          string
	}{
		{"v4", broken, true, "2026-10-04T00:00:00Z"},
		{"v3", full, false, "2026-10-03T00:00:00Z"},
		{"v2", diff.CreateReversePatch(full+" More.", full), true, "2026-10-02T00:00:00Z"},
		{"v1", "A full snapshot.\n\n" + ref + " More.", false, "2026-10-01T00:00:00Z"},
	}
	for i, r := range rows {
		if _, err := s.db.Exec(s.q(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq) VALUES (?, ?, ?, ?, ?, ?)`),
			r.id, item.ID, r.content, s.dialect.BoolToInt(r.isDiff), r.at, len(rows)-i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := diff.ApplyPatch(item.Content, broken); err == nil {
		t.Fatal("instrument: the newest patch must fail to apply")
	}

	if err := s.RemapAttachmentReferencesInWorkspace(ws.ID, map[string]string{oldID: newID}); err != nil {
		t.Fatal(err)
	}
	stored := map[string]string{}
	vrows, err := s.db.Query(s.q(`SELECT id, content FROM item_versions WHERE item_id = ?`), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	for vrows.Next() {
		var id, c string
		if err := vrows.Scan(&id, &c); err != nil {
			t.Fatal(err)
		}
		stored[id] = c
	}
	vrows.Close()
	for id, c := range stored {
		if strings.Contains(c, oldID) {
			t.Errorf("version %s still carries the source id: %q", id, c)
		}
	}
	if !strings.Contains(stored["v4"], newID) {
		t.Errorf("the unresolvable patch's own text was not remapped: %q", stored["v4"])
	}
	// The rows below the full snapshot still resolve, now with the new id.
	got, err := diff.ApplyPatch(stored["v3"], stored["v2"])
	if err != nil || got != "A full snapshot.\n\n![chart](pad-attachment:"+newID+") More." {
		t.Errorf("v2 resolves to %q (%v), want the body with the new id", got, err)
	}
}
