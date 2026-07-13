package server

import (
	"encoding/json"
	"testing"
)

func TestStripReservedUnparentedViewFilter(t *testing.T) {
	t.Parallel()
	input := `{"filters":[{"field":"$unparented","op":"eq","value":true},{"field":"status","op":"eq","value":"open"}],"group_by":"status"}`
	got := stripReservedUnparentedViewFilter(input)
	var config map[string]any
	if err := json.Unmarshal([]byte(got), &config); err != nil {
		t.Fatalf("parse sanitized config: %v", err)
	}
	filters, ok := config["filters"].([]any)
	if !ok || len(filters) != 1 {
		t.Fatalf("filters = %#v, want one ordinary filter", config["filters"])
	}
	filter, _ := filters[0].(map[string]any)
	if filter["field"] != "status" || config["group_by"] != "status" {
		t.Fatalf("sanitizer changed non-reserved config: %#v", config)
	}
}

func TestStripReservedUnparentedViewFilter_DropsMalformedFilterShape(t *testing.T) {
	t.Parallel()
	input := `{"filters":{"field":"$unparented","op":"eq","value":true},"group_by":"status"}`
	got := stripReservedUnparentedViewFilter(input)
	var config map[string]any
	if err := json.Unmarshal([]byte(got), &config); err != nil {
		t.Fatalf("parse sanitized config: %v", err)
	}
	if _, ok := config["filters"]; ok {
		t.Fatalf("malformed filters survived sanitization: %#v", config)
	}
	if config["group_by"] != "status" {
		t.Fatalf("sanitizer changed unrelated config: %#v", config)
	}
}

func TestPreserveReservedUnparentedViewFilter(t *testing.T) {
	t.Parallel()
	existing := `{"filters":[{"field":"$unparented","op":"eq","value":true},{"field":"priority","op":"eq","value":"high"}]}`
	submitted := `{"filters":[{"field":"status","op":"eq","value":"open"},{"field":"$unparented","op":"eq","value":false}],"group_by":"status"}`

	got, err := preserveReservedUnparentedViewFilter(existing, submitted)
	if err != nil {
		t.Fatalf("preserve filter: %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(got), &config); err != nil {
		t.Fatalf("parse merged config: %v", err)
	}
	filters, ok := config["filters"].([]any)
	if !ok || len(filters) != 2 {
		t.Fatalf("filters = %#v, want submitted ordinary + stored reserved", config["filters"])
	}
	first, _ := filters[0].(map[string]any)
	second, _ := filters[1].(map[string]any)
	if first["field"] != "status" || second["field"] != reservedUnparentedViewField || second["value"] != true {
		t.Fatalf("merged filters = %#v", filters)
	}
}
