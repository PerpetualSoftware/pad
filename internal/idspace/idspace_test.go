package idspace

import "testing"

func TestBasesStrictlyIncreaseWithinAProcess(t *testing.T) {
	// The clock separates incarnations across restarts; it does NOT separate
	// two buses constructed inside the same millisecond, which is both what a
	// test does and what makes the invariant conditional rather than true.
	// This pins the CAS half — remove it and this fails, because the loop is
	// fast enough to land repeatedly in one millisecond.
	const n = 200
	prev := int64(0)
	for i := 0; i < n; i++ {
		base := New()
		if base <= prev {
			t.Fatalf("base %d at iteration %d did not exceed the previous base %d", base, i, prev)
		}
		prev = base
	}
}

func TestBasesAreShiftedFarEnoughToHoldAMillisecondOfIDs(t *testing.T) {
	// The invariant in Shift's comment is arithmetic, and arithmetic in a
	// comment is a claim. Two consecutive bases must differ by at least one
	// full stride, so a process publishing fewer than 1<<Shift events per
	// millisecond of its lifetime cannot collide with its successor.
	first := New()
	second := New()
	if gap := second - first; gap < 1<<Shift {
		t.Fatalf("consecutive bases differ by %d, which is less than one stride (%d)", gap, int64(1)<<Shift)
	}
}

func TestBasesDoNotOverflowAtTheDocumentedInstant(t *testing.T) {
	// The overflow date in Shift's comment names a specific millisecond. Pin
	// it: one millisecond later must not fit. Stated as the boundary rather
	// than as a date string so the test fails if Shift changes without the
	// comment following it.
	//
	// The shifts run on VARIABLES rather than constants on purpose: the
	// overflowing one is a compile error as an untyped constant, which would
	// make the boundary unassertable at exactly the point it matters.
	lastFitting := int64(8796093022207)
	if got := lastFitting << Shift; got <= 0 {
		t.Fatalf("millisecond %d should still fit in an int64 base, got %d", lastFitting, got)
	}
	if got := (lastFitting + 1) << Shift; got > 0 {
		t.Fatalf("millisecond %d should overflow the int64 base, got %d", lastFitting+1, got)
	}
}
