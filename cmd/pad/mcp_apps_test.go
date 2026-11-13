package main

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	pad "github.com/PerpetualSoftware/pad"
	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	mcpserver "github.com/PerpetualSoftware/pad/internal/mcp"
)

func TestMCPApps_KillSwitch(t *testing.T) {
	// Same vocabulary as PAD_MCP_ENABLED: false/0/no/off (any case, trimmed)
	// turn apps off; anything else leaves them on.
	cases := map[string]bool{
		"<unset>": true, "": true, "on": true, "true": true, "1": true, "anything": true,
		"off": false, "OFF": false, "oFf": false, " off ": false,
		"false": false, "FALSE": false, "0": false, "no": false, "No": false,
	}
	for value, enabled := range cases {
		t.Run(value, func(t *testing.T) {
			t.Setenv("PAD_MCP_APPS", value)
			if value == "<unset>" {
				if err := os.Unsetenv("PAD_MCP_APPS"); err != nil {
					t.Fatal(err)
				}
			}
			root := newRootCmd()
			doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", Version: fullVersion(), Homepage: padHomepage, MaxDepth: -1})
			srv := mcpserver.NewServer(mcpserver.Options{Version: fullVersion()})
			if err := registerRemoteMCP(srv, doc, &mcpserver.HTTPHandlerDispatcher{}); err != nil {
				t.Fatal(err)
			}
			for _, method := range []string{"tools/list", "resources/list"} {
				req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method})
				raw, err := json.Marshal(srv.MCP().HandleMessage(context.Background(), req))
				if err != nil {
					t.Fatal(err)
				}
				var response struct {
					Result map[string][]map[string]any `json:"result"`
					Error  any                         `json:"error"`
				}
				if err := json.Unmarshal(raw, &response); err != nil {
					t.Fatal(err)
				}
				if response.Error != nil || response.Result == nil {
					t.Fatalf("%s: %s", method, raw)
				}
				found, boardFound := false, false
				if method == "tools/list" {
					for _, tool := range response.Result["tools"] {
						if tool["name"] == "pad_item" {
							_, found = tool["_meta"]
						}
						if tool["name"] == "pad_board" {
							boardFound = true
						}
					}
				} else {
					for _, r := range response.Result["resources"] {
						if strings.HasPrefix(r["uri"].(string), "ui://") {
							found = true
						}
						if r["uri"] == "ui://pad/board" {
							boardFound = true
						}
					}
				}
				if found != enabled {
					t.Fatalf("%s apps=%v, want %v", method, found, enabled)
				}
				if boardFound != enabled {
					t.Fatalf("%s board=%v, want %v", method, boardFound, enabled)
				}
			}
		})
	}
}

func TestMCPApps_EmbeddedBundleSelfContained(t *testing.T) {
	html := strings.TrimSpace(string(pad.MCPAppItemCardHTML))
	if html == "placeholder" || html == "// placeholder" {
		t.Skip("MCP App placeholder; build the card to validate the bundle")
	}
	// A real card means build-mcp-app.mjs ran, and it writes the board too.
	// The server reads an absent board.html as empty HTML (placeholder builds
	// create only the card), so a real build must prove the board is there.
	board, err := pad.MCPAppFS.ReadFile("web/build-mcp-app/board.html")
	if err != nil {
		t.Fatalf("real card build without board.html: %v", err)
	}
	for name, raw := range map[string][]byte{"card": pad.MCPAppItemCardHTML, "board": board} {
		text := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(strings.ToLower(text), "<!doctype html") {
			t.Fatalf("%s: missing HTML doctype", name)
		}
		if len(raw) > 512*1024 {
			t.Fatalf("%s exceeds 512 KiB", name)
		}
		if regexp.MustCompile(`(?i)(src|href)\s*=\s*["']https?://`).MatchString(text) {
			t.Fatalf("%s references external assets", name)
		}
	}
}
