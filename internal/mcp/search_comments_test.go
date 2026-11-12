package mcp

import "testing"

func TestSearchCommentProjection(t *testing.T) {
	input := map[string]any{"results": []any{map[string]any{"item": map[string]any{"ref": "DOC-1", "title": "Plain"}, "snippet": "comment context", "matched_in": "comment", "comment_id": "comment-one"}}, "total": 1}
	got := chatGPTResponseShapes["search"].apply(input).(map[string]any)
	hit := got["results"].([]any)[0].(map[string]any)
	if hit["matched_in"] != "comment" || hit["comment_id"] != "comment-one" || hit["snippet"] != "comment context" {
		t.Errorf("source lost: %+v", hit)
	}
}
