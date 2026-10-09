package main

import (
	"testing"

	mcpserver "github.com/PerpetualSoftware/pad/internal/mcp"
)

// TASK-2308: the production wiring hands the /mcp mount the catalog's own
// write classifier. The middleware is tested through the router in
// internal/server with a stand-in classifier; this pins that wireMCP binds
// the real one, for every catalog action.
func TestTASK2308_WireMCPBindsTheCatalogClassifier(t *testing.T) {
	f := newCapFixture(t, cfgCloud, true, nil, "")
	writes := 0
	for _, def := range mcpserver.Catalog {
		for action := range def.Actions {
			want := mcpserver.CallNeedsWriteScope(def.Name, action)
			if got := f.srv.MCPCallNeedsWrite(def.Name, action); got != want {
				t.Errorf("%s.%s: server classifier says %v, catalog says %v", def.Name, action, got, want)
			}
			if want {
				writes++
			}
		}
	}
	if writes == 0 {
		t.Fatal("no catalog action is a write; the comparison proves nothing")
	}
	if !f.srv.MCPCallNeedsWrite("pad_item", "create") || f.srv.MCPCallNeedsWrite("pad_item", "get") {
		t.Error("pad_item.create must need write and pad_item.get must not")
	}
	if f.srv.MCPCallNeedsWrite("pad_set_workspace", "") {
		t.Error("pad_set_workspace has no action and only reads; it must not need write")
	}
}
