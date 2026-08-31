package store

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/links"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2805 end-to-end, through the real rename cascade.
//
// The unit tests in internal/links pin the rewriter; these pin that the CASCADE
// actually carries the fix to stored content and to the index. Both directions
// were reproduced through the real API on TASK-2826 (Rook) before any fix, with
// hex receipts; these are the same two shapes as store-level regressions.

// TestCascade_RewritesLinksStoredInEscapedForm is BUG-2805 direction 1.
//
// Before: the cascade compared the RAW bracket body against the unescaped
// title, never matched, and left the visible link naming the OLD title while
// the `!applied` arm re-parsed and flipped the index row to broken. Content and
// index disagreed, and the user saw a stale link.
func TestCascade_RewritesLinksStoredInEscapedForm(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "EscapeRoundTrip")
	col := createTestCollection(t, s, ws.ID, "Notes")

	target := createTestItem(t, s, ws.ID, col.ID, "Weird ] Title", "the item being renamed")
	source := createTestItem(t, s, ws.ID, col.ID, "Source A", `See [[Weird \] Title]] for details.`)

	// Precondition: the escaped link indexed and resolved to the target. If it
	// did not, the cascade would have nothing to carry and this test would pass
	// for the wrong reason.
	var resolved int
	if err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM item_wiki_links
		WHERE source_item_id = ? AND target_item_id = ? AND target_kind = 'title'
	`), source.ID, target.ID).Scan(&resolved); err != nil {
		t.Fatalf("precondition query: %v", err)
	}
	if resolved != 1 {
		t.Fatalf("precondition: escaped link indexed %d resolved rows, want 1", resolved)
	}

	newTitle := "Weird Renamed Title"
	if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	got, err := s.GetItem(source.ID)
	if err != nil {
		t.Fatalf("re-read source: %v", err)
	}
	if strings.Contains(got.Content, `Weird \] Title`) {
		t.Errorf("source content still carries the OLD escaped title: %q — the cascade left the "+
			"link stale (BUG-2805 direction 1)", got.Content)
	}
	if want := "See [[Weird Renamed Title]] for details."; got.Content != want {
		t.Errorf("content = %q, want %q", got.Content, want)
	}

	// And the index followed the content rather than flipping broken.
	if err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM item_wiki_links
		WHERE source_item_id = ? AND target_item_id = ? AND target_kind = 'title'
	`), source.ID, target.ID).Scan(&resolved); err != nil {
		t.Fatalf("post query: %v", err)
	}
	if resolved != 1 {
		t.Errorf("index has %d resolved rows after the rename, want 1 — content and index "+
			"disagree", resolved)
	}
}

// TestCascade_EmitsEscapedTitlesAndDoesNotDestroyTheIndex is BUG-2805
// direction 2a, the data-destroying one.
//
// Before: renaming TO a title containing `]` emitted `[[New ] Name]]`, which the
// grammar cannot parse, so the re-parse found no link and DELETED the index row.
// The damage was permanent — a later recovery rename had no row left to cascade,
// so the source content stayed wrong forever. This test pins the emission AND
// the recovery, because the permanence is the part that made it medium severity.
func TestCascade_EmitsEscapedTitlesAndDoesNotDestroyTheIndex(t *testing.T) {
	for _, tc := range []struct{ name, newTitle, wantContent string }{
		{"closing bracket", "New ] Name", `Ref [[New \] Name]] here.`},
		{"pipe", "New | Name", `Ref [[New \| Name]] here.`},
		{"backslash then bracket", `Odd \] Name`, `Ref [[Odd \\\] Name]] here.`},
		{"bare backslash", `Odd \ Name`, `Ref [[Odd \\ Name]] here.`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ws := createTestWorkspace(t, s, "EscapeEmit")
			col := createTestCollection(t, s, ws.ID, "Notes")
			target := createTestItem(t, s, ws.ID, col.ID, "Plain Target", "the item being renamed")
			source := createTestItem(t, s, ws.ID, col.ID, "Source B", "Ref [[Plain Target]] here.")

			if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &tc.newTitle}); err != nil {
				t.Fatalf("rename: %v", err)
			}

			got, err := s.GetItem(source.ID)
			if err != nil {
				t.Fatalf("re-read: %v", err)
			}
			if got.Content != tc.wantContent {
				t.Errorf("content = %q, want %q", got.Content, tc.wantContent)
			}

			// The emitted bracket must parse back to exactly the new title.
			parsed := links.ExtractWikiLinks(got.Content)
			if len(parsed) != 1 {
				t.Fatalf("emitted content %q parses to %d links, want 1 — an unparseable "+
					"bracket is what deletes the index row", got.Content, len(parsed))
			}
			if parsed[0].Title != tc.newTitle {
				t.Errorf("emitted content parses to title %q, want %q", parsed[0].Title, tc.newTitle)
			}

			// The index row survived AND still resolves.
			var resolved int
			if err := s.db.QueryRow(s.q(`
				SELECT COUNT(*) FROM item_wiki_links
				WHERE source_item_id = ? AND target_item_id = ? AND target_kind = 'title'
			`), source.ID, target.ID).Scan(&resolved); err != nil {
				t.Fatalf("index query: %v", err)
			}
			if resolved != 1 {
				t.Fatalf("index has %d resolved rows, want 1 — the row was destroyed, which is "+
					"the permanent-damage shape", resolved)
			}

			// PERMANENCE CHECK: a second rename must still find the row and
			// carry the link. Without it this test would pass on a fix that
			// merely delayed the destruction by one rename.
			recovered := "Recovered Name"
			if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &recovered}); err != nil {
				t.Fatalf("recovery rename: %v", err)
			}
			got2, err := s.GetItem(source.ID)
			if err != nil {
				t.Fatalf("re-read after recovery: %v", err)
			}
			if want := "Ref [[Recovered Name]] here."; got2.Content != want {
				t.Errorf("after recovery rename content = %q, want %q — the cascade could not "+
					"find the link, which is the permanence failure", got2.Content, want)
			}
		})
	}
}

// TestCascade_RenameToEmptyTitleDoesNotDestroyLinks closes a second door onto
// BUG-2805's data-destruction shape, found while probing the TitleEscaper's
// zero value rather than by review.
//
// `handleUpdateItem` has NO empty-title guard — the "Title is required" check
// lives only in `handleCreateItem` — and the store tolerates it, mapping an
// empty slug to "untitled". So a rename to "" reaches the cascade, and the
// rewriter emitted `[[]]`: not a link under the parser's `+` production, so the
// re-parse deleted the index row. Measured before the guard: content became
// `Ref [[]] here.`, parsed to 0 links, 0 index rows. Identical damage to
// direction 2a, and PRE-EXISTING — escaping does not help, since escape("")
// is "".
//
// The rewriter now refuses rather than emitting an unparseable bracket. The
// link keeps naming the old title, stays clickable, and a later valid rename
// can still repair it. The door-level validation gap is filed separately; this
// pins that the rewriter will not be the instrument of destruction even if that
// gap is never closed.
func TestCascade_RenameToEmptyTitleDoesNotDestroyLinks(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "EmptyRenameGuard")
	col := createTestCollection(t, s, ws.ID, "Notes")
	target := createTestItem(t, s, ws.ID, col.ID, "Plain Target", "the item being renamed")
	source := createTestItem(t, s, ws.ID, col.ID, "Src", "Ref [[Plain Target]] here.")

	empty := ""
	if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &empty}); err != nil {
		// The store currently ALLOWS this. If a future change starts refusing
		// it at the store or handler layer that is a fine outcome too — but say
		// so loudly rather than letting this test silently stop exercising the
		// path it was written for.
		t.Skipf("rename to an empty title is now refused by the store (%v) — the door-level "+
			"gap this test guards against may have been closed; re-read before deleting", err)
	}

	got, err := s.GetItem(source.ID)
	if err != nil {
		t.Fatalf("re-read source: %v", err)
	}
	if got.Content != "Ref [[Plain Target]] here." {
		t.Errorf("content = %q, want the link left intact — emitting an empty bracket destroys "+
			"the link and its index row", got.Content)
	}
	if n := len(links.ExtractWikiLinks(got.Content)); n != 1 {
		t.Errorf("content parses to %d links, want 1 — the bracket must stay parseable", n)
	}

	// INDEX STATE IN FULL, not a row count. Codex R2 was right that the first
	// version of this assertion was blind: it counted rows, so it would have
	// passed with the row present but target_item_id cleared — a broken
	// backlink reported as success.
	//
	// What SHOULD happen, and does: the row survives with target_item_id NULL.
	// That is correct rather than damage — the target no longer has any title,
	// so no title-form link can resolve to it, and a NULL target mirrors exactly
	// what a renderer would resolve. The index is supposed to track
	// resolvability. What must NOT happen is the row disappearing, which is what
	// an unparseable `[[]]` emission caused.
	var rows int
	var resolvedTarget sql.NullString
	if err := s.db.QueryRow(s.q(
		`SELECT COUNT(*) FROM item_wiki_links WHERE source_item_id = ?`), source.ID).Scan(&rows); err != nil {
		t.Fatalf("index query: %v", err)
	}
	if rows != 1 {
		t.Fatalf("index has %d rows for the source, want 1 — the row was destroyed", rows)
	}
	if err := s.db.QueryRow(s.q(
		`SELECT target_item_id FROM item_wiki_links WHERE source_item_id = ?`), source.ID).Scan(&resolvedTarget); err != nil {
		t.Fatalf("target query: %v", err)
	}
	if resolvedTarget.Valid {
		t.Errorf("target_item_id is still set after the target lost its title — the index claims " +
			"a resolution the renderer cannot reproduce")
	}

	// RECOVERY, which is the leg that distinguishes "broken but honest" from
	// "irrecoverable". Codex R2 called this unrecoverable; it is not. Renaming
	// back re-resolves the row through resolveBrokenTitleLinks, because the
	// CONTENT was never destroyed — which is precisely what the emission guard
	// protects.
	back := "Plain Target"
	if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &back}); err != nil {
		t.Fatalf("recovery rename: %v", err)
	}
	if err := s.db.QueryRow(s.q(
		`SELECT target_item_id FROM item_wiki_links WHERE source_item_id = ?`), source.ID).Scan(&resolvedTarget); err != nil {
		t.Fatalf("target query after recovery: %v", err)
	}
	if !resolvedTarget.Valid {
		t.Errorf("backlink did not recover when the title came back — THAT would make the empty " +
			"rename irrecoverable, which is the claim this leg exists to test")
	}
}
