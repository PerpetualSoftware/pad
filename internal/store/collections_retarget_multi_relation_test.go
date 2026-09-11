package store

import (
	"encoding/json"
	"testing"
)

// Collection-rename propagation into relation FieldDefs (BUG-2873), extended
// to `multi_relation` by PLAN-2857 U4.
//
// Why this file exists at all: `retargetRelationsInSchemaJSON` had NO test
// coverage before U4 (verified by grep over internal/**/*_test.go), and it is
// the highest-consequence row in U4's population table. Left out, renaming a
// collection orphans every multi_relation field pointing at it — and the
// failure is silent in the worst way: nothing errors, the schema still parses,
// and every subsequent write to the field is refused with `target_missing`
// for a rename nobody would connect to it.

func fieldTypeAndCollection(t *testing.T, raw string) map[string]string {
	t.Helper()
	var doc struct {
		Fields []struct {
			Key        string `json:"key"`
			Type       string `json:"type"`
			Collection string `json:"collection"`
		} `json:"fields"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal result schema: %v", err)
	}
	out := map[string]string{}
	for _, f := range doc.Fields {
		out[f.Key] = f.Type + "|" + f.Collection
	}
	return out
}

func TestRetargetRelations_MovesBothRelationTypes(t *testing.T) {
	t.Parallel()
	in := `{"fields":[
		{"key":"owner","type":"relation","collection":"people"},
		{"key":"owners","type":"multi_relation","collection":"people"},
		{"key":"reviewer","type":"relation","collection":"teams"},
		{"key":"labels","type":"multi_select","collection":"people","options":["a","b"]},
		{"key":"note","type":"text"}
	]}`
	out, changed, err := retargetRelationsInSchemaJSON(in, "people", "humans")
	if err != nil {
		t.Fatalf("retargetRelationsInSchemaJSON: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	got := fieldTypeAndCollection(t, out)

	if got["owner"] != "relation|humans" {
		t.Errorf("owner = %q, want relation|humans", got["owner"])
	}
	// The U4 row. Before this change it stayed on the OLD slug and nothing said so.
	if got["owners"] != "multi_relation|humans" {
		t.Errorf("owners = %q, want multi_relation|humans — a multi_relation left behind by a rename is an orphaned field with no error anywhere", got["owners"])
	}
	// Untouched: a relation pointing somewhere else.
	if got["reviewer"] != "relation|teams" {
		t.Errorf("reviewer = %q, want relation|teams", got["reviewer"])
	}
	// Untouched: a NON-relation field that happens to carry a `collection` key.
	// Without this leg, a change that retargeted on the collection value alone
	// — ignoring the type — would pass every other assertion here.
	if got["labels"] != "multi_select|people" {
		t.Errorf("labels = %q, want multi_select|people — only relation types are retargeted", got["labels"])
	}
}

// The negative: a schema with no relation pointing at the renamed collection
// reports changed=false and is returned BYTE-IDENTICAL, so the caller can skip
// the write entirely.
func TestRetargetRelations_NoMatchLeavesSchemaAlone(t *testing.T) {
	t.Parallel()
	in := `{"fields":[{"key":"owners","type":"multi_relation","collection":"teams"}]}`
	out, changed, err := retargetRelationsInSchemaJSON(in, "people", "humans")
	if err != nil {
		t.Fatalf("retargetRelationsInSchemaJSON: %v", err)
	}
	if changed {
		t.Error("changed = true, want false")
	}
	if out != in {
		t.Errorf("schema was rewritten:\n got %s\nwant %s", out, in)
	}
}
