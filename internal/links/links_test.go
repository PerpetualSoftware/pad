package links

import (
	"strings"
	"testing"
)

// TestRewriteBracketAt covers the position-based per-row cascade
// helper introduced for Codex round 7 finding 2. The cascade SELECT
// returns (position, target_title) per row; this helper rewrites
// exactly the bracket at `position` if its body matches target_title,
// preserving slug prefix and display suffix.
func TestRewriteBracketAt(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		bracket  string // for position lookup via strings.Index
		target   string // what the INDEX ROW recorded for this bracket
		old      string // the renamed item's title BEFORE the rename
		newTitle string
		slug     string
		want     string
	}{
		{
			name:     "plain bracket no display",
			content:  "prose [[Old Title]] more",
			bracket:  "[[Old Title]]",
			target:   "Old Title",
			old:      "Old Title",
			newTitle: "New Title",
			slug:     "tasks",
			want:     "prose [[New Title]] more",
		},
		{
			name:     "bracket with display alias preserved",
			content:  "prose [[Old Title|see this]] more",
			bracket:  "[[Old Title|see this]]",
			target:   "Old Title",
			old:      "Old Title",
			newTitle: "New Title",
			slug:     "tasks",
			want:     "prose [[New Title|see this]] more",
		},
		{
			name:     "qualified slug body verbatim target",
			content:  "see [[tasks/Old Title]] here",
			bracket:  "[[tasks/Old Title]]",
			target:   "tasks/Old Title",
			old:      "Old Title",
			newTitle: "New Title",
			slug:     "tasks",
			want:     "see [[tasks/New Title]] here",
		},
		{
			name:     "qualified slug with display",
			content:  "see [[tasks/Old Title|qual]] here",
			bracket:  "[[tasks/Old Title|qual]]",
			target:   "tasks/Old Title",
			old:      "Old Title",
			newTitle: "New Title",
			slug:     "tasks",
			want:     "see [[tasks/New Title|qual]] here",
		},
		{
			name:     "mixed case body matches case-insensitively",
			content:  "see [[old title]] here",
			bracket:  "[[old title]]",
			target:   "old title",
			old:      "Old Title",
			newTitle: "New Title",
			slug:     "tasks",
			want:     "see [[New Title]] here",
		},
		{
			name:     "bracket body doesn't match target — leave alone",
			content:  "see [[Something Else]] here",
			bracket:  "[[Something Else]]",
			target:   "Old Title",
			old:      "Old Title",
			newTitle: "New Title",
			slug:     "tasks",
			want:     "see [[Something Else]] here",
		},
		{
			name:     "literal-pipe-title row — whole body matches target",
			content:  "see [[Old Title|alias]] here",
			bracket:  "[[Old Title|alias]]",
			target:   "Old Title|alias", // row stored target_title=full body
			old:      "Old Title|alias",
			newTitle: "New Title",
			slug:     "tasks",
			// Renaming the item titled "Old Title|alias" → whole body becomes "New Title".
			want: "see [[New Title]] here",
		},
		{
			// BUG-2830, case A. The item's LITERAL title is "tasks/Setup" and it
			// lives in collection "tasks", so a `[[tasks/Setup]]` bracket resolved
			// to it by stage-1 exact-title match — it was never a qualified
			// reference. Re-emitting a `tasks/` prefix would produce a bracket
			// resolved by a different rule than the exact-title match the author
			// actually wrote. Where an item literally titled `tasks/Renamed`
			// exists, that item takes the link outright; otherwise resolution
			// merely becomes ambiguous. Both measured in internal/store's
			// items_rename_slug_prefix_test.go.
			name:     "literal slash-bearing title does not acquire a slug prefix",
			content:  "see [[tasks/Setup]] here",
			bracket:  "[[tasks/Setup]]",
			target:   "tasks/Setup",
			old:      "tasks/Setup",
			newTitle: "Renamed",
			slug:     "tasks",
			want:     "see [[Renamed]] here",
		},
		{
			// BUG-2830, case B — the TWIN, and the reason the fix is a
			// comparison rather than a prefix test. Content, bracket, target and
			// slug are BYTE-IDENTICAL to case A above. Only `old` differs, and
			// the correct answers differ with it: here the item is titled
			// "Setup", the bracket really was collection-qualified, and the
			// prefix must survive the rename.
			//
			// Keep these two adjacent. Either one alone can be satisfied by a
			// constant, and a fix that passed A while breaking B would be a
			// regression wearing a fix's clothes.
			name:     "genuinely qualified bracket keeps its slug prefix",
			content:  "see [[tasks/Setup]] here",
			bracket:  "[[tasks/Setup]]",
			target:   "tasks/Setup",
			old:      "Setup",
			newTitle: "Renamed",
			slug:     "tasks",
			want:     "see [[tasks/Renamed]] here",
		},
		{
			// BUG-2830, codex R1 P2. `strings.EqualFold` is UNICODE simple case
			// folding, and case-equivalent strings can differ in BYTE LENGTH:
			// EqualFold("K", "\u212A") — KELVIN SIGN — is true at 1 byte vs 3.
			//
			// So a byte-length precondition in front of EqualFold is not a cheap
			// pre-filter, it is a narrower predicate. Here the item is titled
			// "K" and the stored qualified body is "tasks/<KELVIN>"; a length
			// check rejects the pair, the bracket is treated as index drift, and
			// the rename silently leaves a stale qualified link behind.
			name:     "qualified match survives a case fold that changes byte length",
			content:  "see [[tasks/\u212A]] here",
			bracket:  "[[tasks/\u212A]]",
			target:   "tasks/\u212A",
			old:      "K",
			newTitle: "Renamed",
			slug:     "tasks",
			want:     "see [[tasks/Renamed]] here",
		},
		{
			name:     "split-key row preserves pipe-suffix",
			content:  "see [[Old Title|alias]] here",
			bracket:  "[[Old Title|alias]]",
			target:   "Old Title", // row stored target_title=split key, display preserved
			old:      "Old Title",
			newTitle: "New Title",
			slug:     "tasks",
			// Renaming "Old Title" rewrites the title segment, preserves |alias.
			want: "see [[New Title|alias]] here",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pos := strings.Index(c.content, c.bracket)
			if pos < 0 {
				t.Fatalf("bracket %q not found in content %q", c.bracket, c.content)
			}
			got := RewriteBracketAt(c.content, pos, c.target, c.old, c.newTitle, c.slug)
			if got != c.want {
				t.Errorf("RewriteBracketAt: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestRewriteBracketAt_RefusesIndexDrift covers the branch BUG-2830 introduced:
// a row whose target_title is NEITHER the old title nor its `<slug>/` qualified
// form.
//
// Every row the cascade selects points at the one item being renamed, so its
// target_title can only be that item's old title (in any case) or the qualified
// form. Anything else is index drift — the row and the item disagree about what
// the item is called. The rewriter leaves the bracket alone and lets the
// caller's trailing replaceWikiLinks re-parse reconcile the index, which is the
// same conservative posture the body-mismatch path has always had.
//
// This test exists because a mutation exposed the branch as unreachable by the
// rest of the suite: reverting qualifiedFor's `false, false` to `false, true`
// left every other test green. The mixed-changed-and-unchanged test could not
// see it either — there the drifted bracket happens to be byte-identical
// afterwards, so refusing and matching produce the same output. This fixture
// makes the two answers differ.
func TestRewriteBracketAt_RefusesIndexDrift(t *testing.T) {
	const content = "see [[Other]] here"
	// The row says this bracket points at the renamed item and records
	// target_title "Other"; the item's old title is "Old". Both cannot be true.
	got := RewriteBracketAt(content, 4, "Other", "Old", "New", "")
	if got != content {
		t.Errorf("drifted row rewrote the bracket: got %q, want it left alone (%q).\n"+
			"Matching on target_title alone would emit [[New]] here, silently "+
			"acting on a row whose recorded title is not the renamed item's.", got, content)
	}
}

// TestRewriteBracketAt_OutOfBoundsNoop covers the defensive guards:
// invalid position returns content unchanged.
func TestRewriteBracketAt_OutOfBoundsNoop(t *testing.T) {
	content := "see [[Old]] here"
	if got := RewriteBracketAt(content, -1, "Old", "Old", "New", ""); got != content {
		t.Errorf("negative position: got %q, want unchanged", got)
	}
	if got := RewriteBracketAt(content, len(content)+10, "Old", "Old", "New", ""); got != content {
		t.Errorf("past-EOF position: got %q, want unchanged", got)
	}
	if got := RewriteBracketAt(content, 0, "Old", "Old", "New", ""); got != content {
		t.Errorf("position not at `[[`: got %q, want unchanged", got)
	}
}

// TestRewriteWikiTitle covers the four title-form shapes the
// item-rename cascade depends on. Case-insensitive title matching
// mirrors resolveTitleTx (and the renderer's title resolution at
// web/src/lib/utils/markdown.ts::resolveWikiBody). Display aliases must be
// preserved verbatim — including padding and escaped chars — so
// the renderer's user-facing text doesn't silently change.
func TestRewriteWikiTitle(t *testing.T) {
	cases := []struct {
		name, in, old, new, slug, want string
	}{
		{"plain", "see [[Old Title]] here", "Old Title", "New Title", "tasks", "see [[New Title]] here"},
		{"aliased", "see [[Old Title|click me]]", "Old Title", "New Title", "tasks", "see [[New Title|click me]]"},
		{"qualified", "see [[tasks/Old Title]]", "Old Title", "New Title", "tasks", "see [[tasks/New Title]]"},
		{"qualified aliased", "see [[tasks/Old Title|qual]]", "Old Title", "New Title", "tasks", "see [[tasks/New Title|qual]]"},
		{"mixed case", "see [[old title]] and [[OLD TITLE]]", "Old Title", "New Title", "tasks", "see [[New Title]] and [[New Title]]"},
		{"display preserves padding", "see [[Old Title|  padded  ]]", "Old Title", "New Title", "tasks", "see [[New Title|  padded  ]]"},
		{"display preserves escaped pipe", `see [[Old Title|a \| b]]`, "Old Title", "New Title", "tasks", `see [[New Title|a \| b]]`},
		{"non-matching qualified slug untouched", "see [[other/Old Title]]", "Old Title", "New Title", "tasks", "see [[other/Old Title]]"},
		{"only-matching-title untouched", "see [[Different]]", "Old Title", "New Title", "tasks", "see [[Different]]"},
		{"multiple occurrences", "[[Old Title]] then [[Old Title|alias]]", "Old Title", "New Title", "tasks", "[[New Title]] then [[New Title|alias]]"},
		{"empty oldTitle no-op", "see [[Old Title]]", "", "New Title", "tasks", "see [[Old Title]]"},
		{"same title no-op", "see [[Old Title]]", "Old Title", "Old Title", "tasks", "see [[Old Title]]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RewriteWikiTitle(c.in, c.old, c.new, c.slug)
			if got != c.want {
				t.Errorf("RewriteWikiTitle(%q, %q, %q, %q) = %q, want %q",
					c.in, c.old, c.new, c.slug, got, c.want)
			}
		})
	}
}
