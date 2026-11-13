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
				found := false
				if method == "tools/list" {
					for _, tool := range response.Result["tools"] {
						if tool["name"] == "pad_item" {
							_, found = tool["_meta"]
						}
					}
				} else {
					for _, r := range response.Result["resources"] {
						if strings.HasPrefix(r["uri"].(string), "ui://") {
							found = true
						}
					}
				}
				if found != enabled {
					t.Fatalf("%s apps=%v, want %v", method, found, enabled)
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
	if !strings.HasPrefix(strings.ToLower(html), "<!doctype html") {
		t.Fatal("missing HTML doctype")
	}
	if len(pad.MCPAppItemCardHTML) > 512*1024 {
		t.Fatal("card exceeds 512 KiB")
	}
	if regexp.MustCompile(`(?i)(src|href)\s*=\s*["']https?://`).MatchString(html) {
		t.Fatal("card references external assets")
	}
}
