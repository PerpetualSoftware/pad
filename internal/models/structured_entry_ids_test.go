package models

import (
	"encoding/json"
	"strings"
	"testing"
)

// BUG-2788: the repair gives an id to exactly the entries the timeline would
// otherwise number by position, and leaves everything else untouched.

func entryIDs(t *testing.T, fieldsJSON, key string) []any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &m); err != nil {
		t.Fatalf("decode %s: %v", fieldsJSON, err)
	}
	list, _ := m[key].([]any)
	var out []any
	for _, e := range list {
		if obj, ok := e.(map[string]any); ok {
			out = append(out, obj["id"])
		} else {
			out = append(out, e)
		}
	}
	return out
}

func TestEnsureStructuredEntryIDs(t *testing.T) {
	const u = "0b7c5a52-3f0e-4f7a-9c1e-0c1d2e3f4a5b"
	in := `{"status":"open","n":12345678901234567890,"f":1.0,` +
		`"implementation_notes":[` +
		`{"summary":"no id"},` + // absent → minted
		`{"id":"keep","summary":"a","extra":"kept"},` + // kept, unknown key survives
		`{"id":"keep","summary":"dup"},` + // duplicate → minted; FIRST keeps
		`{"id":"` + u + `","summary":"uuid","html":"a<b>&c"}` + // diverted, usable → kept; HTML not escaped
		`],"decision_log":[` +
		`{"id":"keep","decision":"cross-kind dup of a kept raw id"},` + // claimed by a note → minted
		`{"id":"` + u + `","decision":"same uuid, other kind"},` + // derives decision:<u> → distinct → kept
		`{"id":"note:x","decision":"prefixed"}` + // diverted to decision:note:x → kept
		`]}`

	out, changed, err := EnsureStructuredEntryIDs(in)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	notes := entryIDs(t, out, ItemFieldImplementationNotes)
	decs := entryIDs(t, out, ItemFieldDecisionLog)

	minted := func(v any, prefix string) bool {
		s, ok := v.(string)
		return ok && strings.HasPrefix(s, prefix+"-") && !strings.Contains(s, "-idx-")
	}
	if !minted(notes[0], "note") {
		t.Errorf("absent id: got %v, want a minted note id", notes[0])
	}
	if notes[1] != "keep" || notes[3] != u {
		t.Errorf("kept entries changed: %v", notes)
	}
	if !minted(notes[2], "note") {
		t.Errorf("duplicate: got %v, want a minted id (the first holder keeps \"keep\")", notes[2])
	}
	if !minted(decs[0], "decision") || decs[1] != u || decs[2] != "note:x" {
		t.Errorf("decisions = %v", decs)
	}
	// Unknown keys and number literals survive the rewrite byte for byte.
	for _, want := range []string{`"extra":"kept"`, `12345678901234567890`, `"f":1.0`, `"html":"a<b>&c"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lost %s: %s", want, out)
		}
	}

	// Every usable key is now unique across both kinds, which is what keeps
	// the timeline off its positional fallback.
	seen := map[string]bool{}
	for i, list := range [][]any{notes, decs} {
		prefix := structuredEntryPrefixes[i]
		for _, v := range list {
			s, ok := v.(string)
			if !ok {
				continue
			}
			k, usable := StructuredEntryIDKey(s, prefix)
			if !usable || seen[k] {
				t.Errorf("%s id %q: usable=%v, duplicate=%v", prefix, s, usable, seen[k])
			}
			seen[k] = true
		}
	}

	// Idempotent: a second pass changes nothing and returns the input.
	again, changed2, err := EnsureStructuredEntryIDs(out)
	if err != nil || changed2 || again != out {
		t.Errorf("second pass: changed=%v err=%v", changed2, err)
	}
}

func TestEnsureStructuredEntryIDsLeavesACleanBlobAlone(t *testing.T) {
	for _, in := range []string{``, `{}`, `{"status":"open"}`, `not json`,
		`{"implementation_notes":[{"id":"a","summary":"x"}],"decision_log":[{"id":"b","decision":"y"}]}`} {
		out, changed, err := EnsureStructuredEntryIDs(in)
		if err != nil || changed || out != in {
			t.Errorf("%q: out=%q changed=%v err=%v", in, out, changed, err)
		}
	}
}

// The append helpers repair an idless SIBLING when they re-marshal the array.
func TestAppendRepairsIdlessSiblings(t *testing.T) {
	in := `{"implementation_notes":[{"summary":"legacy"}]}`
	out, err := AppendImplementationNote(in, ItemImplementationNote{ID: NewStructuredEntryID("note"), Summary: "new"})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	ids := entryIDs(t, out, ItemFieldImplementationNotes)
	if len(ids) != 2 {
		t.Fatalf("ids = %v", ids)
	}
	if s, _ := ids[0].(string); s == "" {
		t.Errorf("the idless legacy sibling is still idless after an append: %s", out)
	}
	outD, err := AppendDecisionLogEntry(`{"decision_log":[{"decision":"legacy"}]}`, ItemDecisionLogEntry{ID: NewStructuredEntryID("decision"), Decision: "new"})
	if err != nil {
		t.Fatalf("append decision: %v", err)
	}
	if s, _ := entryIDs(t, outD, ItemFieldDecisionLog)[0].(string); s == "" {
		t.Errorf("the idless legacy decision is still idless after an append: %s", outD)
	}
}

// Round-1 review (BUG-2788): the repair touches only what the timeline can
// read, and only a blob that is exactly one JSON value.
func TestEnsureStructuredEntryIDsSkipsWhatTheTimelineCannotRead(t *testing.T) {
	for name, in := range map[string]string{
		"non-object element":   `{"implementation_notes":[{"summary":"idless"},"not-an-object"]}`,
		"non-string id":        `{"implementation_notes":[{"summary":"idless"},{"id":7,"summary":"numeric"}]}`,
		"not an array":         `{"implementation_notes":{"summary":"idless"}}`,
		"trailing bytes":       `{"implementation_notes":[{"summary":"idless"}]} trailing`,
		"top-level not object": `[{"summary":"idless"}]`,
	} {
		out, changed, err := EnsureStructuredEntryIDs(in)
		if err != nil || changed || out != in {
			t.Errorf("%s: changed=%v err=%v out=%q, want the input untouched", name, changed, err, out)
		}
		// Control: the timeline really does show nothing for the kind.
		if name != "trailing bytes" && name != "top-level not object" {
			if got := ExtractItemImplementationNotes(in); len(got) != 0 {
				t.Errorf("%s: control failed, the timeline extraction reads %d notes", name, len(got))
			}
		}
	}
}
