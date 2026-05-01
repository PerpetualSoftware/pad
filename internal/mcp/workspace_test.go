package mcp

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestWorkspaceState_GetSetRoundtrip(t *testing.T) {
	s := NewWorkspaceState("")
	if got := s.Get(); got != "" {
		t.Errorf("default = %q, want empty", got)
	}
	s.Set("docapp")
	if got := s.Get(); got != "docapp" {
		t.Errorf("after Set, got %q, want docapp", got)
	}
	s.Set("")
	if got := s.Get(); got != "" {
		t.Errorf("after clear, got %q, want empty", got)
	}
}

func TestWorkspaceState_InitialValueIsHonored(t *testing.T) {
	// pad mcp serve seeds state from --workspace flag — that path
	// must be reflected in Get() before any tool call mutates it.
	s := NewWorkspaceState("seeded-ws")
	if got := s.Get(); got != "seeded-ws" {
		t.Errorf("seeded value lost: got %q", got)
	}
}

func TestWorkspaceState_ConcurrentAccessRaceFree(t *testing.T) {
	// Run with -race to catch lock regressions. mcp-go's stdio
	// transport processes requests in a worker pool, so concurrent
	// Get/Set is the real-world pattern.
	s := NewWorkspaceState("")
	var wg sync.WaitGroup
	const goroutines = 32
	for i := 0; i < goroutines; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); s.Set("a") }()
		go func() { defer wg.Done(); _ = s.Get() }()
	}
	wg.Wait()
}

func TestSetWorkspaceTool_HandlerUpdatesState(t *testing.T) {
	state := NewWorkspaceState("")
	tool, handler := SetWorkspaceTool(state)

	if tool.Name != SetWorkspaceToolName {
		t.Errorf("tool name = %q, want %q", tool.Name, SetWorkspaceToolName)
	}

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"workspace": "docapp"}
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Errorf("expected success, got IsError")
	}
	if got := state.Get(); got != "docapp" {
		t.Errorf("state.Get() = %q, want docapp", got)
	}
}

func TestSetWorkspaceTool_EmptyStringClears(t *testing.T) {
	// Per the tool's contract docstring: empty string clears the
	// session default. If this regresses, agents can't go back to
	// "no default workspace" mid-session.
	state := NewWorkspaceState("docapp")
	_, handler := SetWorkspaceTool(state)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"workspace": ""}
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Errorf("clearing should succeed, got IsError")
	}
	if state.Get() != "" {
		t.Errorf("expected cleared state, got %q", state.Get())
	}
}

func TestSetWorkspaceTool_MissingArgReturnsError(t *testing.T) {
	state := NewWorkspaceState("untouched")
	_, handler := SetWorkspaceTool(state)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError result for missing required arg")
	}
	// State must NOT be mutated on validation failure — otherwise a
	// malformed call could clear an agent's working workspace.
	if state.Get() != "untouched" {
		t.Errorf("state mutated on validation failure: got %q", state.Get())
	}
	// Smoke-check the error message mentions the offending arg.
	if len(res.Content) > 0 {
		var combined strings.Builder
		for _, c := range res.Content {
			combined.WriteString(asText(c))
		}
		if !strings.Contains(combined.String(), "workspace") {
			t.Errorf("error message should mention 'workspace'; got: %q", combined.String())
		}
	}
}

// asText extracts the text payload from any content type that has one.
// mcp-go's Content interface is implemented by TextContent, etc.;
// we only care about text for these unit tests.
func asText(c mcp.Content) string {
	if tc, ok := c.(mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}
