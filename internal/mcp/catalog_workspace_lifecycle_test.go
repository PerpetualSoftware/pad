package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// MCP catalog wiring tests for the workspace-lifecycle actions
// (PLAN-1519 / TASK-1521 / IDEA-1517 §1 + §4).
//
// The catalog is the contract — agents discover what they can call
// from the schema we expose. These tests pin:
//
//   - Both actions are present in pad_workspace's action enum.
//   - The new params (name, slug, template, code) are advertised.
//   - The tool description mentions both actions so the inline doc
//     keeps step with the schema.
//   - The HTTP route table has entries for "workspace create" and
//     "workspace claim" (otherwise cloud MCP would return "not yet
//     implemented over HTTP transport").
//   - The route mappers produce the right method + path + body
//     shape for canonical input.

func TestCatalogWorkspace_LifecycleActionsRegistered(t *testing.T) {
	// Source-of-truth check: the Actions map on the ToolDef is what
	// makeFanOutHandler dispatches on. The schema test below is the
	// agent-facing-surface check. Both must agree.
	for _, want := range []string{"create", "claim"} {
		if _, ok := padWorkspaceTool.Actions[want]; !ok {
			t.Errorf("padWorkspaceTool.Actions missing %q (dispatcher would 404 the call)", want)
		}
	}

	tool := buildToolFromDef(padWorkspaceTool)
	props := tool.InputSchema.Properties

	// action enum must include both new verbs. The mcp-go library
	// stores enum as []string OR []any depending on builder used;
	// extractEnumStrings normalizes both.
	actionProp, _ := props["action"].(map[string]any)
	enum := extractEnumStrings(actionProp["enum"])
	for _, want := range []string{"create", "claim"} {
		if !enum[want] {
			t.Errorf("pad_workspace action enum missing %q (got %v)", want, actionProp["enum"])
		}
	}

	// New params advertised on the input schema.
	for _, p := range []string{"name", "slug", "template", "code"} {
		if _, ok := props[p]; !ok {
			t.Errorf("pad_workspace input schema missing param %q", p)
		}
	}
}

// extractEnumStrings normalizes the schema's enum representation to a
// string set, accepting either []string or []any (mcp-go's exact
// shape has flipped historically; defensive code keeps the test
// stable across library bumps).
func extractEnumStrings(raw any) map[string]bool {
	out := map[string]bool{}
	switch v := raw.(type) {
	case []string:
		for _, s := range v {
			out[s] = true
		}
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

func TestCatalogWorkspace_DescriptionMentionsLifecycle(t *testing.T) {
	desc := padWorkspaceToolDescription
	for _, want := range []string{
		"create",
		"claim",
		"may_create_workspaces",
		"6-digit",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("pad_workspace description should mention %q; got %s", want, desc)
		}
	}
}

func TestWorkspaceCreateRoute_HTTPMappingShape(t *testing.T) {
	method, path, body, err := mapWorkspaceCreate(map[string]any{
		"name":     "My New WS",
		"slug":     "my-new-ws",
		"template": "startup",
	})
	if err != nil {
		t.Fatalf("mapWorkspaceCreate: %v", err)
	}
	if method != "POST" {
		t.Errorf("method = %q, want POST", method)
	}
	if path != "/api/v1/workspaces" {
		t.Errorf("path = %q, want /api/v1/workspaces", path)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for k, want := range map[string]string{"name": "My New WS", "slug": "my-new-ws", "template": "startup"} {
		if got[k] != want {
			t.Errorf("body[%q] = %v, want %q", k, got[k], want)
		}
	}
}

func TestWorkspaceCreateRoute_RequiresName(t *testing.T) {
	_, _, _, err := mapWorkspaceCreate(map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Errorf("missing name should error; got %v", err)
	}
}

func TestWorkspaceCreateRoute_OmitsOptionalEmptyFields(t *testing.T) {
	// `workspace` arrives via session-default injection but is
	// irrelevant for CREATE. The mapper should produce a body that
	// only carries name + the optional fields the caller actually
	// supplied — not a stale `workspace` slug.
	_, _, body, err := mapWorkspaceCreate(map[string]any{
		"name":      "Minimal",
		"workspace": "some-other-ws",
	})
	if err != nil {
		t.Fatalf("mapWorkspaceCreate: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if _, has := got["workspace"]; has {
		t.Errorf("body should not carry workspace on create: %v", got)
	}
	if _, has := got["slug"]; has {
		t.Errorf("body should omit empty slug: %v", got)
	}
	if _, has := got["template"]; has {
		t.Errorf("body should omit empty template: %v", got)
	}
}

func TestWorkspaceClaimRoute_HTTPMappingShape(t *testing.T) {
	method, path, body, err := mapWorkspaceClaim(map[string]any{
		"workspace": "my-cool-project",
		"code":      "123456",
	})
	if err != nil {
		t.Fatalf("mapWorkspaceClaim: %v", err)
	}
	if method != "POST" {
		t.Errorf("method = %q, want POST", method)
	}
	if path != "/api/v1/oauth/claim" {
		t.Errorf("path = %q, want /api/v1/oauth/claim", path)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["workspace"] != "my-cool-project" {
		t.Errorf("body workspace = %v, want my-cool-project", got["workspace"])
	}
	if got["code"] != "123456" {
		t.Errorf("body code = %v, want 123456", got["code"])
	}
}

func TestWorkspaceClaimRoute_RequiresWorkspaceAndCode(t *testing.T) {
	cases := []map[string]any{
		{},
		{"workspace": "x"},                  // missing code
		{"code": "123456"},                  // missing workspace
		{"workspace": "", "code": "123456"}, // empty
	}
	for i, in := range cases {
		_, _, _, err := mapWorkspaceClaim(in)
		if err == nil {
			t.Errorf("case %d %v: should error on missing/empty inputs", i, in)
		}
	}
}

func TestRouteTable_RegistersWorkspaceLifecycle(t *testing.T) {
	for _, key := range []string{"workspace create", "workspace claim"} {
		if _, ok := routeTable[key]; !ok {
			t.Errorf("routeTable missing entry for %q — cloud MCP dispatch would fail", key)
		}
	}
}
