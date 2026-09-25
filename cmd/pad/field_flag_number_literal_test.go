package main

import (
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3202: `--field n=<integer above 2^53>` is sent with every digit. The
// value is typed client-side before the request exists, so a float64 here
// rounded it before the server ever saw it.
func TestParseFieldFlagKeepsNumberLiterals(t *testing.T) {
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "n", Type: "number"},
		{Key: "j", Type: "json"},
	}}
	if got := parseFieldFlag(schema, "n", "9007199254740993"); got != json.Number("9007199254740993") {
		t.Errorf("number: got %#v, want json.Number(\"9007199254740993\")", got)
	}
	// Not a JSON literal, but ParseFloat accepts it: the old float path.
	if got := parseFieldFlag(schema, "n", "0x1p4"); got != float64(16) {
		t.Errorf("hex float: got %#v, want float64(16)", got)
	}
	b, err := json.Marshal(parseFieldFlag(schema, "j", `{"x":9007199254740993}`))
	if err != nil || string(b) != `{"x":9007199254740993}` {
		t.Errorf("json field: got %s (%v), want the literal kept", b, err)
	}
}
