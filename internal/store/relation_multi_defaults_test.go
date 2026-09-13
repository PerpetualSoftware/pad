package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// A `multi_relation` DEFAULT, and `required` on one (PLAN-2857 U4, codex round
// 1's P2 and P3).
//
// Both were the same defect in two places: a gate reading `def.Type !=
// "relation"`. `ValidateFields` assigns a schema default and skips its own type
// check, so the late pass is the ONLY thing between an injected default and the
// blob — and it was skipping arrays entirely.

func multiDefaultSchema(def any, required bool) models.CollectionSchema {
	return models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "color", Label: "Colour", Type: "multi_relation", Collection: "colors", Default: def, Required: required},
	}}
}

func TestLateMultiRelationDefault_NonArrayIsDroppedNotStored(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, _ := relationFixture(t, s)

	// The shape a schema author can actually write and nothing else refuses.
	fields := map[string]any{"color": 42}
	dropped, err := s.ResolveLateRelationDefaults(nil, ws.ID, multiDefaultSchema(42, false), fields, map[string]bool{})
	if err != nil {
		t.Fatalf("late defaults: %v", err)
	}
	if _, present := fields["color"]; present {
		t.Errorf("color survived as %#v; a default in a shape the resolver cannot use must not reach the row", fields["color"])
	}
	if len(dropped) != 1 || dropped[0].Reason != RelationTargetInvalidShape {
		t.Errorf("dropped = %+v, want one invalid_shape — a silent delete leaves the schema author with no signal", dropped)
	}
}

func TestLateMultiRelationDefault_RefsAreCanonicalisedInOrder(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, colors, _, red := relationFixture(t, s)
	blue := createTestItem(t, s, ws.ID, colors.ID, "Blue", "")

	// The positive half, and the one that says the pass does more than delete:
	// a valid default has to land as canonical ids, or the row holds refs that
	// stop meaning anything the moment an item is renumbered.
	fields := map[string]any{"color": []any{blue.Ref, red.Ref}}
	dropped, err := s.ResolveLateRelationDefaults(nil, ws.ID, multiDefaultSchema([]any{blue.Ref, red.Ref}, false), fields, map[string]bool{})
	if err != nil {
		t.Fatalf("late defaults: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %+v, want none for a resolvable default", dropped)
	}
	arr, ok := fields["color"].([]any)
	if !ok || len(arr) != 2 || arr[0] != blue.ID || arr[1] != red.ID {
		t.Errorf("color = %#v, want both canonicalised in the declared order", fields["color"])
	}
}

func TestLateMultiRelationDefault_UnresolvableElementDropsTheKey(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)

	// A default is asserted by nobody, so it is DROPPED and reported, never
	// refused — otherwise one bad default in a schema makes every write into
	// that collection fail, on a defect its author has to fix elsewhere.
	fields := map[string]any{"color": []any{red.Ref, "COLO-9999"}}
	dropped, err := s.ResolveLateRelationDefaults(nil, ws.ID, multiDefaultSchema([]any{red.Ref, "COLO-9999"}, false), fields, map[string]bool{})
	if err != nil {
		t.Fatalf("late defaults: %v", err)
	}
	if _, present := fields["color"]; present {
		t.Errorf("color survived as %#v; a partially resolvable default is not a value anyone chose", fields["color"])
	}
	if len(dropped) != 1 {
		t.Errorf("dropped = %+v, want one", dropped)
	}
}

func TestLateMultiRelationDefault_EmptyArrayBecomesAnAbsentKey(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, _ := relationFixture(t, s)

	// The injected-default path is the one route by which `[]` can reach the
	// blob after `normalizeEmptyRelationLists` has run, since defaults are
	// assigned DURING the traversal that normalisation precedes. One spelling
	// of none, or every consumer has to handle two.
	fields := map[string]any{"color": []any{}}
	dropped, err := s.ResolveLateRelationDefaults(nil, ws.ID, multiDefaultSchema([]any{}, false), fields, map[string]bool{})
	if err != nil {
		t.Fatalf("late defaults: %v", err)
	}
	if _, present := fields["color"]; present {
		t.Errorf("color survived as %#v, want the key absent", fields["color"])
	}
	if len(dropped) != 0 {
		t.Errorf("dropped = %+v; an empty default is not a defect to report, it is a field with no value", dropped)
	}
}

func TestLateMultiRelationDefault_ControlAKeyPresentBeforeValidationIsNotADefault(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)

	// Without this, a pass that resolved EVERY relation key would satisfy the
	// legs above while quietly taking over the caller-input path, where an
	// unresolvable value must REFUSE rather than drop.
	fields := map[string]any{"color": []any{"COLO-9999"}}
	dropped, err := s.ResolveLateRelationDefaults(nil, ws.ID, multiDefaultSchema([]any{red.Ref}, false), fields, map[string]bool{"color": true})
	if err != nil {
		t.Fatalf("late defaults: %v", err)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped = %+v, want none: this key was present before validation, so it is the caller's value and not a default", dropped)
	}
	if _, present := fields["color"]; !present {
		t.Error("the caller's own value was deleted by the DEFAULTS pass")
	}
}

func TestDropInvisibleMultiRelationDefault_OneUnseenElementDropsTheKey(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, colors, _, red := relationFixture(t, s)
	blue := createTestItem(t, s, ws.ID, colors.ID, "Blue", "")

	// A whole-key drop, not an element drop: a list one element shorter than
	// its schema says is a value nobody chose. The SECOND element is the hidden
	// one so a first-element-only loop fails here.
	fields := map[string]any{"color": []any{blue.ID, red.ID}}
	dropped, err := s.DropInvisibleRelationDefaultsQ(s.Q(), ws.ID, hides(red.ID), multiDefaultSchema([]any{blue.ID, red.ID}, false), fields, map[string]bool{})
	if err != nil {
		t.Fatalf("drop invisible: %v", err)
	}
	if _, present := fields["color"]; present {
		t.Errorf("color survived as %#v with an element the requester cannot see", fields["color"])
	}
	if len(dropped) != 1 || dropped[0].Reason != RelationTargetNotFound {
		t.Errorf("dropped = %+v, want one not_found", dropped)
	}
}

func TestDropInvisibleMultiRelationDefault_ControlAllVisibleSurvives(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, colors, _, red := relationFixture(t, s)
	blue := createTestItem(t, s, ws.ID, colors.ID, "Blue", "")

	fields := map[string]any{"color": []any{blue.ID, red.ID}}
	dropped, err := s.DropInvisibleRelationDefaultsQ(s.Q(), ws.ID, hides("no-such-id"), multiDefaultSchema([]any{blue.ID, red.ID}, false), fields, map[string]bool{})
	if err != nil {
		t.Fatalf("drop invisible: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %+v, want none when every element is visible", dropped)
	}
	arr, _ := fields["color"].([]any)
	if len(arr) != 2 {
		t.Errorf("color = %#v, want both elements kept", fields["color"])
	}
}

func TestRequiredRelationIssues_CoversTheMultiType(t *testing.T) {
	t.Parallel()
	// The promotion that turns a dropped value into a door's missing-required
	// refusal. Gated on `def.Type == "relation"`, a required multi_relation had
	// no required enforcement anywhere: the key is deleted after validation has
	// already passed, so nothing re-checks it and the item lands without it.
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "color", Type: "multi_relation", Collection: "colors", Required: true},
		{Key: "owner", Type: "relation", Collection: "people", Required: false},
	}}
	issues := []RelationIssue{
		{Key: "color", Reason: RelationTargetNotFound},
		{Key: "owner", Reason: RelationTargetNotFound},
	}
	got := RequiredRelationIssues(schema, issues)
	if len(got) != 1 || got[0].Key != "color" {
		t.Errorf("promoted %+v, want only the required multi_relation — the optional scalar is the control that says this filters rather than passes everything", got)
	}
}

// CODEX ROUND 2 — all three findings were in round 1's FIXES, not in the
// original code. Treating a correction as new code that deserves the same
// adversarial pass is the lesson; these are its instruments.

func TestLateMultiRelationDefault_NonStringElementDropsTheWholeDefault(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, _ := relationFixture(t, s)

	// Round 1's reader mapped a non-string element to "" and the resolver SKIPS
	// a blank element, so `default: [42]` was accepted, stored as [""] and
	// reported to nobody. A list is ONE value: an unusable element makes the
	// default unusable, exactly as a non-array does.
	fields := map[string]any{"color": []any{42}}
	dropped, err := s.ResolveLateRelationDefaults(nil, ws.ID, multiDefaultSchema([]any{42}, false), fields, map[string]bool{})
	if err != nil {
		t.Fatalf("late defaults: %v", err)
	}
	if v, present := fields["color"]; present {
		t.Errorf("color survived as %#v, want the key gone — [\"\"] is a list of one reference to nothing", v)
	}
	if len(dropped) != 1 || dropped[0].Reason != RelationTargetInvalidShape {
		t.Errorf("dropped = %+v, want one invalid_shape", dropped)
	}
}

func TestLateMultiRelationDefault_BlankElementDropsTheWholeDefault(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)

	// The same rule `ValidateFields` applies to a CALLER's array: a blank
	// element is refused rather than skipped, because an ordered list whose
	// length depends on which elements were blank is not a list. A default is
	// held to the same shape and differs only in disposition.
	fields := map[string]any{"color": []any{red.Ref, "   "}}
	dropped, err := s.ResolveLateRelationDefaults(nil, ws.ID, multiDefaultSchema([]any{red.Ref, "   "}, false), fields, map[string]bool{})
	if err != nil {
		t.Fatalf("late defaults: %v", err)
	}
	if v, present := fields["color"]; present {
		t.Errorf("color survived as %#v", v)
	}
	if len(dropped) != 1 || dropped[0].Reason != RelationTargetInvalidShape {
		t.Errorf("dropped = %+v, want one invalid_shape", dropped)
	}
}

func TestLateMultiRelationDefault_EmptyDefaultOnARequiredFieldIsReported(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, _ := relationFixture(t, s)

	// Validation was satisfied by the PRESENCE of this default; deleting it
	// afterwards with no issue left a required field absent and nothing to
	// refuse it. `RequiredRelationIssues` promotes by key, so an issue is the
	// only way the door hears about it.
	fields := map[string]any{"color": []any{}}
	dropped, err := s.ResolveLateRelationDefaults(nil, ws.ID, multiDefaultSchema([]any{}, true), fields, map[string]bool{})
	if err != nil {
		t.Fatalf("late defaults: %v", err)
	}
	if len(dropped) != 1 {
		t.Fatalf("dropped = %+v, want one — an empty default cannot satisfy a required field", dropped)
	}
	if len(RequiredRelationIssues(multiDefaultSchema([]any{}, true), dropped)) != 1 {
		t.Error("the issue was not promoted by RequiredRelationIssues, so no door will refuse the write")
	}
}

func TestLateMultiRelationDefault_ControlEmptyDefaultOnAnOptionalFieldIsSilent(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, _ := relationFixture(t, s)

	// The other half, and the reason the leg above is conditional rather than
	// unconditional: on an optional field an empty default is a sensible way to
	// say "starts with nothing", and a drop warning would be a false alarm on a
	// schema doing nothing wrong.
	fields := map[string]any{"color": []any{}}
	dropped, err := s.ResolveLateRelationDefaults(nil, ws.ID, multiDefaultSchema([]any{}, false), fields, map[string]bool{})
	if err != nil {
		t.Fatalf("late defaults: %v", err)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped = %+v, want none", dropped)
	}
}

func TestRelationKeysPresent_AnEmptyListIsNotCallerInput(t *testing.T) {
	t.Parallel()
	// The ORDER bug, and the sharpest of the three: every door captures
	// provenance BEFORE ValidateFields runs, so this function sees the `[]` the
	// normalisation is about to remove. Counting it as caller input marked the
	// key "not a default"; validation then injected the schema default into the
	// hole normalisation had made; and the late pass skipped the key on the
	// strength of that mark. A caller sending `members: []` against
	// `default: 42` stored 42.
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "color", Type: "multi_relation", Collection: "colors"},
		{Key: "owner", Type: "relation", Collection: "people"},
	}}
	before := RelationKeysPresent(schema, map[string]any{"color": []any{}, "owner": ""})
	if before["color"] {
		t.Error("an empty list counted as caller input; the default injected in its place is then invisible to every later pass")
	}
	// CONTROLS. A non-empty list IS a value, and so is an empty STRING on a
	// scalar relation — that type's empty spelling has its own meaning
	// (BUG-3028) and this rule must not reach it.
	if !before["owner"] {
		t.Error("an empty scalar relation value stopped counting as caller input; that is a different type's rule")
	}
	populated := RelationKeysPresent(schema, map[string]any{"color": []any{"COLO-1"}})
	if !populated["color"] {
		t.Error("a populated list stopped counting as caller input")
	}
}
