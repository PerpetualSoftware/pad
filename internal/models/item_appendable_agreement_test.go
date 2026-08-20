package models

// Codex round 2 on BUG-2627 part 2 — StructuredFieldIsAppendable must agree
// with the guard it describes, on every shape, not just the obvious two.
//
// The refusal message the server renders names `pad item note` as the remedy
// only when this predicate says an append would be accepted. If the predicate
// is more permissive than the guard, the message prescribes a command that
// refuses; if it is stricter, the message withholds a remedy that would have
// worked. Either way the caller is told something false, so "these two agree"
// is the actual contract — not "the predicate looks right".
//
// The first version of the predicate was a separate decode into
// []json.RawMessage, which looked equivalent and was not: `[1]` decodes as a
// list of raw messages and fails the guard's []ItemImplementationNote. That
// case is in the table below.

import (
	"encoding/json"
	"testing"
)

func TestStructuredFieldIsAppendableAgreesWithTheGuard(t *testing.T) {
	shapes := []struct {
		name  string
		value string // raw JSON for the key, "" = key absent
	}{
		{"key absent", ""},
		{"explicit null", `null`},
		{"empty array", `[]`},
		{"well-formed entries", `[{"id":"note-1","summary":"ok"}]`},
		{"entries missing every optional field", `[{}]`},
		// The defect shape: the array stored as a JSON-encoded STRING.
		{"encoded string", `"[{\"summary\":\"legacy\"}]"`},
		// The shapes the RawMessage version got wrong.
		{"array of numbers", `[1]`},
		{"array of strings", `["a"]`},
		{"array with a typed field of the wrong type", `[{"summary":{}}]`},
		{"object instead of array", `{"summary":"x"}`},
		{"bare number", `7`},
		{"bare true", `true`},
	}

	keys := []struct {
		key    string
		append func(string) error
	}{
		{
			key: ItemFieldImplementationNotes,
			append: func(fieldsJSON string) error {
				_, err := AppendImplementationNote(fieldsJSON, ItemImplementationNote{ID: "new", Summary: "s"})
				return err
			},
		},
		{
			key: ItemFieldDecisionLog,
			append: func(fieldsJSON string) error {
				_, err := AppendDecisionLogEntry(fieldsJSON, ItemDecisionLogEntry{ID: "new", Decision: "d"})
				return err
			},
		},
	}

	// Malformed OUTER blobs, which the table above cannot express because it
	// builds a well-formed object around the value. Codex round 4: the
	// predicate claimed these were appendable while the helpers error on the
	// same parse, so the refusal message prescribed a command that fails.
	for _, k := range keys {
		for _, fieldsJSON := range []string{`[]`, `not json`, `{"unterminated":`} {
			t.Run(k.key+"/malformed outer blob "+fieldsJSON, func(t *testing.T) {
				predicate := StructuredFieldIsAppendable(fieldsJSON, k.key)
				guardAccepts := k.append(fieldsJSON) == nil
				if predicate != guardAccepts {
					t.Fatalf("disagreement on fields=%q: predicate says appendable=%v, the append helper says %v",
						fieldsJSON, predicate, guardAccepts)
				}
			})
		}
	}

	for _, k := range keys {
		for _, shape := range shapes {
			t.Run(k.key+"/"+shape.name, func(t *testing.T) {
				blob := map[string]json.RawMessage{"status": json.RawMessage(`"open"`)}
				if shape.value != "" {
					blob[k.key] = json.RawMessage(shape.value)
				}
				encoded, err := json.Marshal(blob)
				if err != nil {
					t.Fatalf("marshal fixture: %v", err)
				}
				fieldsJSON := string(encoded)

				predicate := StructuredFieldIsAppendable(fieldsJSON, k.key)
				guardAccepts := k.append(fieldsJSON) == nil

				if predicate != guardAccepts {
					t.Fatalf("disagreement on %s = %s: predicate says appendable=%v, the append helper says %v — "+
						"the refusal message would name a remedy that does not match reality",
						k.key, shape.value, predicate, guardAccepts)
				}
			})
		}
	}
}

// TestAppendSurvivesAMalformedOuterBlob — Codex round 3, P2.
//
// parseMutableItemFields unmarshalled a literal `null` into a NIL map with no
// error, and both Append* helpers then assign into whatever they get back — so
// `pad item note` against an item whose fields column holds "null" PANICKED
// with "assignment to entry in nil map" instead of appending. Reproduced before
// the fix.
//
// The predicate says such an item is appendable, which is the right answer; it
// is the append that had to be made true. The other malformed outer shapes
// (an array, garbage) return a clean error and must keep doing so — a panic and
// an error are not interchangeable to a caller, and the whole point here is
// that a caller told "use pad item note" gets something it can act on.
func TestAppendSurvivesAMalformedOuterBlob(t *testing.T) {
	cases := []struct {
		name      string
		fields    string
		wantError bool
	}{
		{"literal null", `null`, false},
		{"empty string", ``, false},
		{"empty object", `{}`, false},
		{"array instead of object", `[]`, true},
		{"not json at all", `not json`, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A panic fails the test rather than taking the process with it.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("AppendImplementationNote panicked on fields=%q: %v", tc.fields, r)
				}
			}()

			out, err := AppendImplementationNote(tc.fields, ItemImplementationNote{ID: "n1", Summary: "s"})
			if tc.wantError {
				if err == nil {
					t.Fatalf("fields=%q: expected an error, got out=%q", tc.fields, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("fields=%q: unexpected error: %v", tc.fields, err)
			}
			if notes := ExtractItemImplementationNotes(out); len(notes) != 1 {
				t.Fatalf("fields=%q: expected the note to land, got %d entries in %q", tc.fields, len(notes), out)
			}
		})
	}
}

// TestStructuredFieldIsAppendableIgnoresForeignKeys pins the scoping: an
// unreadable value under one key must not make a DIFFERENT key look
// unappendable, or a decision_log refusal would blame implementation_notes.
func TestStructuredFieldIsAppendableIgnoresForeignKeys(t *testing.T) {
	fieldsJSON := `{"implementation_notes":"[{\"summary\":\"broken\"}]","decision_log":[]}`

	if StructuredFieldIsAppendable(fieldsJSON, ItemFieldImplementationNotes) {
		t.Error("implementation_notes is the broken key and must read as unappendable")
	}
	if !StructuredFieldIsAppendable(fieldsJSON, ItemFieldDecisionLog) {
		t.Error("decision_log is well-formed; a sibling key's damage must not implicate it")
	}
	// A key with no append helper can never be refused by one.
	if !StructuredFieldIsAppendable(fieldsJSON, ItemFieldGitHubPR) {
		t.Error("github_pr has no append helper, so nothing can refuse an append to it")
	}
}
