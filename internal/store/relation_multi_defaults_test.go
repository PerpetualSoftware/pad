package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/items"
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

// CODEX ROUND 3. Two of the three were again in the previous round's fixes; the
// third is a pre-existing inconsistency the empty-list rule made visible.

func TestMigrateMultiRelation_ABlankDefaultIsRemovedRatherThanLeftToFailValidation(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, _ := relationFixture(t, s)
	schema := multiDefaultSchema([]any{"   "}, false)

	// The migrate doors validate AFTER resolving, so a value merely SKIPPED
	// here stays in the map and fails the shape check — returning 400 on an
	// ordinary move while the same schema default on a CREATE is dropped and
	// the write succeeds. Same value, same schema, opposite answers.
	fields := map[string]any{"color": []any{"   "}}
	refusals, dropped, err := s.MigrateRelationReferents(nil, ws.ID, schema, fields, nil, nil, RelationCarryMode(0))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(refusals) != 0 {
		t.Errorf("refusals = %+v; a default is asserted by nobody and must never refuse a move", refusals)
	}
	if v, present := fields["color"]; present {
		t.Errorf("color survived as %#v, so ValidateFields refuses the move on a value the caller never sent", v)
	}
	_ = dropped
	// The point is the ABSENCE from the map, not the drop list: a blank list is
	// a cleared field, and reporting a drop for a field that held nothing tells
	// the user they lost something they never had.
	if err := validateMigrated(t, fields, schema); err != nil {
		t.Errorf("the migrated map still fails validation: %v", err)
	}
}

func TestMigrateMultiRelation_ControlAScalarClearedValueIsUntouched(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, _ := relationFixture(t, s)
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "owner", Type: "relation", Collection: "colors"},
	}}

	// `""` is a legal stored spelling for a cleared SCALAR relation and
	// validation accepts it, so removing that key would change what a move
	// stores. The removal above is scoped to the list type on purpose.
	fields := map[string]any{"owner": ""}
	if _, _, err := s.MigrateRelationReferents(nil, ws.ID, schema, fields, nil, nil, RelationCarryMode(0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v, present := fields["owner"]; !present || v != "" {
		t.Errorf("owner = %#v (present=%v), want the empty string kept", v, present)
	}
}

func TestHydrateMultiRelation_AnEmptyListDoesNotDependOnTheRestOfTheBatch(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, colors, _, red := relationFixture(t, s)
	crews, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Crews",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open","required":true},{"key":"color","type":"multi_relation","collection":"colors"}]}`,
	})
	if err != nil {
		t.Fatalf("create crews: %v", err)
	}
	empty, err := s.CreateItem(ws.ID, crews.ID, models.ItemCreate{Title: "Empty", Fields: `{"color":[]}`})
	if err != nil {
		t.Fatalf("create empty: %v", err)
	}
	populated, err := s.CreateItem(ws.ID, crews.ID, models.ItemCreate{Title: "Populated", Fields: `{"color":["` + red.ID + `"]}`})
	if err != nil {
		t.Fatalf("create populated: %v", err)
	}
	_ = colors

	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "done"}, Default: "open", Required: true},
		{Key: "color", Type: "multi_relation", Collection: "colors"},
	}}
	schemas := map[string]models.CollectionSchema{crews.ID: schema}
	alone, err := s.HydrateRelationTargetsQ(s.DB(), ws.ID, []models.Item{*empty}, schemas)
	if err != nil {
		t.Fatalf("hydrate alone: %v", err)
	}
	together, err := s.HydrateRelationTargetsQ(s.DB(), ws.ID, []models.Item{*empty, *populated}, schemas)
	if err != nil {
		t.Fatalf("hydrate together: %v", err)
	}

	// IDENTICAL STORED BYTES, so identical output. The early return made the
	// answer depend on whether some OTHER item in the batch happened to carry a
	// resolvable reference.
	setAlone, okAlone := alone[empty.ID]["color"]
	setTogether, okTogether := together[empty.ID]["color"]
	if okAlone != okTogether {
		t.Fatalf("entry present alone=%v, together=%v — the same row hydrated two ways", okAlone, okTogether)
	}
	if !okAlone {
		t.Fatal("an empty list got no entry at all; absent says \"nothing hydrated this field\", which is a different and false statement")
	}
	if setAlone.List == nil || len(setAlone.List) != 0 {
		t.Errorf("alone = %+v, want an empty LIST", setAlone)
	}
	if setTogether.List == nil || len(setTogether.List) != 0 {
		t.Errorf("together = %+v, want an empty LIST", setTogether)
	}
	// CONTROL: the populated item still hydrates, so this is not a batch that
	// silently produced nothing for everyone.
	if got := together[populated.ID]["color"]; got.List == nil || len(got.List) != 1 || got.List[0].Title != "Red" {
		t.Errorf("the populated item hydrated as %+v, want one Red target", got)
	}
}

// validateMigrated runs the shape check the migrate doors run after resolving,
// which is the step this fix exists to keep from failing.
func validateMigrated(t *testing.T, fields map[string]any, schema models.CollectionSchema) error {
	t.Helper()
	return items.ValidateFields(fields, schema)
}

// CODEX ROUND 4 — both findings were in round 3's fix. The deletion it added ran
// BEFORE the origin split, so it treated a caller's malformed override exactly
// like a schema's malformed default. Those get opposite dispositions on purpose.

func TestMigrateMultiRelation_ASuppliedBlankOverrideIsLeftForValidationToRefuse(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, _ := relationFixture(t, s)
	schema := multiDefaultSchema(nil, false)

	// `[" "]` is not a cleared field — it is a list of ONE reference that
	// happens to be blank, a shape every write door refuses. Round 3's fix
	// counted it as cleared, so a move with `field_overrides: {"color":[" "]}`
	// returned 200 with the field silently absent: the caller's malformed value
	// vanished instead of being refused.
	supplied := map[string]any{"color": []any{"   "}}
	fields := map[string]any{"color": []any{"   "}}
	refusals, dropped, err := s.MigrateRelationReferents(nil, ws.ID, schema, fields, supplied, nil, RelationCarryMode(0))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped = %+v; a value the CALLER supplied is refused, never dropped", dropped)
	}
	if _, present := fields["color"]; !present {
		t.Fatal("the caller's own override was deleted; nothing downstream can refuse a key that is gone, so the move succeeds with the field absent")
	}
	// The refusal itself comes from the shape check the migrate doors run after
	// this pass — asserted here so the leg above is about a value that really
	// does get refused, rather than one merely left lying around.
	if err := validateMigrated(t, fields, schema); err == nil {
		t.Error("the surviving value passes validation, so leaving it refuses nothing")
	}
	_ = refusals
}

func TestMigrateMultiRelation_AnEmptySuppliedListIsStillACLEAR(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, _ := relationFixture(t, s)
	schema := multiDefaultSchema(nil, false)

	// The other side of the narrowing, and the reason it is a narrowing rather
	// than a reversal: `[]` IS the agreed spelling of "no targets", from any
	// origin, and must still clear.
	supplied := map[string]any{"color": []any{}}
	fields := map[string]any{"color": []any{}}
	if _, _, err := s.MigrateRelationReferents(nil, ws.ID, schema, fields, supplied, nil, RelationCarryMode(0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v, present := fields["color"]; present {
		t.Errorf("color survived as %#v, want the key gone", v)
	}
}

func TestMigrateMultiRelation_AMixedBlankDefaultIsDropped(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)
	schema := multiDefaultSchema([]any{red.ID, "   "}, false)

	// `[validID, " "]` is not cleared (it has a real element) and it IS an array
	// of strings, so the lenient shape check passed it, resolution skipped the
	// blank, and the shape check afterwards refused it — a 400 on an optional
	// field, for a default the caller never wrote. A default is dropped, and a
	// drop has to happen while there is still something to drop.
	fields := map[string]any{"color": []any{red.ID, "   "}}
	refusals, dropped, err := s.MigrateRelationReferents(nil, ws.ID, schema, fields, nil, nil, RelationCarryMode(0))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(refusals) != 0 {
		t.Errorf("refusals = %+v; a destination default never refuses a copy", refusals)
	}
	if len(dropped) != 1 || dropped[0].Reason != RelationTargetInvalidShape {
		t.Errorf("dropped = %+v, want one invalid_shape", dropped)
	}
	if v, present := fields["color"]; present {
		t.Errorf("color survived as %#v, so the write is refused after this pass", v)
	}
	if err := validateMigrated(t, fields, schema); err != nil {
		t.Errorf("the migrated map still fails validation: %v", err)
	}
}

func TestMigrateMultiRelation_ControlAWellFormedDefaultSurvives(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)
	schema := multiDefaultSchema([]any{red.ID}, false)

	// Without this, a strict check that dropped EVERY list default would satisfy
	// both legs above and quietly disable destination defaults for the type.
	fields := map[string]any{"color": []any{red.ID}}
	_, dropped, err := s.MigrateRelationReferents(nil, ws.ID, schema, fields, nil, nil, RelationCarryMode(0))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %+v, want none for a resolvable default", dropped)
	}
	arr, ok := fields["color"].([]any)
	if !ok || len(arr) != 1 || arr[0] != red.ID {
		t.Errorf("color = %#v, want the canonical id kept", fields["color"])
	}
}

// The SCALAR arm of the migrate default bucket, which nothing covered.
//
// Found by a surviving mutant, not by reading: applying the strict list reader
// to the scalar type as well — which would drop EVERY scalar relation default
// on a copy or move — survived the whole `internal/store` suite. That is a
// coverage hole older than this unit; the strict/lenient split just made it
// visible, and a survivor is a question rather than a clearance.
func TestMigrateScalarRelationDefault_AWellFormedDefaultSurvives(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "color", Type: "relation", Collection: "colors", Default: red.ID},
	}}

	fields := map[string]any{"color": red.ID}
	_, dropped, err := s.MigrateRelationReferents(nil, ws.ID, schema, fields, nil, nil, RelationCarryMode(0))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %+v; a resolvable scalar default must survive a move", dropped)
	}
	if fields["color"] != red.ID {
		t.Errorf("color = %#v, want the canonical id kept", fields["color"])
	}
}

func TestMigrateScalarRelationDefault_ANonStringDefaultIsDropped(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, _ := relationFixture(t, s)
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "color", Type: "relation", Collection: "colors", Default: 42},
	}}

	// The other half, so the leg above cannot be satisfied by a bucket that
	// accepts everything. `ValidateFields` assigns a default without
	// type-checking it, so this is the only pass between `42` and the row.
	fields := map[string]any{"color": 42}
	_, dropped, err := s.MigrateRelationReferents(nil, ws.ID, schema, fields, nil, nil, RelationCarryMode(0))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(dropped) != 1 || dropped[0].Reason != RelationTargetInvalidShape {
		t.Errorf("dropped = %+v, want one invalid_shape", dropped)
	}
	if v, present := fields["color"]; present {
		t.Errorf("color survived as %#v", v)
	}
}
