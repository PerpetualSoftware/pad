package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// The HTTP gate's REPAIR leg (DOC-2823 S3). The other three are in
// internal/store, beside their own corpus legs.

// TestHTTPGateAcceptsEveryRepairedCorpusValue drives each repaired value
// through the gate exactly as the refusal leg drives the original, with the
// same key-derived classing.
func TestHTTPGateAcceptsEveryRepairedCorpusValue(t *testing.T) {
	for _, c := range textguard.Corpus {
		t.Run(c.Name, func(t *testing.T) {
			repaired := textguard.Repair(c.Value, c.IsJSON)

			key := "content"
			if c.IsJSON {
				key = "fields"
			}
			body, err := json.Marshal(map[string]any{key: repaired})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if bodyDecodesNUL(body) {
				t.Errorf("the gate refuses a REPAIRED value\n  original: %q\n  repaired: %q\n  body: %s\n"+
					"  why this case exists: %s", c.Value, repaired, body, c.Why)
			}
		})
	}
}

// TestImportRepairFlagRepairsExactlyTheEscape pins what the flag does to a
// body, at the level the handler consumes.
//
// The two negative halves are the load-bearing ones. A raw NUL BYTE makes the
// document invalid JSON, so repairing one would turn a body the decoder rejects
// into one it accepts — and widening what parses is not this flag's job. And a
// doubled-backslash literal decodes to no NUL at all, so rewriting it would
// corrupt a legitimate value.
func TestImportRepairFlagRepairsExactlyTheEscape(t *testing.T) {
	esc := textguard.EscNUL
	backslash := esc[:1]

	cases := []struct {
		name        string
		body        string
		wantChanged bool
		wantCount   int
		why         string
	}{
		{
			name:        "a live escape is replaced",
			body:        `{"content":"x` + esc + `y"}`,
			wantChanged: true, wantCount: 1,
			why: "the shape a pre-enforcement export actually carries.",
		},
		{
			name:        "two live escapes are both replaced and counted",
			body:        `{"content":"x` + esc + `y","title":"a` + esc + `b"}`,
			wantChanged: true, wantCount: 2,
			why: "the count is reported to the operator, so it has to be a count and not a flag.",
		},
		{
			name:        "a doubled-backslash literal is untouched",
			body:        `{"content":"x` + backslash + esc + `y"}`,
			wantChanged: false, wantCount: 0,
			why: "literal text after an escaped backslash. Rewriting it would corrupt a valid value.",
		},
		{
			name:        "a raw NUL byte is left to fail the decode",
			body:        `{"content":"x` + textguard.NUL + `y"}`,
			wantChanged: false, wantCount: 0,
			why: "invalid JSON. Repairing it would make a body parse that previously did not.",
		},
		{
			name:        "a clean body is untouched",
			body:        `{"content":"ordinary"}`,
			wantChanged: false, wantCount: 0,
			why: "the control. Without it a repair that rewrites everything passes every other case.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, n := repairBodyNULEscapes([]byte(tc.body))
			changed := string(got) != tc.body
			if changed != tc.wantChanged {
				t.Errorf("%s\n  body:     %q\n  repaired: %q\n  changed=%t, want %t",
					tc.why, tc.body, got, changed, tc.wantChanged)
			}
			if n != tc.wantCount {
				t.Errorf("replaced %d, want %d (%s)", n, tc.wantCount, tc.why)
			}
			// Whatever came back, the gate must accept it when the repair
			// claims to have fixed something — that is the whole contract.
			if tc.wantChanged && bodyDecodesNUL(got) {
				t.Errorf("the repaired body is still refused by the gate: %q", got)
			}
		})
	}
}

// TestImportStrictRefusalNamesTheWorkingRemedy is Dave's day-54 ruling 2 in
// test form, and PATTE-135's rule: a suggested remedy is an untested contract
// claim until something runs it.
//
// So this does not merely assert that the refusal message mentions a flag. It
// takes the EXACT body that produced the refusal, applies the named remedy, and
// asserts the gate then accepts it — which is the only version of this test
// that would fail if the message named a flag that did not work.
func TestImportStrictRefusalNamesTheWorkingRemedy(t *testing.T) {
	failing := []byte(`{"items":[{"content":"x` + textguard.EscNUL + `y"}]}`)

	// Precondition: strict really does refuse this body. Without it the test
	// could pass against a fixture that was never refused in the first place.
	if !bodyDecodesNUL(failing) {
		t.Fatalf("fixture does not reproduce the refusal: %s", failing)
	}

	strict := &nulRepairTally{Enabled: false}
	msg := nulRepairRemedy(strict)
	if !strings.Contains(msg, "--repair-nul") {
		t.Fatalf("the strict refusal does not name the flag: %q", msg)
	}
	if !strings.Contains(msg, "pad db repair-nul") {
		t.Errorf("the refusal does not name the database-side repair either: %q", msg)
	}

	// Now RUN the named remedy against the same body.
	withFlag := &nulRepairTally{Enabled: true}
	repaired := withFlag.Apply(failing)
	if bodyDecodesNUL(repaired) {
		t.Fatalf("the remedy the refusal names does not accept the body it was suggested for\n"+
			"  body:     %s\n  repaired: %s", failing, repaired)
	}
	if withFlag.Replaced != 1 {
		t.Errorf("replaced %d escapes, want 1", withFlag.Replaced)
	}

	// And the message an operator sees AFTER using the flag must not tell them
	// to use it again — at that point the repair has already been tried and did
	// not fix the value.
	afterMsg := nulRepairRemedy(withFlag)
	if strings.Contains(afterMsg, "Re-run the import with --repair-nul") {
		t.Errorf("the post-flag message repeats a remedy the caller already used: %q", afterMsg)
	}
	if !strings.Contains(afterMsg, "pad db repair-nul") {
		t.Errorf("the post-flag message gives no remaining course of action: %q", afterMsg)
	}
}

// TestNULRepairTallyDefaultsToStrict pins the direction a mistake falls in.
//
// A nil tally, or one nobody set the flag on, must leave the body alone — so a
// call site that forgets to thread the flag gets the strict behaviour rather
// than a silently repairing import.
func TestNULRepairTallyDefaultsToStrict(t *testing.T) {
	body := []byte(`{"content":"x` + textguard.EscNUL + `y"}`)

	var nilTally *nulRepairTally
	if got := nilTally.Apply(body); string(got) != string(body) {
		t.Errorf("a nil tally repaired the body: %q", got)
	}

	off := &nulRepairTally{}
	if got := off.Apply(body); string(got) != string(body) {
		t.Errorf("a disabled tally repaired the body: %q", got)
	}
	if off.Replaced != 0 {
		t.Errorf("a disabled tally counted %d replacements", off.Replaced)
	}
}

// TestRepairFlagReachesTheNestedAndObliqueForms is the codex round-1 finding
// turned into a test, and it is the reason the repair walks the DECODED body
// rather than scanning raw bytes.
//
// Both fixtures are shapes a REAL export carries and a raw scan cannot touch:
//
//   - `items.fields` travels as a STRING. The stored blob's live escape is
//     written into the body with a DOUBLED backslash, which at the body's own
//     layer is literal text — correctly left alone by a raw scan, and refused
//     by the gate anyway, because the gate re-parses that string as the
//     document it is. This is the most common carrier in a real export and the
//     first version of the flag could not repair it.
//   - The oblique spelling puts the backslash itself in as an escape, so the
//     six characters do not appear in the raw bytes at all (BUG-2803 round 4).
//     The decode resolves it; a scan never sees it.
//
// Each leg asserts the gate ACCEPTS the repaired body, which is the property
// the flag exists to deliver, and that the count is reported.
func TestRepairFlagReachesTheNestedAndObliqueForms(t *testing.T) {
	esc := textguard.EscNUL
	backslash := esc[:1]

	cases := []struct {
		name string
		body string
		why  string
	}{
		{
			name: "a live escape inside a fields blob carried as a string",
			body: `{"fields":"{\"a\":\"x` + backslash + esc + `y\"}"}`,
			why:  "what an export of an affected items.fields row actually looks like on the wire.",
		},
		{
			name: "the obliquely spelled escape inside a fields blob",
			body: `{"fields":"{\"a\":\"x` + backslash + `u005c` + `u0000y\"}"}`,
			why:  "the backslash written as its own escape; the six characters never appear in the body.",
		},
		{
			name: "a NUL in a KEY of a nested document",
			body: `{"fields":"{\"k` + backslash + esc + `ey\":\"v\"}"}`,
			why:  "keys are as fatal as values, and a value-only repair leaves the body refused.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.body)

			// Precondition: the gate refuses it. A fixture the gate accepts
			// would make every assertion below vacuous.
			if !bodyDecodesNUL(raw) {
				t.Fatalf("the gate does not refuse this fixture, so it measures nothing: %s", raw)
			}

			tally := &nulRepairTally{Enabled: true}
			repaired := tally.Apply(raw)

			if bodyDecodesNUL(repaired) {
				t.Fatalf("%s\n  the repair did not make the body acceptable\n  in:  %s\n  out: %s",
					tc.why, raw, repaired)
			}
			if tally.Replaced != 1 {
				t.Errorf("reported %d repaired value(s), want 1: %s", tally.Replaced, repaired)
			}
		})
	}
}

// TestRepairFlagLeavesALiteralAlone is the negative half of the test above, and
// the reason the repair cannot simply rewrite every `\u0000`-shaped run of
// characters it finds.
//
// A doubled backslash in a field the gate does NOT re-parse is six literal
// characters — corpus case 3 — and an accepted value. Rewriting it would
// corrupt a document that merely writes ABOUT this bug, which in a
// documentation tool is not hypothetical.
func TestRepairFlagLeavesALiteralAlone(t *testing.T) {
	esc := textguard.EscNUL
	backslash := esc[:1]
	body := []byte(`{"content":"x` + backslash + esc + `y"}`)

	if bodyDecodesNUL(body) {
		t.Fatalf("the gate refuses a literal; the control is broken: %s", body)
	}

	tally := &nulRepairTally{Enabled: true}
	got := tally.Apply(body)
	if string(got) != string(body) {
		t.Errorf("an accepted value was rewritten\n  in:  %s\n  out: %s", body, got)
	}
	if tally.Replaced != 0 {
		t.Errorf("reported %d repaired value(s) for a body with nothing to repair", tally.Replaced)
	}
}

// TestBodyRepairMirrorsTheGateOverTheCorpus is the guard on the one risk the
// decoded walk introduces: repairDecodedNULs and bodyDecodesNUL are two
// traversals of the same shape with the same classing, and a divergence between
// them is invisible to review.
//
// So it is measured rather than reviewed. Every corpus case is put into a body
// the way the gate's own leg puts it — classed by KEY — then repaired by the
// BODY path and handed back to the gate. Refused cases must come back accepted;
// accepted cases must come back byte-identical, which is what catches a walk
// that rewrites something nobody complained about.
func TestBodyRepairMirrorsTheGateOverTheCorpus(t *testing.T) {
	for _, c := range textguard.Corpus {
		t.Run(c.Name, func(t *testing.T) {
			key := "content"
			if c.IsJSON {
				key = "fields"
			}
			body, err := json.Marshal(map[string]any{key: c.Value})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			// The corpus's own verdict must be what the gate says about this
			// body, or the case is being measured in the wrong shape.
			if got := bodyDecodesNUL(body); got != c.Refused {
				t.Fatalf("gate refused=%t, corpus says %t — the fixture shape is wrong, not the repair",
					got, c.Refused)
			}

			repaired, n := repairBodyNULEscapes(body)

			if bodyDecodesNUL(repaired) {
				t.Errorf("the body repair left a value the gate still refuses\n  in:  %s\n  out: %s\n"+
					"  why this case exists: %s", body, repaired, c.Why)
			}
			if c.Refused {
				if n == 0 {
					t.Errorf("a refused body reported no repairs: %s", body)
				}
			} else {
				if n != 0 {
					t.Errorf("an accepted body reported %d repair(s): %s", n, repaired)
				}
				if string(repaired) != string(body) {
					t.Errorf("an accepted body was rewritten\n  in:  %s\n  out: %s", body, repaired)
				}
			}
		})
	}
}

// TestBodyRepairPreservesEverythingElse pins the parts of a body the repair
// must not disturb when it DOES re-encode.
//
// Re-encoding is the cost of walking the decoded body, and it is only paid on
// bodies that carry a NUL. What it must not cost is the rest of the payload: an
// import body is full of numbers (item_number, sort_order) and text, and an
// integer that came back as 1e+06 would be a silent data change nobody asked
// for. json.Number is why this passes; without UseNumber it does not.
func TestBodyRepairPreservesEverythingElse(t *testing.T) {
	body := []byte(`{"version":1,"big":9007199254740993,"ratio":1.5,"flag":true,"nothing":null,` +
		`"text":"a < b & c > d","list":[1,2,3],"content":"x` + textguard.EscNUL + `y"}`)

	repaired, n := repairBodyNULEscapes(body)
	if n != 1 {
		t.Fatalf("repaired %d values, want 1", n)
	}

	var got map[string]any
	dec := json.NewDecoder(strings.NewReader(string(repaired)))
	dec.UseNumber()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("repaired body does not decode: %v (%s)", err, repaired)
	}

	// The integer wider than float64 is the one that fails without UseNumber:
	// it comes back as 9007199254740992, off by one, with nothing to see.
	if s, _ := got["big"].(json.Number); s.String() != "9007199254740993" {
		t.Errorf("big = %v, want 9007199254740993 — the number went through float64", got["big"])
	}
	if s, _ := got["ratio"].(json.Number); s.String() != "1.5" {
		t.Errorf("ratio = %v, want 1.5", got["ratio"])
	}
	if got["flag"] != true {
		t.Errorf("flag = %v, want true", got["flag"])
	}
	if v, present := got["nothing"]; !present || v != nil {
		t.Errorf("nothing = %v (present=%v), want a present null", v, present)
	}
	// HTML-ish characters survive as themselves rather than as < escapes.
	// Both decode the same, but SetEscapeHTML(false) keeps the payload legible
	// for anyone who looks at it.
	if got["text"] != "a < b & c > d" {
		t.Errorf("text = %q, want the original", got["text"])
	}
	if !strings.Contains(string(repaired), "a < b & c > d") {
		t.Errorf("HTML-ish characters were escaped on re-encode: %s", repaired)
	}
	if want := "x" + textguard.Replacement + "y"; got["content"] != want {
		t.Errorf("content = %q, want %q", got["content"], want)
	}
}
