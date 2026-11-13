package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

func appsWireResult(t *testing.T, srv *server.MCPServer, method string, params any) map[string]any {
	t.Helper()
	req, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(srv.HandleMessage(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result map[string]any `json:"result"`
		Error  any            `json:"error"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != nil || response.Result == nil {
		t.Fatalf("%s: %s", method, raw)
	}
	return response.Result
}

func appsTestServer(t *testing.T, enabled bool) *server.MCPServer {
	t.Helper()
	srv := server.NewMCPServer("test", "1", server.WithResourceCapabilities(false, false))
	_, err := Register(srv, RegistryOptions{Doc: fixtureDoc(), Workspace: NewWorkspaceState(""), Dispatcher: &fakeDispatcher{}, Apps: enabled, AppUI: AppUI{ItemCardHTML: []byte("<!doctype html><p>fixed card</p>")}})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestMCPApps_ItemToolCarriesUIMeta(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			tools := appsWireResult(t, appsTestServer(t, enabled), "tools/list", map[string]any{})["tools"].([]any)
			found := false
			for _, entry := range tools {
				tool := entry.(map[string]any)
				meta, present := tool["_meta"]
				if enabled && tool["name"] == "pad_item" {
					found = true
					m, ok := meta.(map[string]any)
					if !ok {
						t.Fatalf("missing item meta: %v", tool)
					}
					ui, ok := m["ui"].(map[string]any)
					if !ok || ui["resourceUri"] != "ui://pad/item-card" || m["ui/resourceUri"] != "ui://pad/item-card" {
						t.Fatalf("bad meta: %v", m)
					}
				} else if present && !(enabled && tool["name"] == "pad_board") {
					t.Fatalf("unexpected meta: %v", tool)
				}
			}
			if enabled && !found {
				t.Fatal("pad_item missing")
			}
		})
	}
}

func TestMCPApps_ResourceRead(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			srv := appsTestServer(t, enabled)
			resources, _ := appsWireResult(t, srv, "resources/list", map[string]any{})["resources"].([]any)
			found := false
			for _, entry := range resources {
				r := entry.(map[string]any)
				uri := r["uri"].(string)
				if !enabled && strings.HasPrefix(uri, "ui://") {
					t.Fatalf("unexpected resource: %v", r)
				}
				if uri == "ui://pad/item-card" {
					found = true
					if r["name"] != "Pad item card" || r["mimeType"] != "text/html;profile=mcp-app" {
						t.Fatalf("bad resource: %v", r)
					}
				}
			}
			if found != enabled {
				t.Fatalf("resource found=%v, enabled=%v", found, enabled)
			}
			if !enabled {
				return
			}
			contents := appsWireResult(t, srv, "resources/read", map[string]any{"uri": "ui://pad/item-card"})["contents"].([]any)
			if len(contents) != 1 {
				t.Fatalf("contents: %v", contents)
			}
			c := contents[0].(map[string]any)
			if c["text"] != "<!doctype html><p>fixed card</p>" || c["mimeType"] != "text/html;profile=mcp-app" || c["uri"] != "ui://pad/item-card" {
				t.Fatalf("bad contents: %v", c)
			}
			ui := c["_meta"].(map[string]any)["ui"].(map[string]any)
			if ui["prefersBorder"] != true {
				t.Fatalf("bad ui meta: %v", ui)
			}
			for _, key := range []string{"csp", "domain"} {
				if _, ok := ui[key]; ok {
					t.Fatalf("unexpected %s", key)
				}
			}
		})
	}
}
