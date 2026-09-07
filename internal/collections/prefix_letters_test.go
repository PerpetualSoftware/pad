package collections

import (
	"strings"
	"testing"
)

// BUG-2943: every character of a derived prefix must satisfy the SAME test
// parseItemRef applies, or the collection's items print an issue ID that the
// CLI cannot resolve back. The measured case: "TEMP Rook A 2870" derived
// "TRA2", and `pad item show TRA2-2942` answered "item not found" while the
// slug resolved fine.
//
// The assertion is deliberately the CONSTRAINT rather than a table of expected
// strings: a test that only pinned "TRA" would pass any other letters-only
// answer and fail an equally valid change of heart about which letters to
// take. Both are checked — the constraint over a wide input set, and the
// shapes we actually promise for ordinary names.
func TestDerivePrefix_IsAlwaysResolvable(t *testing.T) {
	names := []string{
		"TEMP Rook A 2870",   // the reported case
		"2026 Goals",         // leading numeric word
		"Q1 Objectives",      // digit inside a word
		"Sprint 42",          // trailing numeric word
		"v2 Roadmap",         // version-style word
		"Tasks",              // the ordinary case
		"Customer Feedback",  // ordinary multi-word
		"a-b_c d",            // every separator at once
		"  Padded   Name  ",  // surrounding and internal whitespace
		"Ünsicherheit",       // non-ASCII letters, single word
		"Ω Ψ",                // non-ASCII letters, multi-word
		"2026",               // digits only
		"— —",                // punctuation only
		"",                   // empty
		"Notes & Références", // mixed script with an ampersand word
	}

	for _, name := range names {
		t.Run(strings.TrimSpace(name), func(t *testing.T) {
			got := DerivePrefix(name)
			for i, r := range got {
				if r < 'A' || r > 'Z' {
					t.Fatalf("DerivePrefix(%q) = %q — character %d (%q) is not A-Z, so "+
						"parseItemRef will refuse every ref built from it", name, got, i, string(r))
				}
			}
			if len(got) > 5 {
				t.Errorf("DerivePrefix(%q) = %q, longer than the 5-char cap", name, got)
			}
		})
	}
}

// The shapes we promise for ordinary names, so "letters only" cannot be
// satisfied by returning "" for everything.
func TestDerivePrefix_OrdinaryNamesUnchanged(t *testing.T) {
	cases := map[string]string{
		"Tasks":             "TASK",
		"Ideas":             "IDEA",
		"Customer Feedback": "CF",
		"Bugs":              "BUG",
		"Conventions":       "CONVE",
	}
	for name, want := range cases {
		if got := DerivePrefix(name); got != want {
			t.Errorf("DerivePrefix(%q) = %q, want %q — the A-Z constraint must not change "+
				"what an ordinary name derives", name, got, want)
		}
	}
}

// A name with no ASCII letters yields "", which is the signal
// store.CreateCollection turns into its existing "ITEM" fallback. Pinned
// because the empty return is load-bearing rather than an oversight.
func TestDerivePrefix_LetterlessNamesYieldEmpty(t *testing.T) {
	for _, name := range []string{"2026", "— —", "  ", "42 42"} {
		if got := DerivePrefix(name); got != "" {
			t.Errorf("DerivePrefix(%q) = %q, want \"\" so the caller applies its ITEM fallback", name, got)
		}
	}
}
