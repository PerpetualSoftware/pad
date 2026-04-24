package server

import (
	"strings"
	"testing"
)

func TestDiffFieldsPrimitives(t *testing.T) {
	got := diffFields(
		`{"status":"open","priority":"medium"}`,
		`{"status":"in-progress","priority":"high"}`,
	)
	// Order is alphabetical (sort.Strings on the changes slice).
	want := "priority: medium → high, status: open → in-progress"
	if got != want {
		t.Errorf("diffFields primitives:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestDiffFieldsAddedRemovedFields(t *testing.T) {
	// Newly added field — old map missing the key.
	got := diffFields(`{"status":"open"}`, `{"status":"open","priority":"high"}`)
	want := "priority: → high"
	if got != want {
		t.Errorf("diffFields added:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestDiffFieldsImplementationNotesSummarised(t *testing.T) {
	// Add the first implementation note. Without the structured-field
	// formatting this used to render as
	//   `implementation_notes: → [map[created_at:... details:... summary:...]]`
	// which is the BUG-748 activity-card regression we're guarding against.
	old := `{"status":"open"}`
	updated := `{"status":"open","implementation_notes":[{"id":"n1","summary":"Phases 1-3 verified shipped","details":"Code audit on 2026-04-23 found Phase 1, 2, and most of Phase 3 already implemented","created_at":"2026-04-23T00:49:04Z","created_by":"user"}]}`

	got := diffFields(old, updated)
	want := "implementation_notes: → (1 note)"
	if got != want {
		t.Errorf("diffFields implementation_notes single:\n  got:  %q\n  want: %q", got, want)
	}

	// Confirm the raw map repr never leaks through.
	if strings.Contains(got, "map[") || strings.Contains(got, "details:") {
		t.Errorf("diffFields leaked Go map repr: %q", got)
	}
}

func TestDiffFieldsImplementationNotesPluralised(t *testing.T) {
	old := `{"implementation_notes":[{"summary":"first"}]}`
	updated := `{"implementation_notes":[{"summary":"first"},{"summary":"second"},{"summary":"third"}]}`

	got := diffFields(old, updated)
	want := "implementation_notes: (1 note) → (3 notes)"
	if got != want {
		t.Errorf("diffFields implementation_notes count change:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestDiffFieldsDecisionLogSummarised(t *testing.T) {
	old := `{"status":"active"}`
	updated := `{"status":"active","decision_log":[{"id":"d1","decision":"Store notes in reserved field keys","rationale":"Avoid a new table"}]}`

	got := diffFields(old, updated)
	want := "decision_log: → (1 entry)"
	if got != want {
		t.Errorf("diffFields decision_log single:\n  got:  %q\n  want: %q", got, want)
	}

	old2 := `{"decision_log":[{"decision":"first"}]}`
	updated2 := `{"decision_log":[{"decision":"first"},{"decision":"second"}]}`
	got2 := diffFields(old2, updated2)
	want2 := "decision_log: (1 entry) → (2 entries)"
	if got2 != want2 {
		t.Errorf("diffFields decision_log count change:\n  got:  %q\n  want: %q", got2, want2)
	}
}

func TestDiffFieldsGenericStructuredFieldsFallback(t *testing.T) {
	// An unknown array-of-objects field should still produce a summary,
	// not a raw map repr.
	old := `{"status":"open"}`
	updated := `{"status":"open","custom_attachments":[{"name":"a.png"},{"name":"b.png"}]}`

	got := diffFields(old, updated)
	want := "custom_attachments: → (2 items)"
	if got != want {
		t.Errorf("diffFields generic structured field:\n  got:  %q\n  want: %q", got, want)
	}
	if strings.Contains(got, "map[") {
		t.Errorf("diffFields generic structured field leaked Go map repr: %q", got)
	}
}

func TestDiffFieldsObjectFieldFallback(t *testing.T) {
	// A bare object field — also previously dumped as `map[k:v]`.
	old := `{"status":"open"}`
	updated := `{"status":"open","convention":{"trigger":"on-implement","scope":"all"}}`

	got := diffFields(old, updated)
	want := "convention: → (object)"
	if got != want {
		t.Errorf("diffFields object field:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestDiffFieldsHandlesInvalidJSON(t *testing.T) {
	if got := diffFields("not-json", `{"a":1}`); got != "" {
		t.Errorf("diffFields invalid old: expected empty, got %q", got)
	}
	if got := diffFields(`{"a":1}`, "not-json"); got != "" {
		t.Errorf("diffFields invalid new: expected empty, got %q", got)
	}
}

func TestFormatChangeValueNilSafe(t *testing.T) {
	if got := formatChangeValue("status", nil); got != "" {
		t.Errorf("formatChangeValue(nil): expected empty, got %q", got)
	}
}

// Regression for the Codex-round-1 finding on PR #236: collapsing slice
// values to a count-only label meant a same-cardinality replacement (one
// note swapped for a different one note) produced equal old/new display
// strings, so `diffFields()` silently dropped the change. The fix compares
// the decoded values via reflect.DeepEqual instead of the display strings,
// so a content change still produces a metadata.changes entry. The display
// string is still the friendly summary (`(1 note) → (1 note)`) — informative
// but coarse — which is acceptable: the activity card also shows the actor
// + timestamp, so the user knows something changed and can drill in.
func TestDiffFieldsSameCardinalityArrayChangeStillReported(t *testing.T) {
	old := `{"implementation_notes":[{"id":"n1","summary":"original"}]}`
	updated := `{"implementation_notes":[{"id":"n1","summary":"revised"}]}`

	got := diffFields(old, updated)
	want := "implementation_notes: (1 note) → (1 note)"
	if got != want {
		t.Errorf("diffFields same-cardinality replacement:\n  got:  %q\n  want: %q", got, want)
	}

	// Also verify same JSON on both sides produces no entry (true no-op).
	if got := diffFields(old, old); got != "" {
		t.Errorf("diffFields no-op: expected empty, got %q", got)
	}
}

// Regression for the Codex-round-1 finding on PR #236: object-valued fields
// (e.g. `convention`) now both stringify to `(object)`, so an in-place edit
// would have been silently dropped. Confirm DeepEqual catches it.
func TestDiffFieldsObjectMutationStillReported(t *testing.T) {
	old := `{"convention":{"trigger":"on-implement","scope":"all"}}`
	updated := `{"convention":{"trigger":"on-commit","scope":"all"}}`

	got := diffFields(old, updated)
	want := "convention: (object) → (object)"
	if got != want {
		t.Errorf("diffFields object mutation:\n  got:  %q\n  want: %q", got, want)
	}

	// Identical object — no-op, no entry.
	if got := diffFields(old, old); got != "" {
		t.Errorf("diffFields object no-op: expected empty, got %q", got)
	}
}
