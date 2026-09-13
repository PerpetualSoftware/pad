package items

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The EMPTY-LIST rule for `multi_relation` (PLAN-2857 U4, lead ruling day 64
// after codex round 1 found P17): `[]` is normalised at the one entry every
// write passes through, into whichever spelling of "no targets" the write shape
// already has.
//
// Two spellings, because the two shapes disagree about what an absent key
// means. A full write's absent key means "no value", so `[]` becomes absent and
// the ordinary required machinery refuses it. A partial write's absent key
// means "leave it alone", so `[]` there must become the patch path's explicit
// deletion sentinel — otherwise a caller asking to empty the field gets a
// no-op, which is the opposite of what they asked for.

func multiSchema(required bool) models.CollectionSchema {
	return models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "members", Label: "Members", Type: "multi_relation", Collection: "people", Required: required},
	}}
}

func TestEmptyMultiRelation_FullWriteBecomesAnAbsentKey(t *testing.T) {
	fields := map[string]any{"members": []any{}}
	if err := ValidateFields(fields, multiSchema(false)); err != nil {
		t.Fatalf("an empty list is a legal way to say no targets: %v", err)
	}
	if _, present := fields["members"]; present {
		t.Errorf("members survived as %#v; `[]` and absent must be ONE spelling of none, or every consumer has to handle both", fields["members"])
	}
}

func TestEmptyMultiRelation_FullWriteOnARequiredFieldIsRefusedForFree(t *testing.T) {
	// "For free" is the claim under test: the normalisation knows nothing about
	// `required`, and the refusal comes from the ordinary missing-required
	// branch. A message about the TYPE here would mean the rule had been
	// implemented twice.
	fields := map[string]any{"members": []any{}}
	err := ValidateFields(fields, multiSchema(true))
	if err == nil {
		t.Fatal("a required multi_relation accepted an empty list; required means at least one resolved element")
	}
	if got := err.Error(); !strings.Contains(got, `field "members" is required`) {
		t.Errorf("refusal was %q, want the ordinary required-field message", got)
	}
}

func TestEmptyMultiRelation_PartialWriteIsACLEAR_NotAnAbsence(t *testing.T) {
	// The leg that would break if the full-write rule were applied to both.
	// Deleting the key from a PATCH turns "empty this field" into "do nothing",
	// and the field keeps its contents while the request reports success.
	patch := map[string]any{"members": []any{}}
	if err := ValidatePartialFields(patch, multiSchema(false)); err != nil {
		t.Fatalf("clearing an optional multi_relation must be allowed: %v", err)
	}
	val, present := patch["members"]
	if !present {
		t.Fatal("the key was DELETED from the patch; a patch without the key leaves the stored value untouched, so the clear silently did nothing")
	}
	if val != nil {
		t.Errorf("patch holds %#v, want the nil deletion sentinel mergeFieldsPatch acts on", val)
	}
}

func TestEmptyMultiRelation_PartialWriteOnARequiredFieldIsRefused(t *testing.T) {
	patch := map[string]any{"members": []any{}}
	err := ValidatePartialFields(patch, multiSchema(true))
	if err == nil {
		t.Fatal("a required multi_relation was cleared by a patch; the nil sentinel is exactly what this path already refuses for a required key")
	}
	if got := err.Error(); !strings.Contains(got, "required") {
		t.Errorf("refusal was %q, want it to say the field is required", got)
	}
}

// CONTROLS. Every leg above asserts that something CHANGED; a normalisation
// that rewrote every multi_relation value, or that ran on every type, would
// satisfy several of them and break the feature.
func TestEmptyMultiRelation_ANonEmptyListIsUntouched(t *testing.T) {
	fields := map[string]any{"members": []any{"PEOP-1", "PEOP-2"}}
	if err := ValidateFields(fields, multiSchema(true)); err != nil {
		t.Fatalf("a populated required list must pass validation: %v", err)
	}
	got, ok := fields["members"].([]any)
	if !ok || len(got) != 2 {
		t.Fatalf("members = %#v, want the two supplied elements unchanged", fields["members"])
	}
}

func TestEmptyMultiRelation_AnEmptyMultiSelectIsUntouched(t *testing.T) {
	// The normalisation is scoped to ONE type. An empty `multi_select` means
	// "no tags selected" and has never been an absent key; rewriting it would
	// be this fix leaking into a neighbour.
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "labels", Type: "multi_select", Options: []string{"a", "b"}},
	}}
	fields := map[string]any{"labels": []any{}}
	if err := ValidateFields(fields, schema); err != nil {
		t.Fatalf("an empty multi_select is valid: %v", err)
	}
	if _, present := fields["labels"]; !present {
		t.Error("an empty multi_select was deleted; the empty-list rule belongs to multi_relation alone")
	}
}

// P13 — the CLI half of "a multi_relation is writable at all". Every
// string-carrying transport hands `coerceValue` raw text, and a JSON array is
// the only way to spell an ordered list in one.
func TestCoerceFields_MultiRelationParsesAJSONArray(t *testing.T) {
	schema := multiSchema(false)
	out := CoerceFields(map[string]any{"members": `["PEOPL-1","PEOPL-2"]`}, schema)
	arr, ok := out["members"].([]any)
	if !ok {
		t.Fatalf("members coerced to %T (%#v), want an array — the validator then refuses a shape the caller never sent", out["members"], out["members"])
	}
	if len(arr) != 2 || arr[0] != "PEOPL-1" || arr[1] != "PEOPL-2" {
		t.Errorf("members = %#v, want the two refs in order", arr)
	}
	// The point of the coercion is that the result VALIDATES. Asserting the
	// shape alone would pass for a coercion that produced a valid-looking array
	// the validator still refused.
	if err := ValidateFields(out, schema); err != nil {
		t.Errorf("the coerced value does not validate: %v", err)
	}
}

func TestCoerceFields_MultiRelationLeavesUnparseableTextAlone(t *testing.T) {
	// The fall-through every other parsed type has: text that is not JSON stays
	// a string so the VALIDATOR produces the error, rather than this function
	// inventing one or storing a guess.
	out := CoerceFields(map[string]any{"members": "PEOPL-1"}, multiSchema(false))
	if got, ok := out["members"].(string); !ok || got != "PEOPL-1" {
		t.Errorf("members = %#v, want the raw string left for the validator", out["members"])
	}
}

func TestCoerceFields_ScalarRelationStillTakesABareString(t *testing.T) {
	// CONTROL: the new case must not swallow the scalar type, whose value is a
	// single reference and is correct as text.
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "owner", Type: "relation", Collection: "people"},
	}}
	out := CoerceFields(map[string]any{"owner": "PEOPL-1"}, schema)
	if got, ok := out["owner"].(string); !ok || got != "PEOPL-1" {
		t.Errorf("owner = %#v, want the bare string", out["owner"])
	}
}

// The NIL spelling (codex round 7). On a full write the traversal treats a
// present-but-nil key as absent for required-ness and defaults, but does not
// REMOVE it — so `{"members": null}` was stored: a third representation of "no
// targets", which is exactly what the one-spelling rule exists to prevent.
func TestNilMultiRelation_FullWriteBecomesAnAbsentKey(t *testing.T) {
	fields := map[string]any{"members": nil}
	if err := ValidateFields(fields, multiSchema(false)); err != nil {
		t.Fatalf("nil on an optional field is not an error: %v", err)
	}
	if _, present := fields["members"]; present {
		t.Errorf("members survived as %#v; absent, [] and null must not be three ways to say the same thing", fields["members"])
	}
}

func TestNilMultiRelation_PartialWriteKeepsTheDeletionSentinel(t *testing.T) {
	// nil is the spelling a PATCH normalises TO — removing it would turn a
	// clear into a no-op, which is the same trap the empty-list rule has.
	patch := map[string]any{"members": nil}
	if err := ValidatePartialFields(patch, multiSchema(false)); err != nil {
		t.Fatalf("clearing via nil is allowed: %v", err)
	}
	val, present := patch["members"]
	if !present || val != nil {
		t.Errorf("patch holds %#v (present=%v), want the nil sentinel kept", val, present)
	}
}

func TestNilMultiRelation_ControlANilSCALARRelationIsUntouched(t *testing.T) {
	// Scoped to the list type. A nil on any other field is pre-existing
	// behaviour with its own consumers, and widening the rule to reach it would
	// be this fix leaking.
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "owner", Type: "relation", Collection: "people"},
	}}
	fields := map[string]any{"owner": nil}
	if err := ValidateFields(fields, schema); err != nil {
		t.Fatalf("nil on an optional scalar relation is not an error: %v", err)
	}
	if _, present := fields["owner"]; !present {
		t.Error("a nil scalar relation was deleted; the one-spelling rule belongs to multi_relation")
	}
}
