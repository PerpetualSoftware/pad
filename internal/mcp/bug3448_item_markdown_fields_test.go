package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// BUG-3448 (codex r1): the MCP item resource's markdown printed a json field
// in Go's map syntax, the agent-facing form of the same defect, and rounded a
// number above 2^53 on the way through.
func TestFormatItemAsMarkdownPrintsJSONFieldsAsJSON(t *testing.T) {
	fields := `{"status":"active","arguments":[{"name":"target","default":9007199254740993}]}`
	blob, _ := json.Marshal(map[string]any{"ref": "PLAYB-5", "title": "Ship", "fields": fields})
	out, err := formatItemAsMarkdown(string(blob))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "map[") {
		t.Errorf("a json field printed in Go map syntax:\n%s", out)
	}
	if !strings.Contains(out, `"name":"target"`) || !strings.Contains(out, "9007199254740993") {
		t.Errorf("the arguments field is not its JSON, digits intact:\n%s", out)
	}
	if !strings.Contains(out, "- **status:** active") {
		t.Errorf("control: the scalar field changed:\n%s", out)
	}
}
