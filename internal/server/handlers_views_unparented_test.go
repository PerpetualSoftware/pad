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
