package textguard

import (
	"strings"
	"testing"
)

// TestConstantsDidNotDecay is the guard against the mistake this package's own
// constants exist to prevent, and it compares against a value it BUILDS rather
// than one it types - because typing the six characters is the mistake.
func TestConstantsDidNotDecay(t *testing.T) {
	if len(NUL) != 1 || NUL[0] != 0 {
		t.Errorf("NUL is %q (len %d), want exactly one zero byte", NUL, len(NUL))
	}

	built := string([]byte{'\\', 'u', '0', '0', '0', '0'})
	if EscNUL != built {
		t.Errorf("EscNUL is %q (len %d), want the six-character escape TEXT %q", EscNUL, len(EscNUL), built)
	}
	if ContainsNUL(EscNUL) {
		t.Error("EscNUL carries a real NUL - it has decayed into the character it is supposed to spell")
	}
	if !ContainsNUL(NUL) {
		t.Error("NUL does not carry a real NUL")
	}
}

// TestCorpusIsWellFormed checks the corpus before anything is measured against
// it. A corpus with a duplicated case, a missing rationale, or a value that
// silently decayed would make every layer's differential test agree about
// nothing.
func TestCorpusIsWellFormed(t *testing.T) {
	if len(Corpus) < 10 {
		t.Fatalf("corpus has %d cases; it is meant to be adversarial, not a sample", len(Corpus))
	}
	seenName := map[string]bool{}
	seenValue := map[string]bool{}
	refused, accepted := 0, 0
	for _, c := range Corpus {
		if c.Name == "" || c.Why == "" {
			t.Errorf("case %q has no name or no rationale; a case nobody can re-derive is not evidence", c.Name)
		}
		if seenName[c.Name] {
			t.Errorf("duplicate case name %q", c.Name)
		}
		seenName[c.Name] = true
		key := c.Value + "|" + map[bool]string{true: "J", false: "T"}[c.IsJSON]
		if seenValue[key] {
			t.Errorf("case %q duplicates an earlier (value, classing) pair - it discriminates nothing new", c.Name)
		}
		seenValue[key] = true
		if c.Refused {
			refused++
		} else {
			accepted++
		}
	}
	// BOTH answers must be represented, or the corpus cannot tell a guard that
	// refuses everything from one that refuses the right things.
	if refused == 0 || accepted == 0 {
		t.Errorf("corpus has %d refused and %d accepted cases; it needs both to discriminate", refused, accepted)
	}

	// Every case that carries the ESCAPE must carry it as six characters. This
	// is the decay check applied to the corpus itself rather than to the
	// constants, because a case built by concatenation can still be typed
	// wrong.
	for _, c := range Corpus {
		if strings.Contains(c.Name, "ESCAPE") || strings.Contains(c.Name, "escape") {
			if !strings.Contains(c.Value, EscNUL) {
				t.Errorf("case %q claims to carry the escape but its value does not contain the six characters", c.Name)
			}
		}
	}
}

// TestParameterRefusedMatchesTheCorpus is the store layer's leg of the
// differential test - the predicate answering for every shape, before any
// database is involved. The remaining legs (HTTP gate, real SQLite write, real
// Postgres write) drive the same corpus from their own packages.
func TestParameterRefusedMatchesTheCorpus(t *testing.T) {
	for _, c := range Corpus {
		t.Run(c.Name, func(t *testing.T) {
			got := ParameterRefused(c.Value, c.IsJSON)
			if got != c.Refused {
				t.Errorf("ParameterRefused(%q, isJSON=%t) = %t, want %t\nwhy this case exists: %s",
					c.Value, c.IsJSON, got, c.Refused, c.Why)
			}
		})
	}
}

// TestKnownGapsStillGap asserts the recorded under-refusals are STILL
// under-refusals — and it is written to fail when one closes, not when one
// persists.
//
// That direction is the point. A gap that closes silently is a layer diverging
// from the others without anyone deciding to, which is the failure this whole
// cluster is made of. When BUG-2812's token-walk lands and this fails, the fix
// is to move the entry into Corpus with Refused as it should be.
func TestKnownGapsStillGap(t *testing.T) {
	if len(KnownGaps) == 0 {
		t.Skip("no recorded gaps")
	}
	for _, c := range KnownGaps {
		t.Run(c.Name, func(t *testing.T) {
			got := ParameterRefused(c.Value, c.IsJSON)
			if got == c.Refused {
				t.Errorf("this gap has CLOSED: ParameterRefused(%q, isJSON=%t) now returns %t, which is the "+
					"CORRECT answer. That is good news and a required action — move this case into Corpus "+
					"and delete it from KnownGaps, so the differential test starts enforcing it on every layer.\n"+
					"why it was a gap: %s", c.Value, c.IsJSON, got, c.Why)
			}
		})
	}
}
