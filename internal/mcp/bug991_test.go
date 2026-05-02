package mcp

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestNormalizeStringEncodedJSONFields_SingleItem covers the most
// common shape — a single-item response with stringified `fields`
// and `tags`. After the BUG-991 path-A normalization at the MCP
// boundary, both come back as native types.
func TestNormalizeStringEncodedJSONFields_SingleItem(t *testing.T) {
	in := map[string]any{
		"ref":    "TASK-5",
		"title":  "Test",
		"fields": `{"status":"open","priority":"high"}`,
		"tags":   `["frontend","auth"]`,
	}
	got := normalizeStringEncodedJSONFields(in)
	gotMap, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("got %T, want map", got)
	}

	fields, ok := gotMap["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %T, want map[string]any", gotMap["fields"])
	}
	if fields["status"] != "open" || fields["priority"] != "high" {
		t.Errorf("fields content lost: %v", fields)
	}

	tags, ok := gotMap["tags"].([]any)
	if !ok {
		t.Fatalf("tags = %T, want []any", gotMap["tags"])
	}
	if len(tags) != 2 || tags[0] != "frontend" || tags[1] != "auth" {
		t.Errorf("tags content lost: %v", tags)
	}
}

// TestNormalizeStringEncodedJSONFields_ItemArray covers list-style
// responses where every entry has stringified fields/tags. Each
// item gets normalized independently.
func TestNormalizeStringEncodedJSONFields_ItemArray(t *testing.T) {
	in := []any{
		map[string]any{
			"ref":    "TASK-1",
			"fields": `{"status":"open"}`,
			"tags":   `[]`,
		},
		map[string]any{
			"ref":    "TASK-2",
			"fields": `{"status":"done","priority":"low"}`,
			"tags":   `["bug"]`,
		},
	}
	got := normalizeStringEncodedJSONFields(in).([]any)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	first := got[0].(map[string]any)
	second := got[1].(map[string]any)

	if _, ok := first["fields"].(map[string]any); !ok {
		t.Errorf("first.fields = %T, want map", first["fields"])
	}
	if _, ok := first["tags"].([]any); !ok {
		t.Errorf("first.tags = %T, want []any", first["tags"])
	}
	if _, ok := second["fields"].(map[string]any); !ok {
		t.Errorf("second.fields = %T, want map", second["fields"])
	}
}

// TestNormalizeStringEncodedJSONFields_NestedItem covers shapes with
// items embedded inside a wrapping envelope (e.g. dashboard's
// active_items, recent_activity, etc.). Each nested item still gets
// its fields/tags normalized.
func TestNormalizeStringEncodedJSONFields_NestedItem(t *testing.T) {
	in := map[string]any{
		"summary": map[string]any{"total": 5},
		"active_items": []any{
			map[string]any{
				"ref":    "TASK-1",
				"fields": `{"status":"in-progress"}`,
			},
		},
		"recent_activity": []any{
			map[string]any{
				"action": "updated",
				"item": map[string]any{
					"ref":    "TASK-2",
					"fields": `{"status":"done"}`,
					"tags":   `["release"]`,
				},
			},
		},
	}
	got := normalizeStringEncodedJSONFields(in).(map[string]any)
	active := got["active_items"].([]any)
	first := active[0].(map[string]any)
	if _, ok := first["fields"].(map[string]any); !ok {
		t.Errorf("active_items[0].fields = %T, want map", first["fields"])
	}
	activity := got["recent_activity"].([]any)
	embeddedItem := activity[0].(map[string]any)["item"].(map[string]any)
	if _, ok := embeddedItem["fields"].(map[string]any); !ok {
		t.Errorf("recent_activity[0].item.fields = %T, want map", embeddedItem["fields"])
	}
	if _, ok := embeddedItem["tags"].([]any); !ok {
		t.Errorf("recent_activity[0].item.tags = %T, want []any", embeddedItem["tags"])
	}
}

// TestNormalizeStringEncodedJSONFields_LeavesUnrelatedStringsAlone is
// the safety net: free-form `tags` / `fields` keys outside item
// contexts (e.g. `metadata` blobs) shouldn't be touched if their
// strings don't look like JSON. Conservative: only objects with
// leading `{` or `[` are candidates for substitution.
func TestNormalizeStringEncodedJSONFields_LeavesUnrelatedStringsAlone(t *testing.T) {
	in := map[string]any{
		"ref":    "TASK-1",
		"fields": `not actually json`,
		"tags":   ``,
		"deeply": map[string]any{
			"nested": map[string]any{
				"unrelated": "value",
			},
		},
	}
	got := normalizeStringEncodedJSONFields(in).(map[string]any)
	if got["fields"] != "not actually json" {
		t.Errorf("non-JSON fields modified: %v", got["fields"])
	}
	if got["tags"] != "" {
		t.Errorf("empty tags modified: %v", got["tags"])
	}
	// Sanity: nested unrelated values stay where they are.
	deeply, _ := got["deeply"].(map[string]any)
	nested, _ := deeply["nested"].(map[string]any)
	if nested["unrelated"] != "value" {
		t.Errorf("nested traversal mutated unrelated leaves: %v", nested)
	}
}

// TestNormalizeStringEncodedJSONFields_PrimitivesPassThrough confirms
// the walk doesn't bomb on primitive values at any depth. Any non-
// (map|slice) input is returned unchanged.
func TestNormalizeStringEncodedJSONFields_PrimitivesPassThrough(t *testing.T) {
	cases := []any{
		nil,
		"hello",
		float64(42),
		true,
		[]any{},
		map[string]any{},
	}
	for _, in := range cases {
		got := normalizeStringEncodedJSONFields(in)
		if !reflect.DeepEqual(got, in) {
			t.Errorf("got %v, want %v (input %v)", got, in, in)
		}
	}
}

// TestPackageJSONResult_NormalizesItemFields end-to-end test: a CLI
// stdout body containing a single item with stringified fields/tags
// produces structuredContent where both are native shapes. The text
// fallback is unchanged (raw CLI body) so older clients that read
// content[0].text keep seeing the same shape.
func TestPackageJSONResult_NormalizesItemFields(t *testing.T) {
	body := `{"ref":"TASK-5","title":"x","fields":"{\"status\":\"open\",\"priority\":\"high\"}","tags":"[\"frontend\"]"}`
	res := packageJSONResult(body)
	if res.IsError {
		t.Fatalf("unexpected IsError")
	}
	sc := res.StructuredContent.(map[string]any)
	fields, ok := sc["fields"].(map[string]any)
	if !ok {
		t.Fatalf("structuredContent.fields = %T, want map[string]any", sc["fields"])
	}
	if fields["status"] != "open" || fields["priority"] != "high" {
		t.Errorf("fields content wrong: %v", fields)
	}
	tags, ok := sc["tags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != "frontend" {
		t.Errorf("structuredContent.tags wrong: %v", sc["tags"])
	}
	// Text fallback preserves the original CLI body verbatim. Older
	// clients that don't parse structuredContent should NOT see a
	// breaking shape change.
	if got := textOf(res); got != body {
		t.Errorf("text fallback diverged:\n got %q\nwant %q", got, body)
	}
}

// TestPackageJSONResult_ArrayItemsNormalized confirms list-style
// responses are normalized too, in combination with the BUG-985
// {items: [...]} array wrap.
func TestPackageJSONResult_ArrayItemsNormalized(t *testing.T) {
	body := `[{"ref":"TASK-1","fields":"{\"status\":\"open\"}"},{"ref":"TASK-2","fields":"{\"status\":\"done\"}"}]`
	res := packageJSONResult(body)
	wrapped := res.StructuredContent.(map[string]any)
	items := wrapped["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected 2 items; got %d", len(items))
	}
	for i, it := range items {
		m := it.(map[string]any)
		if _, ok := m["fields"].(map[string]any); !ok {
			t.Errorf("items[%d].fields = %T, want map", i, m["fields"])
		}
	}
}

// TestPackageJSONResult_MalformedFieldsStaysString covers the safety
// net at the end-to-end level: if `fields` carries an invalid JSON
// string, leave it as a string rather than dropping it. Agents that
// see a string here can still surface the underlying value.
func TestPackageJSONResult_MalformedFieldsStaysString(t *testing.T) {
	body := `{"ref":"TASK-X","fields":"{not actually json"}`
	res := packageJSONResult(body)
	sc := res.StructuredContent.(map[string]any)
	fields, ok := sc["fields"].(string)
	if !ok {
		t.Fatalf("malformed fields should stay string; got %T", sc["fields"])
	}
	if fields != "{not actually json" {
		t.Errorf("malformed fields content lost: %q", fields)
	}
}

// _ silences a potential unused-import warning on encoding/json if a
// later refactor stops using it directly. Keeps the file resilient
// without affecting the test inventory.
var _ = json.Marshal
