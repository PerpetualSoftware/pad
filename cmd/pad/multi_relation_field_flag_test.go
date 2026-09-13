package main

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// P14 — `pad item create ... --field 'members=["PEOPL-1"]'` (PLAN-2857 U4,
// codex round 1).
//
// This function runs BEFORE the request exists, so it is a second copy of the
// rule `items.coerceValue` states, kept in step by hand. It gets its own test
// for the same reason it gets its own implementation: nothing links them, and
// the CLI was the surface where the type was unwritable.
func TestParseFieldFlag_MultiRelationParsesAJSONArray(t *testing.T) {
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "members", Type: "multi_relation", Collection: "people"},
	}}
	got := parseFieldFlag(schema, "members", `["PEOPL-1","PEOPL-2"]`)
	arr, ok := got.([]any)
	if !ok {
		t.Fatalf("parsed to %T (%#v), want an array — as a string the server refuses a shape the user never typed", got, got)
	}
	if len(arr) != 2 || arr[0] != "PEOPL-1" || arr[1] != "PEOPL-2" {
		t.Errorf("parsed %#v, want the two refs in order", arr)
	}
}

func TestParseFieldFlag_MultiRelationLeavesUnparseableTextAlone(t *testing.T) {
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "members", Type: "multi_relation", Collection: "people"},
	}}
	if got := parseFieldFlag(schema, "members", "PEOPL-1"); got != "PEOPL-1" {
		t.Errorf("parsed %#v, want the raw string left for the server's validator", got)
	}
}

func TestParseFieldFlag_ScalarRelationStillTakesABareString(t *testing.T) {
	// CONTROL: the scalar type's value is one reference and is correct as text.
	// A `case` that swallowed both would pass the leg above and break this.
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "owner", Type: "relation", Collection: "people"},
	}}
	if got := parseFieldFlag(schema, "owner", "PEOPL-1"); got != "PEOPL-1" {
		t.Errorf("parsed %#v, want the bare string", got)
	}
}
