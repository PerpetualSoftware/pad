package mcp

import "testing"

func TestSearchEntryProjection(t *testing.T) {
	input := map[string]any{"results": []any{
		map[string]any{"item": map[string]any{"ref": "DOC-1", "title": "Plain"}, "snippet": "note context", "matched_in": "note", "entry_id": "note-one"},
		map[string]any{"item": map[string]any{"ref": "DOC-2", "title": "Plain"}, "snippet": "decision context", "matched_in": "decision", "entry_id": "decision-two"},
	}, "total": 2}
	got := chatGPTResponseShapes["search"].apply(input).(map[string]any)
	results := got["results"].([]any)
	for i, want := range []struct{ field, id, snippet string }{{"note", "note-one", "note context"}, {"decision", "decision-two", "decision context"}} {
		hit := results[i].(map[string]any)
		if hit["matched_in"] != want.field || hit["entry_id"] != want.id || hit["snippet"] != want.snippet {
			t.Errorf("source lost: %+v", hit)
		}
	}
}
