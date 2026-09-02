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

// TestRepairFlagDoesNotReachTheObliqueEscape pins the limit the post-flag
// message now claims, rather than leaving it as prose.
//
// A backslash spelled as its OWN escape (u005c) followed by the text u0000
// decodes to the six-character NUL escape, which a nested document then
// re-parses into a real NUL — the oblique spelling BUG-2803's round 4 found,
// and the reason the gate walks the DECODED body rather than searching raw
// bytes. A document-level rewrite cannot reach it: at the layer the repair
// scans, those bytes are an escaped backslash followed by ordinary text.
//
// So the flag must NOT accept this body, and the message it produces must not
// tell the operator the cause is a raw NUL byte, which it is not.
func TestRepairFlagDoesNotReachTheObliqueEscape(t *testing.T) {
	backslash := textguard.EscNUL[:1]
	oblique := backslash + "u005c" + "u0000"
	body := []byte(`{"fields":"{\"a\":\"x` + oblique + `y\"}"}`)

	// Precondition: the gate refuses it. If it does not, this test is measuring
	// a body that was never a problem.
	if !bodyDecodesNUL(body) {
		t.Fatalf("the gate does not refuse the oblique fixture; it proves nothing: %s", body)
	}

	tally := &nulRepairTally{Enabled: true}
	repaired := tally.Apply(body)
	if tally.Replaced != 0 {
		t.Errorf("the repair claims to have rewritten %d escape(s) in a body that carries none at its "+
			"own layer: %s", tally.Replaced, repaired)
	}
	if !bodyDecodesNUL(repaired) {
		t.Fatalf("the repair made the oblique body acceptable — it must not, since the value it would " +
			"have to rewrite lives one decode deeper than the document it scans")
	}

	msg := nulRepairRemedy(tally)
	if strings.Contains(msg, "NUL byte") {
		t.Errorf("the post-flag message names a cause it cannot know — this value carries an escape, "+
			"not a raw byte: %q", msg)
	}
	if !strings.Contains(msg, "pad db repair-nul") {
		t.Errorf("the post-flag message leaves the operator with no course of action: %q", msg)
	}
}
