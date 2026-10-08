package mcp

import (
	"encoding/json"
	"testing"

	protocol "github.com/mark3labs/mcp-go/mcp"
)

// These budgets are FIXED CEILINGS on the prompt-bearing MCP payloads every
// agent receives (lead ruling, TASK-3489). Aim to keep about 2 KiB of headroom
// under each. When the headroom drops under about 512 bytes, TRIM the text
// (move detail into docs/mcp.md, keeping every rule an agent acts on) rather
// than raising the ceiling. A trim does not lower the ceiling either: a budget
// that ratchets down on every reduction turns the next small addition into a
// budget fight, while a fixed ceiling and a trim rule keep growth visible.
const (
	mcpInitializePromptBudget = 24 * 1024
	mcpToolListPromptBudget   = 54 * 1024
)

func TestMCPPromptBudgets(t *testing.T) {
	srv := NewServer(Options{Version: "prompt-budget-test"})
	registered, err := Register(srv.MCP(), RegistryOptions{
		Doc:        fixtureDoc(),
		Workspace:  NewWorkspaceState(""),
		Dispatcher: &fakeDispatcher{},
		PadVersion: "test",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	client, initialize, cleanup := runClientSession(t, srv)
	defer cleanup()

	tools, err := client.ListTools(t.Context(), protocol.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != registered {
		t.Fatalf("ListTools returned %d tools; registered %d", len(tools.Tools), registered)
	}

	initializeJSON, err := json.Marshal(initialize)
	if err != nil {
		t.Fatalf("marshal initialize result: %v", err)
	}
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal tools/list result: %v", err)
	}

	t.Logf("MCP initialize payload: %d bytes (budget %d)", len(initializeJSON), mcpInitializePromptBudget)
	t.Logf("MCP tools/list payload: %d bytes (budget %d)", len(toolsJSON), mcpToolListPromptBudget)
	if len(initializeJSON) > mcpInitializePromptBudget {
		t.Errorf("MCP initialize payload is %d bytes; budget is %d", len(initializeJSON), mcpInitializePromptBudget)
	}
	if len(toolsJSON) > mcpToolListPromptBudget {
		t.Errorf("MCP tools/list payload is %d bytes; budget is %d", len(toolsJSON), mcpToolListPromptBudget)
	}
}
