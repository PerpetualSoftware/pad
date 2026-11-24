package mcp

import (
	"reflect"
	"strings"
	"testing"
)

func TestBUG4812NormalizeNativeFieldsAndTags(t *testing.T) {
	in := map[string]any{
		"ref":          "TASK-5",
		"workspace_id": "ws-1",
		"fields": map[string]any{
			"status":               "open",
			"implementation_notes": []any{map[string]any{"summary": "note"}},
			"decision_log":         []any{map[string]any{"decision": "dec"}},
			"fields":               `{"x":1}`,
		},
		"tags": []any{"a"},
	}

	got := normalizeStringEncodedJSONFields(in).(map[string]any)
	fields, ok := got["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %T, want map[string]any", got["fields"])
	}
	if _, has := fields["implementation_notes"]; has {
		t.Errorf("implementation_notes not stripped from native fields: %v", fields)
	}
	if _, has := fields["decision_log"]; has {
		t.Errorf("decision_log not stripped from native fields: %v", fields)
	}
	if fields["status"] != "open" {
		t.Errorf("fields.status = %v, want open", fields["status"])
	}
	nestedFields, ok := fields["fields"].(string)
	if !ok || nestedFields != `{"x":1}` {
		t.Errorf("nested fields key was recursed into: got %T(%v), want string %q", fields["fields"], fields["fields"], `{"x":1}`)
	}

	tags, ok := got["tags"].([]any)
	if !ok || !reflect.DeepEqual(tags, []any{"a"}) {
		t.Errorf("tags = %#v, want []any{\"a\"}", got["tags"])
	}
}

func TestBUG4812NormalizeStringAndNativeEqual(t *testing.T) {
	strItem := map[string]any{
		"ref":          "TASK-5",
		"workspace_id": "ws-1",
		"title":        "Parity",
		"fields":       `{"status":"open","implementation_notes":[{"summary":"note"}],"decision_log":[{"decision":"dec"}],"fields":"{\"x\":1}"}`,
		"tags":         `["a","b"]`,
	}
	nativeItem := map[string]any{
		"ref":          "TASK-5",
		"workspace_id": "ws-1",
		"title":        "Parity",
		"fields": map[string]any{
			"status":               "open",
			"implementation_notes": []any{map[string]any{"summary": "note"}},
			"decision_log":         []any{map[string]any{"decision": "dec"}},
			"fields":               `{"x":1}`,
		},
		"tags": []any{"a", "b"},
	}

	gotStr := normalizeStringEncodedJSONFields(strItem)
	gotNative := normalizeStringEncodedJSONFields(nativeItem)
	if !reflect.DeepEqual(gotStr, gotNative) {
		t.Errorf("string and native shapes normalized differently:\nstring: %#v\nnative: %#v", gotStr, gotNative)
	}
}

// The list summary shape already carried a native fields object before
// BUG-4812, with no top-level implementation_notes or decision_log: those
// keys are the only copy there, so the walk must keep them (and keep
// walking the object as it always has).
func TestBUG4812NormalizeSummaryShapeKeepsNotes(t *testing.T) {
	in := map[string]any{
		"ref": "TASK-5",
		"fields": map[string]any{
			"status":               "open",
			"implementation_notes": []any{map[string]any{"summary": "note"}},
			"decision_log":         []any{map[string]any{"decision": "dec"}},
		},
	}
	got := normalizeStringEncodedJSONFields(in).(map[string]any)
	fields := got["fields"].(map[string]any)
	for _, k := range []string{"implementation_notes", "decision_log"} {
		if _, has := fields[k]; !has {
			t.Errorf("summary shape lost fields.%s: %v", k, fields)
		}
	}
}

func TestBUG4812FormatItemAsMarkdownBothShapes(t *testing.T) {
	strBlob := `{
		"ref": "TASK-5",
		"title": "Big number",
		"parent_ref": "PLAN-1",
		"parent_title": "Parent",
		"fields": "{\"status\":\"open\",\"big\":12345678901234567890}",
		"content": "Body text."
	}`
	nativeBlob := `{
		"ref": "TASK-5",
		"title": "Big number",
		"parent_ref": "PLAN-1",
		"parent_title": "Parent",
		"fields": {"status":"open","big":12345678901234567890},
		"content": "Body text."
	}`

	mdStr, err := formatItemAsMarkdown(strBlob)
	if err != nil {
		t.Fatalf("formatItemAsMarkdown(string shape): %v", err)
	}
	mdNative, err := formatItemAsMarkdown(nativeBlob)
	if err != nil {
		t.Fatalf("formatItemAsMarkdown(native shape): %v", err)
	}
	if mdStr != mdNative {
		t.Errorf("markdown differs between string and native shapes:\n--- string ---\n%s\n--- native ---\n%s", mdStr, mdNative)
	}
	if !strings.Contains(mdNative, "- **big:** 12345678901234567890\n") {
		t.Errorf("native shape did not preserve big number digits:\n%s", mdNative)
	}

	// Empty fields object matches empty string-encoded fields object.
	emptyStr, err := formatItemAsMarkdown(`{"ref":"TASK-6","title":"Empty","fields":"{}","content":"Body"}`)
	if err != nil {
		t.Fatalf("formatItemAsMarkdown(empty string): %v", err)
	}
	emptyNative, err := formatItemAsMarkdown(`{"ref":"TASK-6","title":"Empty","fields":{},"content":"Body"}`)
	if err != nil {
		t.Fatalf("formatItemAsMarkdown(empty object): %v", err)
	}
	if emptyStr != emptyNative {
		t.Errorf("empty fields markdown differs:\nstring: %q\nnative: %q", emptyStr, emptyNative)
	}
}
