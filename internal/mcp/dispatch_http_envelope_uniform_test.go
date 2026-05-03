package mcp

// Tests for TASK-1077 / 1078 / 1079 — pin the uniform error envelope
// shape across every HTTP-dispatcher tool.
//
// Pre-fix some dispatchers (note, decide, project next, bulk-update)
// emitted plain-string errors via NewToolResultErrorf. Post-fix every
// error path goes through validationFailedResult / dispatcherErrorResult
// / upstreamHTTPErrorResult, which all wrap the standard envelope:
//
//	{"error": {"code": "...", "message": "...", "hint": "..."}}
//
// This test parameterizes over each dispatcher's missing-required-input
// error path because that's the cheapest reproducible failure case
// (no need to mock a backend handler) and the path most agents will
// hit when learning the surface.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestDispatcher_AllErrorsUseStructuredEnvelope walks every special-
// case dispatcher + each link command and asserts that calling them
// without the required workspace/ref/etc. produces a structured
// {error: {code, message, hint}} envelope — never a bare string.
//
// New dispatchers that emit errors via NewToolResultErrorf will fail
// this test, which is the regression gate TASK-1077 wants.
func TestDispatcher_AllErrorsUseStructuredEnvelope(t *testing.T) {
	user := &models.User{ID: "u-1", Name: "Tester"}
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not call backend"),
		UserResolver: fixedUserResolver(user),
	}

	cases := []struct {
		name     string
		cmdPath  []string
		input    map[string]any
		wantCode ErrorCode // expected envelope code
	}{
		// Missing workspace on every workspace-required dispatcher.
		// The cmdPath list mirrors the special-case switch in
		// HTTPHandlerDispatcher.Dispatch — adding a new entry there
		// without a matching case here is a smell.
		{"item update no workspace", []string{"item", "update"}, map[string]any{}, ErrValidationFailed},
		{"item deps no workspace", []string{"item", "deps"}, map[string]any{}, ErrValidationFailed},
		{"item related no workspace", []string{"item", "related"}, map[string]any{}, ErrValidationFailed},
		{"item implemented-by no workspace", []string{"item", "implemented-by"}, map[string]any{}, ErrValidationFailed},
		{"item bulk-update no workspace", []string{"item", "bulk-update"}, map[string]any{}, ErrValidationFailed},
		{"item note no workspace", []string{"item", "note"}, map[string]any{}, ErrValidationFailed},
		{"item decide no workspace", []string{"item", "decide"}, map[string]any{}, ErrValidationFailed},
		{"project ready no workspace", []string{"project", "ready"}, map[string]any{}, ErrValidationFailed},
		{"project stale no workspace", []string{"project", "stale"}, map[string]any{}, ErrValidationFailed},
		{"project next no workspace", []string{"project", "next"}, map[string]any{}, ErrValidationFailed},
		{"project standup no workspace", []string{"project", "standup"}, map[string]any{}, ErrValidationFailed},
		{"project changelog no workspace", []string{"project", "changelog"}, map[string]any{}, ErrValidationFailed},
		{"attachment list no workspace", []string{"attachment", "list"}, map[string]any{}, ErrValidationFailed},
		{"attachment show no workspace", []string{"attachment", "show"}, map[string]any{}, ErrValidationFailed},

		// Link commands — workspace required, then refs.
		{"item block no workspace", []string{"item", "block"}, map[string]any{}, ErrValidationFailed},
		{"item blocked-by no workspace", []string{"item", "blocked-by"}, map[string]any{}, ErrValidationFailed},
		{"item unblock no workspace", []string{"item", "unblock"}, map[string]any{}, ErrValidationFailed},

		// Library commands.
		{"library activate no workspace", []string{"library", "activate"}, map[string]any{}, ErrValidationFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithDispatchInput(context.Background(), tc.input)
			res, err := d.Dispatch(ctx, tc.cmdPath, nil)
			if err != nil {
				t.Fatalf("Dispatch protocol error: %v", err)
			}
			if res == nil {
				t.Fatal("nil result")
			}
			if !res.IsError {
				t.Errorf("expected IsError=true; result=%+v", res)
			}
			env := unwrapErrorEnvelope(t, res)
			if env.Error.Code != tc.wantCode {
				t.Errorf("code: got %q, want %q (full envelope: %+v)",
					env.Error.Code, tc.wantCode, env.Error)
			}
			if env.Error.Message == "" {
				t.Error("envelope missing Message")
			}
			if env.Error.Hint == "" {
				t.Error("envelope missing Hint — agents need actionable recovery text (TASK-1079)")
			}
			// Hints should NOT be the literal "404 page not found" /
			// "page not found" passthrough that triggered Bug 17.
			if strings.EqualFold(env.Error.Hint, "404 page not found") ||
				strings.EqualFold(env.Error.Hint, "page not found") {
				t.Errorf("hint is the raw HTTP status text — not actionable (Bug 17 / TASK-1079): %q",
					env.Error.Hint)
			}
		})
	}
}

// TestClassifyHTTPStatus_KindAware pins TASK-1078's resource-kind
// classification — 404s split into item_not_found / unknown_workspace /
// not_found based on what the dispatcher was reading, instead of the
// pre-fix blanket item_not_found.
func TestClassifyHTTPStatus_KindAware(t *testing.T) {
	cases := []struct {
		name     string
		kind     ResourceKind
		wantCode ErrorCode
	}{
		{"item lookup → item_not_found", ResourceItem, ErrItemNotFound},
		{"workspace lookup → unknown_workspace", ResourceWorkspace, ErrUnknownWorkspace},
		{"collection lookup → not_found", ResourceCollection, ErrNotFound},
		{"listing route → not_found", ResourceListing, ErrNotFound},
		{"link target → not_found", ResourceLink, ErrNotFound},
		{"attachment → not_found", ResourceAttachment, ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := classifyHTTPStatusKind(context.Background(),
				"item show", "/api/v1/workspaces/foo/items/TASK-7",
				404, []byte("404 page not found"), nil, tc.kind, "TASK-7")
			env, ok := res.StructuredContent.(ErrorEnvelope)
			if !ok {
				t.Fatalf("expected ErrorEnvelope, got %T", res.StructuredContent)
			}
			if env.Error.Code != tc.wantCode {
				t.Errorf("code: got %q, want %q", env.Error.Code, tc.wantCode)
			}
		})
	}
}

// TestClassifyHTTPStatus_HintsAreActionable pins TASK-1079's hint
// improvements: hints reference the actual route + ref (or workspace
// slug) the dispatcher was reading, and never just dump the upstream
// "404 page not found" passthrough.
func TestClassifyHTTPStatus_HintsAreActionable(t *testing.T) {
	cases := []struct {
		name             string
		kind             ResourceKind
		ref              string
		route            string
		body             []byte
		mustContain      []string
		mustNotContainEq string // forbid this as the WHOLE hint
	}{
		{
			name:        "item 404 hint references the ref + recovery tools",
			kind:        ResourceItem,
			ref:         "TASK-7",
			route:       "/api/v1/workspaces/foo/items/TASK-7",
			body:        []byte("404 page not found"),
			mustContain: []string{"TASK-7", "/api/v1/workspaces/foo/items/TASK-7", "pad_item"},
		},
		{
			name:        "collection 404 hint suggests the listing tool + names route",
			kind:        ResourceCollection,
			ref:         "tasks",
			route:       "/api/v1/workspaces/foo/collections/tasks",
			body:        []byte("404 page not found"),
			mustContain: []string{"pad_collection list", "/api/v1/workspaces/foo/collections/tasks"},
		},
		{
			name:        "5xx hint suggests retry + flags as transient",
			kind:        ResourceItem, // kind doesn't matter for 5xx
			ref:         "TASK-7",
			route:       "/api/v1/workspaces/foo/items/TASK-7",
			body:        []byte(`{"error":{"message":"db down"}}`),
			mustContain: []string{"transient", "retry", "500"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := 404
			if strings.Contains(tc.name, "5xx") {
				status = 500
			}
			res := classifyHTTPStatusKind(context.Background(),
				"test cmd", tc.route, status, tc.body, nil, tc.kind, tc.ref)
			env := res.StructuredContent.(ErrorEnvelope)
			if env.Error.Hint == "" {
				t.Fatal("hint is empty")
			}
			for _, want := range tc.mustContain {
				if !strings.Contains(env.Error.Hint, want) {
					t.Errorf("hint missing %q; got %q", want, env.Error.Hint)
				}
			}
			// Forbid the bare "404 page not found" passthrough that
			// triggered Bug 17.
			if env.Error.Hint == "404 page not found" {
				t.Errorf("hint is the raw HTTP status text passthrough (Bug 17 regression)")
			}
		})
	}
}

// TestExtractUpstreamMessage covers each shape the helper handles —
// envelope happy path, chi default 404, and the safe-fallback cases.
//
// Codex review #387 round 1 caught that the pre-fix fallback returned
// the raw body verbatim, which would forward arbitrary upstream JSON
// (potentially including tokens / passwords / debug dumps) into the
// hint surface. Post-fix non-envelope bodies return "" — the hint
// surface omits the upstream-message clause entirely and operators
// debugging unstructured upstream errors check the pad container
// logs instead.
func TestExtractUpstreamMessage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Happy path — pad's writeError envelope shape.
		{"envelope happy path", `{"error":{"message":"workspace not visible"}}`, "workspace not visible"},
		{"envelope with sibling fields", `{"error":{"message":"db down"},"code":"db_err"}`, "db down"},

		// chi's default NotFound body — known-zero-value, drop it.
		{"chi 404 literal", "404 page not found", ""},
		{"chi 404 with trailing newline", "404 page not found\n", ""},

		// Empty / whitespace input → empty output.
		{"empty", "", ""},
		{"whitespace only", "   ", ""},

		// Safe fallbacks: anything we can't recognize as the structured
		// envelope returns "" rather than leaking the raw body.
		{"empty inner message", `{"error":{"message":""}}`, ""},
		{"missing inner message field", `{"error":{}}`, ""},
		{"unparseable", "not json at all", ""},
		{"wrong shape", `{"unrelated":"shape"}`, ""},
		{"html debug dump with token-like content", `<html>internal stack trace with token=abc</html>`, ""},
		{"sensitive-looking JSON (NOT envelope shape)", `{"secret":"abc","token":"xyz"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractUpstreamMessage(tc.in); got != tc.want {
				t.Errorf("extractUpstreamMessage(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// unwrapErrorEnvelope extracts the structured ErrorEnvelope from a
// CallToolResult. Test helper — fails fast if the result isn't shaped
// like a structured envelope (which is the whole regression we're
// guarding against).
func unwrapErrorEnvelope(t *testing.T, res *mcp.CallToolResult) ErrorEnvelope {
	t.Helper()
	if res == nil {
		t.Fatal("result is nil")
	}
	if env, ok := res.StructuredContent.(ErrorEnvelope); ok {
		return env
	}
	// Fallback: parse the JSON content. Some helpers use
	// NewErrorResult which sets StructuredContent directly; others
	// might not have the typed struct populated — try to round-trip
	// from the text content for robustness.
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(mcp.TextContent); ok {
			var env ErrorEnvelope
			if err := json.Unmarshal([]byte(tc.Text), &env); err == nil && env.Error.Code != "" {
				return env
			}
			t.Fatalf("text content isn't a parseable ErrorEnvelope: %s", tc.Text)
		}
	}
	t.Fatalf("result has no parseable error envelope; result=%+v", res)
	return ErrorEnvelope{}
}
