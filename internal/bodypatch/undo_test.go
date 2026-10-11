package bodypatch

import (
	"errors"
	"strings"
	"testing"
)

// PLAN-3578 step 3 (lead ruling): an undo is the inverse patch anchored on
// each edit's context, and it refuses unless every anchor occurs exactly once.

func roundTrip(t *testing.T, body string, edits []Edit) {
	t.Helper()
	after, applied, err := ApplyWithContext(body, edits)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want, _ := Apply(body, edits); want != after {
		t.Fatalf("ApplyWithContext disagrees with Apply: %q vs %q", after, want)
	}
	back, err := Apply(after, Invert(applied))
	if err != nil {
		t.Fatalf("undo refused: %v (applied %+v)", err, applied)
	}
	if back != body {
		t.Fatalf("undo gave %q, want %q", back, body)
	}
}

func TestUndoReplaceDeleteInsert(t *testing.T) {
	body := "intro line\nthe quick brown fox jumps\nlast line\n"
	t.Run("replace", func(t *testing.T) { roundTrip(t, body, []Edit{{Old: "quick", New: "slow"}}) })
	t.Run("delete", func(t *testing.T) { roundTrip(t, body, []Edit{{Old: "brown ", New: ""}}) })
	t.Run("insert", func(t *testing.T) { roundTrip(t, body, []Edit{{Old: "fox", New: "fox, then a dog,"}}) })
	t.Run("delete a whole line", func(t *testing.T) { roundTrip(t, body, []Edit{{Old: "the quick brown fox jumps\n", New: ""}}) })
	t.Run("several edits undo in reverse", func(t *testing.T) {
		roundTrip(t, body, []Edit{{Old: "intro", New: "INTRO"}, {Old: "INTRO line\nthe", New: "x"}, {Old: "last", New: ""}})
	})
	t.Run("multibyte context is not split", func(t *testing.T) {
		roundTrip(t, strings.Repeat("é", 30)+"target"+strings.Repeat("ü", 30), []Edit{{Old: "target", New: "T"}})
	})
}

func TestUndoRefusesAfterALaterEditToItsText(t *testing.T) {
	body := "alpha beta gamma\n"
	after, applied, err := ApplyWithContext(body, []Edit{{Old: "beta", New: "BETA"}})
	if err != nil {
		t.Fatal(err)
	}
	// Someone rewrites the text around the edit afterwards.
	later := strings.Replace(after, "alpha BETA", "omega BETA", 1)
	if _, err := Apply(later, Invert(applied)); !errors.As(err, new(*AnchorMissingError)) {
		t.Fatalf("an undo over a later change must refuse; got %v", err)
	}
}

func TestUndoRefusesAnAmbiguousAnchor(t *testing.T) {
	body := "x\n"
	after, applied, err := ApplyWithContext(body, []Edit{{Old: "x", New: "y"}})
	if err != nil {
		t.Fatal(err)
	}
	// The same line now exists twice.
	twice := after + after
	if _, err := Apply(twice, Invert(applied)); !errors.As(err, new(*AnchorAmbiguousError)) {
		t.Fatalf("an ambiguous undo anchor must refuse; got %v", err)
	}
}

func TestContextIsBoundedAndLineClipped(t *testing.T) {
	_, applied, err := ApplyWithContext("first line\n"+strings.Repeat("a", 100)+"Z"+strings.Repeat("b", 100)+"\nlast", []Edit{{Old: "Z", New: "z"}})
	if err != nil {
		t.Fatal(err)
	}
	a := applied[0]
	if len(a.CtxBefore) != ContextChars || len(a.CtxAfter) != ContextChars || strings.Contains(a.CtxBefore+a.CtxAfter, "\n") {
		t.Fatalf("context = %q / %q", a.CtxBefore, a.CtxAfter)
	}
}
