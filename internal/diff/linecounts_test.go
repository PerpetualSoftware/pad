package diff

import "testing"

func TestLineCounts(t *testing.T) {
	for _, c := range []struct {
		name, old, new string
		added, removed int
	}{
		{"create", "", "a\nb\nc\n", 3, 0},
		{"clear", "a\nb\n", "", 0, 2},
		{"one line edited", "a\nb\nc\n", "a\nB\nc\n", 1, 1},
		{"one character edited is one line, not a chunk count", "alpha beta\n", "alpha Beta\n", 1, 1},
		{"insert in the middle", "a\nc\n", "a\nb\nc\n", 1, 0},
		// The old last line gains a newline, so it is removed and re-added: jsdiff's
		// diffLines tokenises the same way and DiffView renders the same +2 −1.
		{"appending past a last line with no newline", "a\nb", "a\nb\nc", 2, 1},
		{"unchanged", "a\nb\n", "a\nb\n", 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, r := LineCounts(c.old, c.new)
			if a != c.added || r != c.removed {
				t.Fatalf("LineCounts = +%d −%d, want +%d −%d", a, r, c.added, c.removed)
			}
		})
	}
}
