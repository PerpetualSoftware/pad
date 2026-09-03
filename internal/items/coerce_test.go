package items

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The schema every case below is coerced against.
func coerceSchema() models.CollectionSchema {
	return models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "cost", Type: "number"},
		{Key: "spec", Type: "json"},
		{Key: "tags", Type: "multi_select"},
		{Key: "done", Type: "checkbox"},
		{Key: "note", Type: "text"},
		{Key: "due", Type: "date"},
	}}
}

// The defect this exists for: the remote /mcp door builds its field map with
// `dst[key] = val`, so a declared number field arrives as the STRING "42" and
// validateFieldType refuses it — the field is unwritable on that transport
// (BUG-2850). Every assertion here is about the native TYPE that reaches the
// store, because "the write succeeded" is what the CLI door already did.
func TestCoerceFieldsTypesDeclaredStrings(t *testing.T) {
	in := map[string]any{
		"cost": "42",
		"spec": `[{"name":"a"}]`,
		"tags": `["x","y"]`,
		"done": "true",
	}
	out := CoerceFields(in, coerceSchema())

	if got, ok := out["cost"].(float64); !ok || got != 42 {
		t.Fatalf("cost: want float64(42), got %[1]T(%[1]v)", out["cost"])
	}
	if _, ok := out["spec"].([]any); !ok {
		t.Fatalf("spec: want []any, got %[1]T(%[1]v)", out["spec"])
	}
	if _, ok := out["tags"].([]any); !ok {
		t.Fatalf("tags: want []any, got %[1]T(%[1]v)", out["tags"])
	}
	if got, ok := out["done"].(bool); !ok || !got {
		t.Fatalf("done: want bool(true), got %[1]T(%[1]v)", out["done"])
	}
}

// Types that are already strings must stay strings. A "coerce everything that
// parses" implementation would turn a text field holding "42" into a number and
// corrupt data that was never broken — this is the guard against fixing the bug
// by over-reaching.
func TestCoerceFieldsLeavesStringTypedFieldsAlone(t *testing.T) {
	out := CoerceFields(map[string]any{
		"note": "42",
		"due":  "2026-09-02",
	}, coerceSchema())

	if got, ok := out["note"].(string); !ok || got != "42" {
		t.Fatalf("note: want string(\"42\"), got %[1]T(%[1]v)", out["note"])
	}
	if got, ok := out["due"].(string); !ok || got != "2026-09-02" {
		t.Fatalf("due: want the date string, got %[1]T(%[1]v)", out["due"])
	}
}

// A value that will not parse is handed to the validator UNCHANGED, so the
// existing "must be a number" error still fires. Coercion must not invent an
// error path, and must not swallow a bad value into something plausible.
func TestCoerceFieldsLeavesUnparseableValuesForTheValidator(t *testing.T) {
	out := CoerceFields(map[string]any{
		"cost": "not-a-number",
		"spec": "{definitely not json",
	}, coerceSchema())

	if got, ok := out["cost"].(string); !ok || got != "not-a-number" {
		t.Fatalf("cost: want the original string, got %[1]T(%[1]v)", out["cost"])
	}
	if err := ValidateFields(out, coerceSchema()); err == nil {
		t.Fatal("expected the validator to still refuse the un-coercible value")
	}
}

// NaN and ±Inf parse as floats and then cannot be marshalled: encoding/json
// fails, the downstream json.Marshal(fields) error is ignored, and the ENTIRE
// fields payload is silently dropped. They must fall through as strings so the
// validator refuses them loudly instead.
func TestCoerceFieldsRefusesNonFiniteNumbers(t *testing.T) {
	for _, raw := range []string{"NaN", "Inf", "-Inf", "+Inf"} {
		out := CoerceFields(map[string]any{"cost": raw}, coerceSchema())
		if f, ok := out["cost"].(float64); ok {
			t.Fatalf("%q was coerced to float64(%v); non-finite values must stay strings", raw, f)
		}
	}
}

// Non-string values pass through untouched. This is not a re-typing pass over
// well-formed input — a caller already sending 42 keeps sending 42, and an
// int must not become a float64 behind their back.
func TestCoerceFieldsPassesNonStringsThrough(t *testing.T) {
	in := map[string]any{"cost": 42, "spec": []any{"already", "parsed"}, "done": true}
	out := CoerceFields(in, coerceSchema())

	if got, ok := out["cost"].(int); !ok || got != 42 {
		t.Fatalf("cost: want int(42) untouched, got %[1]T(%[1]v)", out["cost"])
	}
	if _, ok := out["spec"].([]any); !ok {
		t.Fatalf("spec: want []any untouched, got %T", out["spec"])
	}
	if got, ok := out["done"].(bool); !ok || !got {
		t.Fatalf("done: want bool(true) untouched, got %[1]T(%[1]v)", out["done"])
	}
}

// Keys the schema does not declare are left exactly as they arrived. This is
// TODAY'S behaviour and the other half of BUG-2850 — the disposition (refuse /
// warn / keep) is a product decision still open. The test pins what the code
// does so the decision, when it lands, is a deliberate change to a stated
// behaviour rather than a silent one.
func TestCoerceFieldsLeavesUndeclaredKeysUntouched(t *testing.T) {
	out := CoerceFields(map[string]any{"materials_cost": "42"}, coerceSchema())

	if got, ok := out["materials_cost"].(string); !ok || got != "42" {
		t.Fatalf("undeclared key: want the string untouched, got %[1]T(%[1]v)", out["materials_cost"])
	}
}

// CoerceFields returns a NEW map. Two call sites re-marshal the map they pass
// in, so a function that quietly rewrote its argument would change what gets
// stored from behind a name that does not say so.
func TestCoerceFieldsDoesNotMutateItsInput(t *testing.T) {
	in := map[string]any{"cost": "42"}
	_ = CoerceFields(in, coerceSchema())

	if got, ok := in["cost"].(string); !ok || got != "42" {
		t.Fatalf("input was mutated: cost is now %[1]T(%[1]v)", in["cost"])
	}
}

// Reserved keys are system-written metadata, not a user's stray field, so
// naming them as "undeclared" on every write that carries one would be noise
// the reader learns to ignore (BUG-2850).
//
// Excluded via models.IsReservedItemField rather than a second list here: that
// set exists so callers ask instead of re-listing, and its doc comment records
// what re-listing cost the last time someone did it.
func TestUndeclaredFieldKeysExcludesReservedMetadata(t *testing.T) {
	got := UndeclaredFieldKeys(map[string]any{
		"implementation_notes": "written by pad item note",
		"decision_log":         "written by pad item decide",
		"github_pr":            "written by pad github link",
		"convention":           "system",
		"materials_cost":       42,
	}, coerceSchema())

	if len(got) != 1 || got[0] != "materials_cost" {
		t.Fatalf("undeclared = %v, want only [materials_cost] — reserved metadata must not be reported", got)
	}
}

// Declared keys are never reported, and the result is sorted so the warning
// text is stable across runs (map iteration order is not).
func TestUndeclaredFieldKeysIsSortedAndSkipsDeclared(t *testing.T) {
	got := UndeclaredFieldKeys(map[string]any{
		"cost":  1,
		"zebra": 1,
		"alpha": 1,
	}, coerceSchema())

	if len(got) != 2 || got[0] != "alpha" || got[1] != "zebra" {
		t.Fatalf("undeclared = %v, want [alpha zebra] sorted, with the declared 'cost' absent", got)
	}
}
