package models

import (
	"encoding/json"
	"testing"
)

// ImportScheduleDrop.Count is additive: a drop of one record has the two members
// it always had, and an aggregate entry adds the number of records it stands for.
func TestImportScheduleDropCountIsAdditive(t *testing.T) {
	one, _ := json.Marshal(ImportScheduleDrop{Item: "TASK-1", Reason: "links:cycle"})
	if string(one) != `{"item":"TASK-1","reason":"links:cycle"}` {
		t.Fatalf("a single drop is %s", one)
	}
	many, _ := json.Marshal(ImportScheduleDrop{Reason: "links:bundle_limit", Count: 7000})
	if string(many) != `{"item":"","reason":"links:bundle_limit","count":7000}` {
		t.Fatalf("an aggregate drop is %s", many)
	}
}
