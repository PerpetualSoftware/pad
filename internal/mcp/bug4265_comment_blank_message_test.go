package mcp

import (
	"strings"
	"testing"
)

// BUG-4265: the MCP comment action refuses an empty or whitespace-only
// message before building any request, so nothing is posted.
func TestMapItemComment_RefusesABlankMessage(t *testing.T) {
	for name, msg := range map[string]any{
		"missing":          nil,
		"empty":            "",
		"spaces":           "   ",
		"newlines and tab": "\n\t\n",
	} {
		t.Run(name, func(t *testing.T) {
			in := map[string]any{"workspace": "docapp", "ref": "TASK-5"}
			if msg != nil {
				in["message"] = msg
			}
			method, path, body, err := mapItemComment(in)
			if err == nil {
				t.Fatalf("mapItemComment(%q) = %s %s %s, want a refusal", msg, method, path, body)
			}
			if !strings.Contains(err.Error(), "message is required") {
				t.Errorf("error = %q, want it to say the message is required", err.Error())
			}
			if method != "" || path != "" || body != nil {
				t.Errorf("a request was built (%q %q %s) despite the refusal", method, path, body)
			}
		})
	}

	// Control: a real message still maps to the create route.
	method, path, body, err := mapItemComment(map[string]any{"workspace": "docapp", "ref": "TASK-5", "message": "a real comment"})
	if err != nil || method != "POST" || !strings.HasSuffix(path, "/items/TASK-5/comments") || !strings.Contains(string(body), "a real comment") {
		t.Errorf("control = %q %q %s %v, want a POST to the item's comments carrying the message", method, path, body, err)
	}
}
