package items

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The server half of BUG-3043's re-home rule.
//
// A lane draft whose lane is gone is saved "into Uncategorized", and whether
// that create would actually LAND there is decided client-side by
// `draftSaveTarget` (web/src/lib/collections/laneDrafts.ts). That module mirrors
// three facts about this validator, and cites them:
//
//  1. a select stores "" even when REQUIRED — `required` fires only on an
//     absent or nil key (validate.go, the `!exists || val == nil` branch of
//     ValidateFieldsDetailedWithDrops), and "" skips the options check
//     (validateFieldType's `s != "" &&` guard);
//  2. a multi_select stores [] even when required, for the same first reason;
//  3. an OMITTED key is filled by a schema default when one exists, and refused
//     as required when none does.
//
// If any of these moves, the client's "can Uncategorized receive it" answer is
// wrong in silence — it would re-home a draft into a create the server refuses,
// or block one the server would take. Not a test of the fix, which is in
// TypeScript: a test of the facts the fix was derived from, on the
// TestLaneKeyWrites_BUG3057 pattern.
func TestUncategorizedCreateFacts_BUG3043(t *testing.T) {
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "stage", Type: "select", Options: []string{"open", "done"}, Required: true},
		{Key: "tags", Type: "multi_select", Options: []string{"a"}, Required: true},
	}}

	t.Run("fact 1: a REQUIRED select stores the blank", func(t *testing.T) {
		fields := map[string]any{"stage": "", "tags": []any{"a"}}
		if err := ValidateFields(fields, schema); err != nil {
			t.Fatalf("'' on a required select must be accepted, got: %v — laneDrafts.ts would now re-home into a refused create", err)
		}
		if got, ok := fields["stage"]; !ok || got != "" {
			t.Fatalf("the blank must be stored as '', got %#v (present=%v)", got, ok)
		}
	})

	t.Run("fact 2: a REQUIRED multi_select stores the empty list", func(t *testing.T) {
		fields := map[string]any{"stage": "open", "tags": []any{}}
		if err := ValidateFields(fields, schema); err != nil {
			t.Fatalf("[] on a required multi_select must be accepted, got: %v", err)
		}
	})

	t.Run("fact 3a: an omitted REQUIRED key with no default is refused", func(t *testing.T) {
		s := models.CollectionSchema{Fields: []models.FieldDef{{Key: "score", Type: "number", Required: true}}}
		err := ValidateFields(map[string]any{}, s)
		if err == nil || !strings.Contains(err.Error(), "required") {
			t.Fatalf("an omitted required number must be refused as required, got: %v — laneDrafts.ts blocks this case", err)
		}
	})

	t.Run("fact 3b: an omitted key WITH a default is filled by it", func(t *testing.T) {
		s := models.CollectionSchema{Fields: []models.FieldDef{
			{Key: "shipped", Type: "checkbox", Default: true},
			{Key: "score", Type: "number", Default: float64(3)},
		}}
		fields := map[string]any{}
		if err := ValidateFields(fields, s); err != nil {
			t.Fatalf("defaults must validate, got: %v", err)
		}
		if fields["shipped"] != true || fields["score"] != float64(3) {
			t.Fatalf("an omitted key must take its default (so the item would NOT land in Uncategorized), got %#v", fields)
		}
	})

	t.Run("fact 3c: an omitted optional key with no default stays absent", func(t *testing.T) {
		s := models.CollectionSchema{Fields: []models.FieldDef{{Key: "score", Type: "number"}}}
		fields := map[string]any{}
		if err := ValidateFields(fields, s); err != nil {
			t.Fatalf("got: %v", err)
		}
		if _, ok := fields["score"]; ok {
			t.Fatalf("an omitted optional key must stay absent (Uncategorized), got %#v", fields)
		}
	})
}
