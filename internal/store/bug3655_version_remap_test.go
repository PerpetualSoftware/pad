package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/diff"
)

// BUG-3655 (codex r1 and r2 on the fix): a patch in an item's history that
// does not apply must not leave the rest of the chain on the source ids, and
// every row must end up as the reader's walk (ListItemVersionsResolvedPage)
// resolves it: that walk skips a failed patch and resolves the row below it
// against the same successor.
func TestBUG3655_BrokenPatchDoesNotStrandTheRestOfTheChain(t *testing.T) {
	oldID, newID := "00000000-0000-0000-0000-0000000000a1", "00000000-0000-0000-0000-0000000000b2"
	ref := func(id string) string { return "![chart](pad-attachment:" + id + ")" }
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Broken chain")
	col := createTestCollection(t, s, ws.ID, "Docs")
	current := "Draft three, with a table:\n\n| a | b |\n| - | - |\n| 1 | 2 |"
	item := createTestItem(t, s, ws.ID, col.ID, "Doc", current)

	// Newest first:
	//   v5  a patch whose context is nowhere in the body: fails, carries the id
	//   v4  a patch against the CURRENT body, which the walk resolves past v5;
	//       a character-level diff, so the id is not whole in its text and
	//       only re-diffing the resolved body can remap it
	//   v3  a full snapshot with the id
	//   v2  a patch against v3
	//   v1  the create row, in full
	broken := "@@ -1,12 +1,60 @@\n-Nowhere text\n+" + ref(oldID) + "\n"
	b4 := "Draft three, with a table and a picture:\n\n" + ref(oldID)
	full := "A full snapshot.\n\n" + ref(oldID)
	b2 := full + " More."
	rows := []struct {
		id, content string
		isDiff      bool
	}{
		{"v5", broken, true},
		{"v4", diff.CreateReversePatch(b4, current), true},
		{"v3", full, false},
		{"v2", diff.CreateReversePatch(b2, full), true},
		{"v1", b2, false},
	}
	for i, r := range rows {
		if _, err := s.db.Exec(s.q(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq) VALUES (?, ?, ?, ?, ?, ?)`),
			r.id, item.ID, r.content, s.dialect.BoolToInt(r.isDiff), "2026-10-0"+string(rune('5'-i))+"T00:00:00Z", len(rows)-i); err != nil {
			t.Fatal(err)
		}
	}
	resolve := func() map[string]string {
		t.Helper()
		vs, err := s.ListItemVersionsResolved(item.ID, current)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, v := range vs {
			out[v.ID] = v.Content
		}
		return out
	}
	if strings.Contains(rows[1].content, oldID) {
		t.Fatalf("instrument: v4's patch must not carry the id whole: %q", rows[1].content)
	}
	before := resolve()
	if !isPatchError(before["v5"]) || before["v4"] != b4 || before["v2"] != b2 {
		t.Fatalf("instrument: v5 must fail and v4, v2 resolve before the remap: %q", before)
	}

	if err := s.RemapAttachmentReferencesInWorkspace(ws.ID, map[string]string{oldID: newID}); err != nil {
		t.Fatal(err)
	}
	after := resolve()
	swap := func(b string) string { return strings.ReplaceAll(b, oldID, newID) }
	for id, want := range map[string]string{"v4": swap(b4), "v3": swap(full), "v2": swap(b2), "v1": swap(b2)} {
		if after[id] != want {
			t.Errorf("%s resolves to %q after the remap, want %q", id, after[id], want)
		}
	}
	if !isPatchError(after["v5"]) {
		t.Errorf("v5 should still fail to apply, got %q", after["v5"])
	}
	var raw string
	if err := s.db.QueryRow(s.q(`SELECT content FROM item_versions WHERE id = 'v5'`)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, oldID) || !strings.Contains(raw, newID) {
		t.Errorf("the unresolvable patch's own text was not remapped: %q", raw)
	}
}
