package links

import "testing"

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
