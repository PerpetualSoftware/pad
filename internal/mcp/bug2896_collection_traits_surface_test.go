package mcp

import "testing"

// BUG-2896 added warnings.collapsed_duplicate_keys to collection create and
// update responses. It is set only when the request's `traits` repeats a JSON
// member, and pad_collection takes no `traits` param (catalog input validation
// refuses undeclared keys since v0.24), so no MCP call on either transport can
// produce it. That is why it came with no ToolSurfaceVersion bump.
//
// This fails the moment `traits` joins the catalog, because that is when the
// warning becomes reachable from MCP and the additive bump is owed (the rule
// BUG-2819 set for v0.53).
func TestPadCollectionTakesNoTraitsParam(t *testing.T) {
	if len(padCollectionTool.Schema.Params) == 0 {
		t.Fatal("pad_collection declares no params at all; this guard is not reading the catalog")
	}
	for _, p := range padCollectionTool.Schema.Params {
		if p.Name == "traits" {
			t.Fatal("pad_collection now takes `traits`, so warnings.collapsed_duplicate_keys (BUG-2896) can reach an MCP response: " +
				"bump ToolSurfaceVersion (additive) with a version.go entry, then update this guard")
		}
	}
}
