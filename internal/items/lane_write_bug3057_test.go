package items

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The server half of BUG-3057's contract.
//
// A board/list lane key is a STRING projection of a field value, and the web
// write paths used to assign it straight back to the field. `laneWriteValue`
// (web/src/lib/collections/laneWriteValue.ts) now converts through the declared
// type, and its comments cite what this validator does with each form. That
// citation is only worth anything if it is pinned HERE: a change to the
// validator that started accepting the string forms, or stopped accepting the
// converted ones, would leave that module correct against a validator that has
// moved and nothing would notice.
//
// Not a test of the fix — the fix is in TypeScript. A test of the facts the fix
// was derived from.
func TestLaneKeyWrites_BUG3057(t *testing.T) {
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "score", Type: "number"},
		{Key: "shipped", Type: "checkbox"},
		{Key: "tags", Type: "multi_select", Options: []string{"a", "b", "c"}},
		{Key: "stage", Type: "select", Options: []string{"open", "done"}},
	}}

	cases := []struct {
		name    string
		patch   map[string]any
		wantErr string // "" means the write must be ACCEPTED
	}{
		// What the lane key IS, pre-fix. Every one of these is a legitimate
		// drag or create-in-lane that failed.
		{"number gets the raw lane key", map[string]any{"score": "0"}, "must be a number"},
		{"number gets the uncategorised lane key", map[string]any{"score": ""}, "must be a number"},
		{"checkbox gets the raw lane key", map[string]any{"shipped": "false"}, "must be a boolean"},
		{"multi_select gets the raw lane key", map[string]any{"tags": "c"}, "must be an array of strings"},

		// What `laneWriteValue` sends instead.
		{"number gets a number", map[string]any{"score": float64(0)}, ""},
		{"number cleared by the delete sentinel", map[string]any{"score": nil}, ""},
		{"checkbox gets a bool", map[string]any{"shipped": false}, ""},
		{"checkbox cleared by the delete sentinel", map[string]any{"shipped": nil}, ""},
		{"multi_select gets a one-element array", map[string]any{"tags": []any{"c"}}, ""},
		// A lane is a COMBINATION, so a two-tag lane writes two tags — and both
		// have to be declared options or this arm refuses, which is exactly why
		// the web side resolves the lane key against the options rather than
		// splitting it blindly.
		{"multi_select gets a two-element array", map[string]any{"tags": []any{"a", "b"}}, ""},
		{"multi_select given the JOINED lane key as one tag", map[string]any{"tags": []any{"a,b"}}, "not in allowed options"},
		{"multi_select cleared by an empty array", map[string]any{"tags": []any{}}, ""},

		// The case that was never broken, pinned so a conversion applied to
		// EVERY type would be caught from this side too.
		{"select keeps taking its lane key", map[string]any{"stage": "done"}, ""},
		{"select cleared by the empty lane key", map[string]any{"stage": ""}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePartialFields(tc.patch, schema)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("write must be accepted, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("write must be refused (%s), got nil — laneWriteValue's premise has moved", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("refused for the wrong reason: want %q, got %v", tc.wantErr, err)
			}
		})
	}
}
