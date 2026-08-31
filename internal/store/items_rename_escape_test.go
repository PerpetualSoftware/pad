package store

import (
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
