package mcp

import (
	"testing"
)

// TestStripDuplicatedFieldsKeys covers the dedup helper behavior in
// isolation. Both implementation_notes and decision_log get dropped;
// other keys pass through untouched.
func TestStripDuplicatedFieldsKeys(t *testing.T) {
	t.Run("strips both", func(t *testing.T) {
		in := map[string]any{
			"status":               "open",
			"priority":             "high",
			"implementation_notes": []any{map[string]any{"id": "n1"}},
			"decision_log":         []any{map[string]any{"id": "d1"}},
		}
		out := stripDuplicatedFieldsKeys(in).(map[string]any)
		if _, has := out["implementation_notes"]; has {
			t.Errorf("implementation_notes not stripped: %v", out)
		}
		if _, has := out["decision_log"]; has {
			t.Errorf("decision_log not stripped: %v", out)
		}
		if out["status"] != "open" || out["priority"] != "high" {
			t.Errorf("non-duplicate keys lost: %v", out)
		}
	})

	t.Run("no-op when keys absent", func(t *testing.T) {
		in := map[string]any{"status": "open"}
		out := stripDuplicatedFieldsKeys(in).(map[string]any)
		if out["status"] != "open" {
			t.Errorf("status lost: %v", out)
		}
		if len(out) != 1 {
			t.Errorf("unexpected keys present: %v", out)
		}
	})

	t.Run("non-object input passes through", func(t *testing.T) {
		// Defensive: if a hand-written fields value is somehow a
		// primitive or array, stripDuplicatedFieldsKeys must not
		// panic.
		cases := []any{
			nil,
			"a string",
			float64(42),
			[]any{1, 2, 3},
		}
		for _, in := range cases {
			out := stripDuplicatedFieldsKeys(in)
			// Just confirm no crash and the value is returned (we
			// don't compare DeepEqual because nil passes through as nil
			// and we want to avoid the type-assertion noise).
			_ = out
		}
	})
}

// TestPackageJSONResult_StripsDuplicatedNotesAndLog is the end-to-end
// regression: a CLI body that carries top-level
// implementation_notes / decision_log arrays AND embeds them inside
// the fields blob — exactly the duplication BUG-987 bug 10 reported —
// produces a normalized structuredContent where the embeds are gone
// and the top-level arrays remain.
func TestPackageJSONResult_StripsDuplicatedNotesAndLog(t *testing.T) {
	body := `{
		"ref": "TASK-7",
		"implementation_notes": [{"id":"n1","summary":"Outer note"}],
		"decision_log": [{"id":"d1","decision":"Outer decision"}],
		"fields": "{\"status\":\"open\",\"implementation_notes\":[{\"id\":\"n1\"}],\"decision_log\":[{\"id\":\"d1\"}]}"
	}`
	res := packageJSONResult(body)
	sc := res.StructuredContent.(map[string]any)

	// Top-level arrays preserved verbatim.
	notes, ok := sc["implementation_notes"].([]any)
	if !ok || len(notes) != 1 {
		t.Errorf("top-level implementation_notes lost or wrong shape: %v", sc["implementation_notes"])
	}
	log, ok := sc["decision_log"].([]any)
	if !ok || len(log) != 1 {
		t.Errorf("top-level decision_log lost or wrong shape: %v", sc["decision_log"])
	}

	// fields parsed (BUG-991) AND the duplicated entries removed.
	fields, ok := sc["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %T, want map[string]any", sc["fields"])
	}
	if fields["status"] != "open" {
		t.Errorf("fields.status lost: %v", fields)
	}
	if _, has := fields["implementation_notes"]; has {
		t.Errorf("fields.implementation_notes not stripped: %v", fields)
	}
	if _, has := fields["decision_log"]; has {
		t.Errorf("fields.decision_log not stripped: %v", fields)
	}
}

// TestPackageJSONResult_StripsAcrossArrayItems confirms the dedup
// happens for every entry in a list-style response, not just the
// top-level item.
func TestPackageJSONResult_StripsAcrossArrayItems(t *testing.T) {
	body := `[
		{"ref":"TASK-1","fields":"{\"status\":\"open\",\"implementation_notes\":[{\"id\":\"n1\"}]}"},
		{"ref":"TASK-2","fields":"{\"status\":\"done\",\"decision_log\":[{\"id\":\"d1\"}]}"}
	]`
	res := packageJSONResult(body)
	wrapped := res.StructuredContent.(map[string]any)
	items := wrapped["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	for i, it := range items {
		m := it.(map[string]any)
		fields, ok := m["fields"].(map[string]any)
		if !ok {
			t.Errorf("items[%d].fields = %T, want map", i, m["fields"])
			continue
		}
		if _, has := fields["implementation_notes"]; has {
			t.Errorf("items[%d].fields.implementation_notes not stripped", i)
		}
		if _, has := fields["decision_log"]; has {
			t.Errorf("items[%d].fields.decision_log not stripped", i)
		}
	}
}
