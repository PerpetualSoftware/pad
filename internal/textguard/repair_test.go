package textguard

import (
	"encoding/json"
	"strings"
	"testing"
)

// The repair's contract is a property over the SAME corpus the four
// enforcement layers are measured against (DOC-2823 S3). Driving the repair
// through the corpus rather than through examples of its own is what stops it
// from being correct about a set of values nobody enforces.

// TestRepairSatisfiesTheCorpusBothWays is the whole contract in one test.
//
// Both directions matter and they fail differently. A repair that is not
// ACCEPTED afterwards has not repaired anything; a repair that is not the
// IDENTITY on accepted values has corrupted something nobody complained about,
// which is the harder failure to notice because every layer stays green.
func TestRepairSatisfiesTheCorpusBothWays(t *testing.T) {
	for _, c := range Corpus {
		t.Run(c.Name, func(t *testing.T) {
			got := Repair(c.Value, c.IsJSON)

			if ParameterRefused(got, c.IsJSON) {
				t.Errorf("repaired value is still refused\n  in:  %q\n  out: %q", c.Value, got)
			}

			if c.Refused {
				// A repair that returned its input unchanged would satisfy the
				// check above only by accident on the accepted cases, and
				// would silently do nothing here. Asserting the change is what
				// makes this leg fail for a no-op implementation.
				if got == c.Value {
					t.Errorf("refused value came back unchanged: %q", c.Value)
				}
			} else if got != c.Value {
				t.Errorf("accepted value was rewritten\n  in:  %q\n  out: %q", c.Value, got)
			}
		})
	}
}

// TestRepairAcceptsTheStoreOverRefusals covers the values only Layer A refuses.
//
// They are outside Corpus because the layers disagree about them, but the
// repair still has to produce something the store will accept — otherwise
// `pad db repair-nul` leaves behind exactly the rows the store's own classing
// would refuse to rewrite.
func TestRepairAcceptsTheStoreOverRefusals(t *testing.T) {
	for _, c := range StoreOverRefusals {
		t.Run(c.Name, func(t *testing.T) {
			// The store classes by COLUMN; these are text columns whose value
			// happens to parse as a document, so the store checks them as JSON.
			got := Repair(c.Value, true)
			if ParameterRefused(got, true) {
				t.Errorf("still refused by the store's classing\n  in:  %q\n  out: %q", c.Value, got)
			}
		})
	}
}

// TestRepairIsIdempotent pins that a second pass changes nothing.
//
// The scan and the repair are separate commands, so an operator re-running the
// repair after a partial run is expected, not exceptional; and a repair whose
// output it would itself flag is a repair that never terminates.
func TestRepairIsIdempotent(t *testing.T) {
	for _, c := range Corpus {
		once := Repair(c.Value, c.IsJSON)
		twice := Repair(once, c.IsJSON)
		if once != twice {
			t.Errorf("%s: not idempotent\n  once:  %q\n  twice: %q", c.Name, once, twice)
		}
	}
}

// TestRepairPreservesTheDocumentModuloNUL is the faithfulness half.
//
// "Accepted afterwards" is satisfied by a repair that returns `{}` for every
// document. This asserts the repaired document decodes to the SAME structure as
// the original, with every NUL — in a value or in a key — replaced by U+FFFD
// and nothing else touched.
func TestRepairPreservesTheDocumentModuloNUL(t *testing.T) {
	for _, c := range Corpus {
		if !c.IsJSON || !json.Valid([]byte(strings.TrimSpace(c.Value))) {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			var before, after any
			if err := json.Unmarshal([]byte(c.Value), &before); err != nil {
				t.Fatalf("decode original: %v", err)
			}
			repaired := Repair(c.Value, true)
			if err := json.Unmarshal([]byte(repaired), &after); err != nil {
				t.Fatalf("repaired value is not valid JSON: %v (%q)", err, repaired)
			}
			if !equalModuloNUL(before, after) {
				t.Errorf("repair changed more than the NULs\n  before: %#v\n  after:  %#v", before, after)
			}
		})
	}
}

// equalModuloNUL compares two decoded JSON values, treating a NUL on the left
// as equal to U+FFFD on the right and requiring everything else to match.
func equalModuloNUL(before, after any) bool {
	switch b := before.(type) {
	case string:
		a, ok := after.(string)
		return ok && strings.ReplaceAll(b, NUL, Replacement) == a
	case map[string]any:
		a, ok := after.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for k, bv := range b {
			av, present := a[strings.ReplaceAll(k, NUL, Replacement)]
			if !present || !equalModuloNUL(bv, av) {
				return false
			}
		}
		return true
	case []any:
		a, ok := after.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i := range b {
			if !equalModuloNUL(b[i], a[i]) {
				return false
			}
		}
		return true
	default:
		return before == after
	}
}

// TestRepairScannerEdges covers the shapes the corpus does not carry, because
// the corpus is about what the LAYERS disagree on and these are about what the
// SCANNER can misread.
func TestRepairScannerEdges(t *testing.T) {
	// Built, never typed — see corpus.go. A case that means the escape while
	// carrying the character is vacuous.
	esc := EscNUL
	backslash := esc[:1]

	cases := []struct {
		name   string
		in     string
		isJSON bool
		want   string
		why    string
	}{
		{
			name: "escape in a key AND a value", isJSON: true,
			in:   `{"k` + esc + `":"v` + esc + `"}`,
			want: `{"k` + ReplacementEscape + `":"v` + ReplacementEscape + `"}`,
			why:  "keys and values take the same path; a value-only rewrite leaves the document refused.",
		},
		{
			name: "doubled backslash then the escape text", isJSON: true,
			in:   `{"a":"x` + backslash + esc + `y"}`,
			want: `{"a":"x` + backslash + esc + `y"}`,
			why:  "literal text after an escaped backslash. This is the case a substring replace corrupts.",
		},
		{
			name: "escape immediately after another escape", isJSON: true,
			in:   `{"a":"` + backslash + `n` + esc + `"}`,
			want: `{"a":"` + backslash + `n` + ReplacementEscape + `"}`,
			why:  "the scanner must resume at the right offset after consuming a two-byte escape.",
		},
		{
			// THE DISCRIMINATING CASE, and the one the first draft of this test
			// did not have. A doubled-backslash literal ALONE never reaches the
			// scanner: Repair's guard skips a document that decodes to no NUL,
			// so a naive substring replace survived every leg here. It is only
			// when the same document ALSO carries a live escape that the
			// scanner runs over the literal — and that is the row an operator
			// actually has, since the literal is what a document about this bug
			// contains. Found by mutating the scanner to strings.ReplaceAll and
			// watching the suite stay green.
			name: "a live escape and a doubled-backslash literal in ONE document", isJSON: true,
			in:   `{"a":"x` + esc + `y","b":"lit` + backslash + esc + `eral"}`,
			want: `{"a":"x` + ReplacementEscape + `y","b":"lit` + backslash + esc + `eral"}`,
			why:  "the live escape is rewritten and the literal is not. A substring replace corrupts the second.",
		},
		{
			name: "the escape text OUTSIDE any string", isJSON: true,
			in:   `{"a":1}`,
			want: `{"a":1}`,
			why:  "control: structural bytes are copied verbatim and no rewrite fires.",
		},
		{
			name: "raw NUL and a live escape in one JSON value", isJSON: true,
			in:   `{"a":"x` + NUL + `y` + esc + `z"}`,
			want: `{"a":"x` + Replacement + `y` + ReplacementEscape + `z"}`,
			why:  "both defects in one value: the raw pass and the document pass must BOTH fire.",
		},
		{
			name: "escape inside a string that is not classed as JSON", isJSON: false,
			in:   `{"a":"x` + esc + `y"}`,
			want: `{"a":"x` + esc + `y"}`,
			why:  "classing decides. In a text column these are six characters and rewriting them is the false positive.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Repair(tc.in, tc.isJSON)
			if got != tc.want {
				t.Errorf("%s\n  in:   %q\n  got:  %q\n  want: %q", tc.why, tc.in, got, tc.want)
			}
		})
	}
}

// TestRepairConstantsDidNotDecay is the same guard corpus_test.go keeps over
// NUL and EscNUL, extended to the two this file adds.
//
// It compares against values it BUILDS rather than ones it types, because
// typing the six characters is what produces the character (measured: it
// happened twice while writing this unit).
func TestRepairConstantsDidNotDecay(t *testing.T) {
	if []rune(Replacement)[0] != 0xFFFD || len([]rune(Replacement)) != 1 {
		t.Errorf("Replacement is not a single U+FFFD: %q", Replacement)
	}
	if len(ReplacementEscape) != 6 ||
		ReplacementEscape[0] != '\\' || ReplacementEscape[1] != 'u' ||
		ReplacementEscape[2:] != "fffd" {
		t.Errorf("ReplacementEscape decayed: %q", ReplacementEscape)
	}
	// And the two must mean the same thing to a JSON parser.
	var viaEscape string
	if err := json.Unmarshal([]byte(`"`+ReplacementEscape+`"`), &viaEscape); err != nil {
		t.Fatalf("ReplacementEscape is not a valid JSON escape: %v", err)
	}
	if viaEscape != Replacement {
		t.Errorf("the escape and the character disagree: %q vs %q", viaEscape, Replacement)
	}
}
