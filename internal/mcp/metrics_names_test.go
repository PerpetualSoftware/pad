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
	// Literal strings, not the mcp constants protocolMethods is built from, so
	// an entry dropped from that list fails here (every method mcp-go v1.1.0
	// defines).
	for _, m := range []string{
		"completion/complete",
		"elicitation/create",
		"initialize",
		"logging/setLevel",
		"notifications/cancelled",
		"notifications/elicitation/complete",
		"notifications/initialized",
		"notifications/message",
		"notifications/progress",
		"notifications/prompts/list_changed",
		"notifications/resources/list_changed",
		"notifications/resources/updated",
		"notifications/roots/list_changed",
		"notifications/subscriptions/acknowledged",
		"notifications/tasks/status",
		"notifications/tools/list_changed",
		"ping",
		"prompts/get",
		"prompts/list",
		"resources/list",
		"resources/read",
		"resources/subscribe",
		"resources/templates/list",
		"resources/unsubscribe",
		"roots/list",
		"sampling/createMessage",
		"server/discover",
		"subscriptions/listen",
		"tasks/cancel",
		"tasks/get",
		"tasks/list",
		"tasks/result",
		"tools/call",
		"tools/list",
	} {
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
