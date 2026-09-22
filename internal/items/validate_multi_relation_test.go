package items

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// `multi_relation` shape validation (PLAN-2857 U4).
//
// This package is DB-free, so these tests are about SHAPE only — whether an
// element names a live item is the store's question. What is decided here is
// the emptiness rule, and it deliberately DIVERGES from scalar `relation`:
// an empty or whitespace-only element is REFUSED, where a scalar empty value
// is skipped downstream. See BUG-3028 for why there was no coherent scalar
// behaviour to inherit.

func multiRelationSchema(required bool) models.CollectionSchema {
	return models.CollectionSchema{
		Fields: []models.FieldDef{{
			Key:        "owners",
			Label:      "Owners",
			Type:       "multi_relation",
			Collection: "people",
			Required:   required,
		}},
	}
}

func TestMultiRelation_AcceptsAnArrayOfReferences(t *testing.T) {
	t.Parallel()
	for _, val := range []any{
		[]any{"11111111-1111-1111-1111-111111111111", "PEOP-3", "Ada Lovelace"},
		[]string{"PEOP-3"},
		[]any{}, // "no targets" is a legal SHAPE; normalizeEmptyRelationLists rewrites it before either validator traverses
	} {
		fields := map[string]any{"owners": val}
		if err := ValidateFields(fields, multiRelationSchema(false)); err != nil {
			t.Errorf("ValidateFields(%#v) = %v, want nil", val, err)
		}
	}
}

// The three refusals, each naming the ELEMENT INDEX — a caller sending twelve
// references needs to know which one is wrong, and "field owners is invalid"
// does not tell them.
func TestMultiRelation_RefusesBadShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		val  any
		want string
	}{
		{"a bare string is not an array", "PEOP-3", "must be an array"},
		{"a number is not an array", 7, "must be an array"},
		{"an object is not an array", map[string]any{"a": 1}, "must be an array"},
		{"a non-string element", []any{"PEOP-3", 7}, "element 1"},
		{"an empty element", []any{"PEOP-3", ""}, "element 1"},
		{"a whitespace-only element", []any{"   ", "PEOP-3"}, "element 0"},
		{"a whitespace-only element, typed slice", []string{"PEOP-3", "\t"}, "element 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]any{"owners": tc.val}
			err := ValidateFields(fields, multiRelationSchema(false))
			if err == nil {
				t.Fatalf("ValidateFields(%#v) = nil, want an error", tc.val)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// THE COUNTERFACTUAL, and the reason it is here rather than assumed: the
// VALIDATOR still accepts an empty scalar `relation`. BUG-3028 converged the
// two types at the write DOORS instead — each normalises a blank relation to
// key-absent before or after this validator by provenance (items.
// DropBlankRelations), so a set blank never reaches it as a value. Moving that
// rule into the validator would refuse carried legacy blanks it cannot tell
// from set ones; this leg is what would catch that change.
func TestMultiRelation_ScalarRelationIsUnchangedAndStillAcceptsEmpty(t *testing.T) {
	t.Parallel()
	schema := models.CollectionSchema{
		Fields: []models.FieldDef{{
			Key: "owner", Label: "Owner", Type: "relation", Collection: "people",
		}},
	}
	for _, val := range []any{"", "   ", "PEOP-3"} {
		fields := map[string]any{"owner": val}
		if err := ValidateFields(fields, schema); err != nil {
			t.Errorf("scalar relation ValidateFields(%q) = %v, want nil (BUG-3028 territory, not U4's to change here)", val, err)
		}
	}
	// ...and an ARRAY in a scalar relation is still refused.
	if err := ValidateFields(map[string]any{"owner": []any{"PEOP-3"}}, schema); err == nil {
		t.Error("scalar relation accepted an array, want an error")
	}
}
