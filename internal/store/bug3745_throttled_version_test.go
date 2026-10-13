package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3745: a throttled write (same actor and source within the window)
// replaces the body without a new version. The newest version is a reverse
// patch made against the body it replaced, so unless that patch is rebased
// onto the new body, History resolves it against content it was never made
// from, and the old version picks up text that did not exist yet.
func TestBUG3745_ThrottledWriteKeepsTheNewestVersionExact(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Throttle")
	col := createTestCollection(t, s, ws.ID, "Docs")

	var paras []string
	for i := 0; i < 40; i++ {
		paras = append(paras, "Paragraph about the deployment pipeline and its many careful steps, number "+strings.Repeat("x", i%7)+".")
	}
	a := strings.Join(paras, "\n\n")
	b := strings.Replace(a, "Paragraph about", "Notes about", 1) // a small edit early in a long doc
	c := b + "\n\nA paragraph appended later, which version A never had."

	item := createTestItem(t, s, ws.ID, col.ID, "Long doc", a)
	write := func(body string) *models.Item {
		t.Helper()
		out, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &body, LastModifiedBy: "alice", Source: "web"})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := write(b)
	if first.BodyEditedWithoutVersion {
		t.Fatal("instrument: the first edit by alice/web must write a version")
	}
	var isDiff bool
	if err := s.db.QueryRow(s.q(`SELECT is_diff FROM item_versions WHERE item_id = ? ORDER BY created_at DESC, version_seq DESC LIMIT 1`), item.ID).Scan(&isDiff); err != nil || !isDiff {
		t.Fatalf("instrument: the newest version must be a reverse patch (is_diff=%v, %v)", isDiff, err)
	}
	second := write(c)
	if !second.BodyEditedWithoutVersion {
		t.Fatal("instrument: the second edit inside the window must be throttled")
	}

	versions, err := s.ListItemVersionsResolved(item.ID, c)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range versions {
		if strings.Contains(v.Content, "appended later") && v.Content != c {
			t.Errorf("a version holds text written after it: %q...", v.Content[:60])
		}
		if v.Content == a {
			found = true
		}
	}
	if !found {
		t.Fatalf("no version resolves to exactly the body before alice's edits; got %d versions", len(versions))
	}
	// Its line counts describe the change it now covers: a to c.
	wantAdded, wantRemoved := diff.LineCounts(a, c)
	var added, removed int
	if err := s.db.QueryRow(s.q(`SELECT lines_added, lines_removed FROM item_versions WHERE item_id = ? ORDER BY created_at DESC, version_seq DESC LIMIT 1`), item.ID).Scan(&added, &removed); err != nil {
		t.Fatal(err)
	}
	if added != wantAdded || removed != wantRemoved {
		t.Errorf("newest version counts +%d -%d, want +%d -%d (a to c)", added, removed, wantAdded, wantRemoved)
	}
}

// The property: after any run of throttled writes, every version in the
// reverse chain still resolves to exactly the body it recorded.
func TestBUG3745_EveryVersionResolvesExactlyAfterThrottledWrites(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Chain")
	col := createTestCollection(t, s, ws.ID, "Docs")

	base := make([]string, 30)
	for i := range base {
		base[i] = "Line " + strings.Repeat("ab", i%5) + " of a long shared document about release planning."
	}
	body := func(edit func([]string) []string) string {
		cp := append([]string(nil), base...)
		return strings.Join(edit(cp), "\n")
	}
	b0 := body(func(l []string) []string { return l })
	b1 := body(func(l []string) []string { l[2] = "bob rewrote line two"; return l })
	b2 := body(func(l []string) []string { l[2] = "bob rewrote line two"; l[5] = "alice edited line five"; return l })
	b3 := b2 + "\nalice appended a line"
	b4 := "alice prepended a line\n" + b3
	b5 := strings.Replace(b4, "release planning", "launch planning", 1)

	item := createTestItem(t, s, ws.ID, col.ID, "Chain doc", b0)
	write := func(who, body string) bool {
		t.Helper()
		out, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &body, LastModifiedBy: who, Source: "web"})
		if err != nil {
			t.Fatal(err)
		}
		return out.BodyEditedWithoutVersion
	}
	if write("bob", b1) || write("alice", b2) {
		t.Fatal("instrument: a change of writer must write a version")
	}
	for _, b := range []string{b3, b4, b5} {
		if !write("alice", b) {
			t.Fatal("instrument: alice's later edits inside the window must be throttled")
		}
	}

	versions, err := s.ListItemVersionsResolved(item.ID, b5)
	if err != nil {
		t.Fatal(err)
	}
	// Newest first: alice's version holds b1 (the body before her first
	// edit), bob's holds b0, then the create.
	want := []string{b1, b0}
	if len(versions) < len(want) {
		t.Fatalf("got %d versions, want at least %d", len(versions), len(want))
	}
	for i, w := range want {
		if versions[i].Content != w {
			t.Errorf("version %d resolves to %q..., want %q...", i, versions[i].Content[:50], w[:50])
		}
	}
}

// The two system rewrites of items.content that write no version, the wiki
// title cascade and the attachment remap, also leave the newest reverse
// patch made against a body that is gone (BUG-3745 class sweep). In both,
// the rewritten text sits far from the patch's hunk, so an unrebased patch
// still applies and carries the rewrite into the old version.
func TestBUG3745_SystemRewritesKeepTheNewestVersionExact(t *testing.T) {
	long := func(head, tail string) string {
		lines := []string{head}
		for i := 0; i < 30; i++ {
			lines = append(lines, "Filler line "+strings.Repeat("q", i%6)+" about quarterly planning and staffing.")
		}
		return strings.Join(append(lines, tail), "\n")
	}
	setup := func(t *testing.T, head string) (*Store, *models.Workspace, *models.Collection, *models.Item, string) {
		t.Helper()
		s := testStore(t)
		ws := createTestWorkspace(t, s, "Rewrites")
		col := createTestCollection(t, s, ws.ID, "Docs")
		before := long(head, "the tail as bob left it")
		item := createTestItem(t, s, ws.ID, col.ID, "Linker", long(head, "the original tail"))
		if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &before, LastModifiedBy: "bob", Source: "web"}); err != nil {
			t.Fatal(err)
		}
		after := long(head, "the tail as alice left it")
		if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &after, LastModifiedBy: "alice", Source: "web"}); err != nil {
			t.Fatal(err)
		}
		var isDiff bool
		if err := s.db.QueryRow(s.q(`SELECT is_diff FROM item_versions WHERE item_id = ? ORDER BY created_at DESC, version_seq DESC LIMIT 1`), item.ID).Scan(&isDiff); err != nil || !isDiff {
			t.Fatalf("instrument: the newest version must be a reverse patch (is_diff=%v, %v)", isDiff, err)
		}
		return s, ws, col, item, before
	}
	newest := func(t *testing.T, s *Store, itemID string) string {
		t.Helper()
		cur, err := s.GetItem(itemID)
		if err != nil {
			t.Fatal(err)
		}
		versions, err := s.ListItemVersionsResolved(itemID, cur.Content)
		if err != nil || len(versions) == 0 {
			t.Fatalf("versions: %d, %v", len(versions), err)
		}
		return versions[0].Content
	}

	t.Run("wiki title cascade", func(t *testing.T) {
		s, ws, col, item, before := setup(t, "See [[Old Target]] for the plan.")
		target := createTestItem(t, s, ws.ID, col.ID, "Old Target", "the target")
		title := "New Target"
		if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &title}); err != nil {
			t.Fatal(err)
		}
		if cur, _ := s.GetItem(item.ID); !strings.Contains(cur.Content, "[[New Target]]") {
			t.Fatal("instrument: the cascade must rewrite the linking item's body")
		}
		if got := newest(t, s, item.ID); got != before {
			t.Errorf("the newest version after the cascade resolves to %q..., want the body before alice's edit, with the old title", got[:40])
		}
	})

	t.Run("attachment remap", func(t *testing.T) {
		oldID, newID := "00000000-0000-0000-0000-0000000000a1", "00000000-0000-0000-0000-0000000000b2"
		s, ws, _, item, before := setup(t, "![chart](pad-attachment:"+oldID+")")
		if err := s.RemapAttachmentReferencesInWorkspace(ws.ID, map[string]string{oldID: newID}); err != nil {
			t.Fatal(err)
		}
		if cur, _ := s.GetItem(item.ID); !strings.Contains(cur.Content, newID) {
			t.Fatal("instrument: the remap must rewrite the item's body")
		}
		if got := newest(t, s, item.ID); got != before {
			t.Errorf("the newest version after the remap resolves to %q..., want the body before alice's edit, with the old id", got[:40])
		}
	})
}
