package config

import "testing"

// TASK-3321 U2a: the ChatGPT catalog's resource is the /mcp resource's MCP
// path plus /chatgpt; the bare cloud host maps to /mcp first.
func TestChatGPTResourceURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://mcp.getpad.dev":    "https://mcp.getpad.dev/mcp/chatgpt",
		"https://pad.example/mcp":   "https://pad.example/mcp/chatgpt",
		"https://pad.example/mcp/":  "https://pad.example/mcp/chatgpt",
		"https://x.example/pad/mcp": "https://x.example/pad/mcp/chatgpt",
		"":                          "",
	} {
		if got := chatGPTResourceURL(in); got != want {
			t.Errorf("chatGPTResourceURL(%q) = %q, want %q", in, got, want)
		}
	}
}
