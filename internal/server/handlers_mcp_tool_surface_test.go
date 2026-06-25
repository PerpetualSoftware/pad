package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// toolSurfaceFixture is a minimal stand-in for the real
// mcp.ToolSurfaceJSON serializer. internal/server can't import
// internal/mcp (import cycle — see SetToolSurfaceHandler), so the test
// injects its own serializer producing the same shape: the catalog's
// nine tools, each action carrying a read_only bool. This validates the
// endpoint wiring (auth gating, injection, JSON passthrough, no
// leakage) independently of the real catalog's contents — exactly the
// boundary the production injection crosses.
func toolSurfaceFixture() ([]byte, error) {
	// Shape mirrors mcp/tool_surface.go's buildToolSurfacePayload: the
	// nine env.Catalog tools (pad_set_workspace is registered separately
	// and excluded), each with read_only-annotated actions.
	type action struct {
		Name     string `json:"name"`
		ReadOnly bool   `json:"read_only"`
	}
	type tool struct {
		Name    string   `json:"name"`
		Actions []action `json:"actions"`
	}
	names := []string{
		"pad_item", "pad_workspace", "pad_collection", "pad_project",
		"pad_role", "pad_search", "pad_meta", "pad_playbook", "pad_library",
	}
	tools := make([]tool, 0, len(names))
	for _, n := range names {
		tools = append(tools, tool{
			Name:    n,
			Actions: []action{{Name: "list", ReadOnly: true}, {Name: "create", ReadOnly: false}},
		})
	}
	return json.Marshal(map[string]any{
		"tool_surface_version": "0.7",
		"rollout_status":       "complete",
		"tools":                tools,
	})
}

// TestMCPToolSurface_RequiresAuth pins the auth gate: with a user
// present (no fresh-install bypass), an unauthenticated GET must 401.
func TestMCPToolSurface_RequiresAuth(t *testing.T) {
	srv := testServer(t)
	srv.SetToolSurfaceHandler(toolSurfaceFixture)

	// Create the first admin so RequireAuth no longer waves everything
	// through under the fresh-install (UserCount==0) bypass.
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr := doRequest(srv, "GET", "/api/v1/mcp/tool-surface", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestMCPToolSurface_AuthedReturnsCatalog pins the happy path: an
// authenticated caller gets 200 with the nine catalog tools and
// read_only flags present on every action.
func TestMCPToolSurface_AuthedReturnsCatalog(t *testing.T) {
	srv := testServer(t)
	srv.SetToolSurfaceHandler(toolSurfaceFixture)

	token := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr := doRequestWithCookie(srv, "GET", "/api/v1/mcp/tool-surface", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("authed: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		ToolSurfaceVersion string `json:"tool_surface_version"`
		Tools              []struct {
			Name    string `json:"name"`
			Actions []struct {
				Name     string `json:"name"`
				ReadOnly bool   `json:"read_only"`
			} `json:"actions"`
		} `json:"tools"`
	}
	parseJSON(t, rr, &resp)

	if resp.ToolSurfaceVersion == "" {
		t.Error("expected tool_surface_version in payload")
	}

	// Exactly the nine env.Catalog tools — pad_set_workspace excluded.
	const wantTools = 9
	if len(resp.Tools) != wantTools {
		t.Fatalf("expected %d catalog tools, got %d", wantTools, len(resp.Tools))
	}
	expected := map[string]bool{
		"pad_item": false, "pad_workspace": false, "pad_collection": false,
		"pad_project": false, "pad_role": false, "pad_search": false,
		"pad_meta": false, "pad_playbook": false, "pad_library": false,
	}
	for _, tl := range resp.Tools {
		if _, ok := expected[tl.Name]; !ok {
			t.Errorf("unexpected tool %q in surface", tl.Name)
			continue
		}
		expected[tl.Name] = true
		if len(tl.Actions) == 0 {
			t.Errorf("tool %q: expected at least one action", tl.Name)
		}
		// pad_set_workspace must never appear.
		if tl.Name == "pad_set_workspace" {
			t.Errorf("pad_set_workspace leaked into the catalog surface")
		}
	}
	for name, seen := range expected {
		if !seen {
			t.Errorf("expected catalog tool %q missing from surface", name)
		}
	}
}

// TestMCPToolSurface_NoLeakage asserts the response carries only catalog
// descriptors — no internal route table, handler internals, or other
// server state. The fixture body is a closed set of keys; the endpoint
// must pass it through verbatim and add nothing.
func TestMCPToolSurface_NoLeakage(t *testing.T) {
	srv := testServer(t)
	srv.SetToolSurfaceHandler(toolSurfaceFixture)

	token := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr := doRequestWithCookie(srv, "GET", "/api/v1/mcp/tool-surface", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("authed: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var top map[string]json.RawMessage
	parseJSON(t, rr, &top)

	allowed := map[string]bool{
		"tool_surface_version": true,
		"rollout_status":       true,
		"tools":                true,
	}
	for k := range top {
		if !allowed[k] {
			t.Errorf("unexpected top-level key %q in tool-surface response (possible state leak)", k)
		}
	}
}

// TestMCPToolSurface_NotWired pins the nil-serializer path: when
// SetToolSurfaceHandler was never called, the endpoint 404s rather than
// panicking or returning an empty 200.
func TestMCPToolSurface_NotWired(t *testing.T) {
	srv := testServer(t)
	// Intentionally NOT calling SetToolSurfaceHandler.

	token := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr := doRequestWithCookie(srv, "GET", "/api/v1/mcp/tool-surface", nil, token)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("not-wired: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}
