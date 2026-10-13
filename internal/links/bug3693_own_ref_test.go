package links

import (
	"strings"
	"testing"
)

// BUG-3693: a reference-shaped new title is written as the renamed item's own
// reference, with the title as display text, and the writer and the
// projection agree on every bracket shape.
func TestBUG3693_ReferenceShapedTitleWritesTheOwnRef(t *testing.T) {
	for _, tc := range []struct {
		name, newTitle, ownRef, in, want string
	}{
		{"plain", "TASK-1", "TASK-2", "a [[Old]] b", "a [[TASK-2|TASK-1]] b"},
		{"aliased", "TASK-1", "TASK-2", "a [[Old|see this]] b", "a [[TASK-2|see this]] b"},
		{"qualified", "TASK-1", "TASK-2", "a [[tasks/Old]] b", "a [[TASK-2|TASK-1]] b"},
		{"workspace ref", "acme::BUG-7", "TASK-2", "a [[Old]] b", "a [[TASK-2|acme::BUG-7]] b"},
		{"not a ref", "Task 1", "TASK-2", "a [[Old]] b", "a [[Task 1]] b"},
		{"no own ref: qualified title", "TASK-1", "", "a [[Old]] b", "a [[tasks/TASK-1]] b"},
		{"no own ref, aliased", "TASK-1", "", "a [[Old|see]] b", "a [[tasks/TASK-1|see]] b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			esc := NewTitleEscaper("Old", tc.newTitle, "tasks").WithOwnRef(tc.newTitle, tc.ownRef)
			target := "Old"
			if strings.Contains(tc.in, "tasks/") {
				target = "tasks/Old"
			}
			rw := []BracketRewrite{{Position: strings.Index(tc.in, "[["), TargetTitle: target}}
			got, applied := RewriteBracketsAt(tc.in, rw, esc)
			if got != tc.want || applied != 1 {
				t.Fatalf("got %q (applied %d), want %q", got, applied, tc.want)
			}
			if n, a := ProjectRewrittenLen(tc.in, rw, esc); n != len(got) || a != applied {
				t.Errorf("projection %d/%d, rewrite %d/%d", n, a, len(got), applied)
			}
			// Running the same rename again over its own output changes nothing.
			if again, a := RewriteBracketsAt(got, []BracketRewrite{{Position: strings.Index(got, "[["), TargetTitle: target}}, esc); a != 0 || again != got {
				t.Errorf("a second pass rewrote %q to %q", got, again)
			}
		})
	}
}

// No own ref and no slug to qualify with: the bracket is left as it is.
func TestBUG3693_ReferenceShapedTitleWithNothingSafeToWriteIsLeft(t *testing.T) {
	esc := NewTitleEscaper("Old", "TASK-1", "").WithOwnRef("TASK-1", "")
	in := "a [[Old]] b"
	rw := []BracketRewrite{{Position: 2, TargetTitle: "Old"}}
	if got, applied := RewriteBracketsAt(in, rw, esc); got != in || applied != 0 {
		t.Errorf("got %q (applied %d), want it unchanged", got, applied)
	}
	if n, a := ProjectRewrittenLen(in, rw, esc); n != len(in) || a != 0 {
		t.Errorf("projection %d/%d, want %d/0", n, a, len(in))
	}
}
