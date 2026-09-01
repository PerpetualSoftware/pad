package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2830 end-to-end, through the real rename cascade.
//
// The unit tests in internal/links pin TitleEscaper.qualifiedFor directly.
// These pin the BINDING: that cascadeTitleRename actually hands the escaper the
// renamed item's OLD title, and hands it the right one. A direct-call test
// cannot show that (CONVE-19) — and the binding is the whole fix here, because
// the discriminating value existed in the cascade all along and simply never
// reached the rewriter.
//
// The filing asked for a repro before any fix landed: create the ambiguous
// literal title, link it, rename it, observe what the cascade emits. It also
// predicted a "retarget" as the outcome; see the boundary note at the bottom of
// the first test for what was actually observed.

// TestItemRename_LiteralSlashTitleIsNotRewrittenAsQualified is the repro.
//
// An item whose LITERAL title is "tasks/Setup", living in the collection whose
// slug is "tasks". A `[[tasks/Setup]]` bracket resolves to it by exact-title
// match, and the index row records target_title = "tasks/Setup" — byte-for-byte
// what a genuinely qualified reference to an item titled "Setup" would record.
//
// Renaming it must produce `[[Renamed]]`. The pre-fix cascade produced
// `[[tasks/Renamed]]`, which is a QUALIFIED reference — a different kind of
// reference than the author wrote, resolved by a different rule. What that
// costs in practice is recorded in the boundary note at the bottom of this
// test, which is narrower than the filing predicted.
func TestItemRename_LiteralSlashTitleIsNotRewrittenAsQualified(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	if col.Slug != "tasks" {
		t.Fatalf("fixture depends on the collection slug being %q, got %q — "+
			"the whole ambiguity is between that slug and the literal title's prefix", "tasks", col.Slug)
	}

	// The item under rename. Its title CONTAINS the collection's own slug plus
	// a slash, which is what makes the stored row ambiguous.
	target := createTestItem(t, s, ws.ID, col.ID, "tasks/Setup", "")

	const newTitle = "Renamed 2"

	source := createTestItem(t, s, ws.ID, col.ID, "Source",
		"Please see [[tasks/Setup]] for details.")

	// Baseline: the bracket resolves to the literal-titled item.
	bls, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(bls) != 1 {
		t.Fatalf("baseline: expected the bracket to resolve to the literal-titled item, got %d backlinks", len(bls))
	}

	title := newTitle
	if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatalf("UpdateItem rename: %v", err)
	}

	updated, err := s.GetItem(source.ID)
	if err != nil {
		t.Fatalf("GetItem source: %v", err)
	}

	if strings.Contains(updated.Content, "[[tasks/Renamed 2]]") {
		t.Errorf("cascade emitted a QUALIFIED bracket for an item whose title merely "+
			"starts with the collection slug.\n content = %q\n"+
			"The author wrote a literal-title reference; `[[tasks/Renamed 2]]` is a "+
			"collection-qualified one, resolved by a different rule and ambiguous "+
			"wherever a same-titled sibling exists. See the boundary note below for "+
			"what this does and does not demonstrate.",
			updated.Content)
	}
	if !strings.Contains(updated.Content, "[[Renamed 2]]") {
		t.Errorf("expected the literal title to be rewritten plainly as [[Renamed 2]], got %q", updated.Content)
	}

	got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 1 {
		t.Errorf("post-rename: expected the link to still resolve to the renamed item, got %d backlinks", len(got))
	}
	// SCOPE — what this test shows, and where the rest of the evidence is.
	//
	// This one pins the CONTENT: the cascade must not rewrite a literal-title
	// reference into a collection-qualified one. That is the defect itself,
	// independent of what any particular workspace happens to contain.
	//
	// It deliberately has NO decoy. With no item literally titled
	// `tasks/Renamed 2`, the wrongly-emitted bracket still finds the renamed
	// item — collSlug is that item's own collection and it now carries the new
	// title, so it satisfies the qualified fallback itself. A decoy here would
	// therefore assert nothing, and an assertion that cannot fire is worse than
	// none because it reads as evidence.
	//
	// The retarget the filing predicted IS real, and it needs a decoy whose
	// literal title contains the slash, so that resolveTitleTx's stage-1
	// full-title match beats the qualified fallback. That is
	// TestItemRename_LiteralSlashTitleRetargetsOntoALiteralSlashSibling below,
	// where the decoy demonstrably gains the backlink under the unfixed code.
	//
	// Recorded because I got this wrong in both directions before measuring it:
	// first asserting a retarget with a decoy that could not be stolen, then
	// over-correcting to "the renamed item always wins", which codex R4 caught
	// by reading resolveTitleTx's stage order rather than my summary of it.
}

// TestItemRename_LiteralSlashTitleRetargetsOntoALiteralSlashSibling is the
// DETERMINISTIC retarget, and it exists because my first boundary note was
// wrong in the other direction (codex R4).
//
// Having measured that the wrongly-qualified bracket still found the renamed
// item, I wrote that it "always" does. It does not. resolveTitleTx tries an
// exact FULL-TITLE match (stage 1) before the qualified fallback (stage 2), so
// an item literally titled `tasks/<newTitle>` — slash and all — captures
// `[[tasks/<newTitle>]]` outright and beats the renamed item to it.
//
// That makes the filing's "silent retarget" real after all, with a decoy the
// earlier fixture did not have: the decoy's title must contain the slash. The
// link moves off the renamed item entirely, and nothing in the UI says so.
func TestItemRename_LiteralSlashTitleRetargetsOntoALiteralSlashSibling(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "tasks/Setup", "")

	// The decoy that CAN be stolen: its literal title is what the pre-fix
	// cascade would emit, so stage 1 hands the link to it.
	decoy := createTestItem(t, s, ws.ID, col.ID, "tasks/Renamed 2", "")

	source := createTestItem(t, s, ws.ID, col.ID, "Source",
		"Please see [[tasks/Setup]] for details.")

	// Premise, asserted rather than assumed: before the rename the link belongs
	// to the target and the decoy has nothing. Without this leg a decoy that was
	// never linked would "prove" the fix by staying at zero.
	if bls, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true}); len(bls) != 1 {
		t.Fatalf("baseline: target should own the link, got %d backlinks", len(bls))
	}
	if pre, _ := s.GetBacklinks(decoy.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true}); len(pre) != 0 {
		t.Fatalf("baseline: decoy should own nothing, got %d backlinks", len(pre))
	}

	title := "Renamed 2"
	if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatalf("UpdateItem rename: %v", err)
	}

	stolen, _ := s.GetBacklinks(decoy.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(stolen) != 0 {
		updated, _ := s.GetItem(source.ID)
		t.Errorf("the rename RETARGETED the link onto a different item.\n"+
			" content  = %q\n"+
			" decoy %q gained %d backlink(s) from a rename it had nothing to do with.\n"+
			"Emitting `[[tasks/Renamed 2]]` for an item whose literal title was "+
			"`tasks/Setup` hands the link to whatever item is LITERALLY titled "+
			"`tasks/Renamed 2`, because resolveTitleTx matches the full title before "+
			"falling back to the qualified form.",
			updated.Content, decoy.Title, len(stolen))
	}

	kept, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(kept) != 1 {
		t.Errorf("post-rename: the renamed item should still own its link, got %d backlinks", len(kept))
	}
}

// TestItemRename_GenuinelyQualifiedBracketKeepsItsPrefix is the twin, and it is
// what stops the fix above from being "never emit a prefix".
//
// Here the item really IS titled "Setup" and the `[[tasks/Setup]]` bracket
// really was a collection-qualified reference. The stored target_title is
// identical to the case above — "tasks/Setup" — so only the old title tells
// them apart, and this one must KEEP its prefix across the rename.
func TestItemRename_GenuinelyQualifiedBracketKeepsItsPrefix(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Setup", "")
	source := createTestItem(t, s, ws.ID, col.ID, "Source",
		"Please see [[tasks/Setup]] for details.")

	bls, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(bls) != 1 {
		t.Fatalf("baseline: expected the qualified bracket to resolve, got %d backlinks", len(bls))
	}

	newTitle := "Setup Renamed"
	if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("UpdateItem rename: %v", err)
	}

	updated, err := s.GetItem(source.ID)
	if err != nil {
		t.Fatalf("GetItem source: %v", err)
	}
	if !strings.Contains(updated.Content, "[[tasks/Setup Renamed]]") {
		t.Errorf("a genuinely qualified bracket LOST its slug prefix: got %q, want it to keep `tasks/`.\n"+
			"Dropping the prefix here would be the BUG-2830 fix overshooting — the two cases "+
			"are distinguished by the old title, not by refusing to qualify at all.",
			updated.Content)
	}

	got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 1 {
		t.Errorf("post-rename: expected the qualified link to still resolve, got %d backlinks", len(got))
	}
}
