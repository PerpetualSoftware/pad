package mcp

import "testing"

// BUG-2817: IsKnownCallName is the bound on the MCP metrics tool label. The
// server is built the way cmd/pad builds the remote one (NewServer, then
// Register). The predicate answers from the live server (GetTool); this test
// draws the names it checks from Catalog, a source independent of that, so a
// catalog tool the live server does not report fails here.
func TestIsKnownCallName(t *testing.T) {
	srv := NewServer(Options{Version: "metrics-names-test"})

	// Before registration no catalog tool is known: the predicate asks the
	// live server, it does not carry a copied list.
	if srv.IsKnownCallName(Catalog[0].Name) {
		t.Fatalf("%s is known before any tool is registered", Catalog[0].Name)
	}

	if _, err := Register(srv.MCP(), RegistryOptions{
		Doc:        fixtureDoc(),
		Workspace:  NewWorkspaceState(""),
		Dispatcher: &fakeDispatcher{},
		PadVersion: "test",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	for _, td := range Catalog {
		if !srv.IsKnownCallName(td.Name) {
			t.Errorf("catalog tool %q is not a known call name", td.Name)
		}
	}
	if !srv.IsKnownCallName(SetWorkspaceToolName) {
		t.Errorf("%q is not a known call name", SetWorkspaceToolName)
	}
	for _, m := range []string{"initialize", "tools/list", "tools/call", "ping", "resources/read", "notifications/initialized"} {
		if !srv.IsKnownCallName(m) {
			t.Errorf("protocol method %q is not a known call name", m)
		}
	}
	for _, bogus := range []string{"", "pad_itemx", "PAD_ITEM", "pad_item ", "tools/call2", "(unknown)"} {
		if srv.IsKnownCallName(bogus) {
			t.Errorf("caller-invented name %q is known; it would mint its own series", bogus)
		}
	}
}
