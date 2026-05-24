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
		target   string
		newTitle string
		slug     string
		want     string
	}{
		{
			name:     "plain bracket no display",
			content:  "prose [[Old Title]] more",
			bracket:  "[[Old Title]]",
			target:   "Old Title",
			newTitle: "New Title",
			slug:     "tasks",
			want:     "prose [[New Title]] more",
		},
		{
			name:     "bracket with display alias preserved",
			content:  "prose [[Old Title|see this]] more",
			bracket:  "[[Old Title|see this]]",
			target:   "Old Title",
			newTitle: "New Title",
			slug:     "tasks",
			want:     "prose [[New Title|see this]] more",
		},
		{
			name:     "qualified slug body verbatim target",
			content:  "see [[tasks/Old Title]] here",
			bracket:  "[[tasks/Old Title]]",
			target:   "tasks/Old Title",
			newTitle: "New Title",
			slug:     "tasks",
			want:     "see [[tasks/New Title]] here",
		},
		{
			name:     "qualified slug with display",
			content:  "see [[tasks/Old Title|qual]] here",
			bracket:  "[[tasks/Old Title|qual]]",
			target:   "tasks/Old Title",
			newTitle: "New Title",
			slug:     "tasks",
			want:     "see [[tasks/New Title|qual]] here",
		},
		{
			name:     "mixed case body matches case-insensitively",
			content:  "see [[old title]] here",
			bracket:  "[[old title]]",
			target:   "old title",
			newTitle: "New Title",
			slug:     "tasks",
			want:     "see [[New Title]] here",
		},
		{
			name:     "bracket body doesn't match target — leave alone",
			content:  "see [[Something Else]] here",
			bracket:  "[[Something Else]]",
			target:   "Old Title",
			newTitle: "New Title",
			slug:     "tasks",
			want:     "see [[Something Else]] here",
		},
		{
			name:     "literal-pipe-title row — whole body matches target",
			content:  "see [[Old Title|alias]] here",
			bracket:  "[[Old Title|alias]]",
			target:   "Old Title|alias", // row stored target_title=full body
			newTitle: "New Title",
			slug:     "tasks",
			// Renaming the item titled "Old Title|alias" → whole body becomes "New Title".
			want: "see [[New Title]] here",
		},
		{
			name:     "split-key row preserves pipe-suffix",
			content:  "see [[Old Title|alias]] here",
			bracket:  "[[Old Title|alias]]",
			target:   "Old Title", // row stored target_title=split key, display preserved
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
			got := RewriteBracketAt(c.content, pos, c.target, c.newTitle, c.slug)
			if got != c.want {
				t.Errorf("RewriteBracketAt: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestRewriteBracketAt_OutOfBoundsNoop covers the defensive guards:
// invalid position returns content unchanged.
func TestRewriteBracketAt_OutOfBoundsNoop(t *testing.T) {
	content := "see [[Old]] here"
	if got := RewriteBracketAt(content, -1, "Old", "New", ""); got != content {
		t.Errorf("negative position: got %q, want unchanged", got)
	}
	if got := RewriteBracketAt(content, len(content)+10, "Old", "New", ""); got != content {
		t.Errorf("past-EOF position: got %q, want unchanged", got)
	}
	if got := RewriteBracketAt(content, 0, "Old", "New", ""); got != content {
		t.Errorf("position not at `[[`: got %q, want unchanged", got)
	}
}

// TestRewriteWikiTitle covers the four title-form shapes the
// item-rename cascade depends on. Case-insensitive title matching
// mirrors resolveTitleTx (and the renderer's title resolution at
// web/src/lib/utils/markdown.ts:543). Display aliases must be
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
