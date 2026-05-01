package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// TestNewErrorResult_Envelope confirms the wire shape every error
// path produces. The test inspects:
//
//   - IsError flag set on the result.
//   - StructuredContent contains the envelope as a Go value.
//   - Text fallback parses back to the same envelope.
//
// MCP clients pick whichever surface they understand (Claude Desktop
// uses StructuredContent; older clients fall back to text). The
// invariant: both must agree.
func TestNewErrorResult_Envelope(t *testing.T) {
	payload := ErrorPayload{
		Code:    ErrNoWorkspace,
		Message: "No workspace context.",
		Hint:    "Available workspaces: docapp",
		AvailableWorkspaces: []WorkspaceHint{
			{Slug: "docapp", Name: "Pad", Default: true},
		},
	}
	res := NewErrorResult(payload)

	if !res.IsError {
		t.Errorf("IsError = false, want true")
	}
	if res.StructuredContent == nil {
		t.Errorf("StructuredContent missing — clients won't see the typed envelope")
	}

	// Round-trip via the text fallback.
	body := textOf(res)
	if body == "" {
		t.Fatalf("text fallback missing")
	}
	var got ErrorEnvelope
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal envelope: %v\nbody=%s", err, body)
	}
	if got.Error.Code != ErrNoWorkspace {
		t.Errorf("Code = %q, want %q", got.Error.Code, ErrNoWorkspace)
	}
	if got.Error.Message != payload.Message {
		t.Errorf("Message mismatch: got %q, want %q", got.Error.Message, payload.Message)
	}
	if len(got.Error.AvailableWorkspaces) != 1 {
		t.Fatalf("AvailableWorkspaces length = %d, want 1", len(got.Error.AvailableWorkspaces))
	}
	if got.Error.AvailableWorkspaces[0].Slug != "docapp" {
		t.Errorf("workspace slug = %q, want docapp", got.Error.AvailableWorkspaces[0].Slug)
	}
}

// fakeWorkspaceLister is a controllable WorkspaceLister for testing.
// Returns the configured slice unless ErrOut is set, in which case it
// errors (best-effort path test).
type fakeWorkspaceLister struct {
	hints  []WorkspaceHint
	errOut error
}

func (f *fakeWorkspaceLister) ListWorkspaces(_ context.Context) ([]WorkspaceHint, error) {
	if f.errOut != nil {
		return nil, f.errOut
	}
	return f.hints, nil
}

// TestClassifyExecError covers the full taxonomy via the ExecDispatcher
// classifier. Each subtest pairs a stderr pattern with the expected
// ErrorCode. Hits all 6 patterns the classifier recognizes plus the
// server_error fallback for unmatched output.
func TestClassifyExecError(t *testing.T) {
	lookup := &fakeWorkspaceLister{hints: []WorkspaceHint{
		{Slug: "docapp"}, {Slug: "pad-web"},
	}}
	cases := []struct {
		name      string
		stderr    string
		wantCode  ErrorCode
		wantHints bool // true if this code populates available_workspaces
	}{
		{"no_workspace", "Error: no workspace linked. Run 'pad workspace init'.", ErrNoWorkspace, true},
		{"unknown_workspace_does_not_exist", "Error: workspace 'foo' does not exist", ErrUnknownWorkspace, true},
		{"unknown_workspace_explicit", "Error: unknown workspace bar", ErrUnknownWorkspace, true},
		{"auth_required_login", "Error: not authenticated. please run pad auth login.", ErrAuthRequired, false},
		{"auth_required_expired", "Error: expired token", ErrAuthRequired, false},
		{"permission_denied", "Error: permission denied for this resource", ErrPermissionDenied, false},
		{"item_not_found", "Error: item TASK-99 not found", ErrItemNotFound, false},
		{"validation_failed_required", "Error: missing required field 'title'", ErrValidationFailed, false},
		{"validation_failed_enum", "Error: status must be one of: open, in-progress, done", ErrValidationFailed, false},
		{"server_error_unknown", "Error: something went wrong in unexpected ways", ErrServerError, false},
		{"server_error_empty", "", ErrServerError, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runErr := errors.New("exit status 1")
			res := classifyExecError(context.Background(),
				[]string{"item", "list"}, runErr, tc.stderr, lookup)
			if !res.IsError {
				t.Errorf("expected IsError, got success")
			}
			env := decodeEnvelope(t, res)
			if env.Error.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", env.Error.Code, tc.wantCode)
			}
			if tc.wantHints {
				if len(env.Error.AvailableWorkspaces) == 0 {
					t.Errorf("expected available_workspaces populated for %s", tc.wantCode)
				}
			} else {
				if len(env.Error.AvailableWorkspaces) > 0 {
					t.Errorf("did not expect available_workspaces for %s; got %v",
						tc.wantCode, env.Error.AvailableWorkspaces)
				}
			}
		})
	}
}

// TestClassifyExecError_LookupFailureStillReturnsEnvelope ensures
// best-effort listing — when ListWorkspaces fails, we still surface
// the no_workspace error (just with empty available_workspaces).
// Without this guarantee, a transient lookup failure would degrade
// every workspace-context error into a confusing "couldn't even
// produce an error envelope" path.
func TestClassifyExecError_LookupFailureStillReturnsEnvelope(t *testing.T) {
	lookup := &fakeWorkspaceLister{errOut: errors.New("listing exploded")}
	res := classifyExecError(context.Background(),
		[]string{"item", "list"},
		errors.New("exit"),
		"Error: no workspace linked",
		lookup,
	)
	env := decodeEnvelope(t, res)
	if env.Error.Code != ErrNoWorkspace {
		t.Errorf("Code = %q, want no_workspace", env.Error.Code)
	}
	if len(env.Error.AvailableWorkspaces) != 0 {
		t.Errorf("expected empty AvailableWorkspaces on lookup failure; got %v",
			env.Error.AvailableWorkspaces)
	}
}

// TestClassifyHTTPStatus covers the HTTP status → ErrorCode mapping.
// Each documented status maps to the expected code; unmapped statuses
// fall through to server_error.
func TestClassifyHTTPStatus(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantCode ErrorCode
	}{
		{"401_unauthorized", http.StatusUnauthorized, "token invalid", ErrAuthRequired},
		{"403_forbidden", http.StatusForbidden, "role insufficient", ErrPermissionDenied},
		{"404_item", http.StatusNotFound, "item TASK-99 not found", ErrItemNotFound},
		{"404_workspace", http.StatusNotFound, "workspace foo not visible", ErrUnknownWorkspace},
		{"409_conflict", http.StatusConflict, "version mismatch", ErrConflict},
		{"422_validation", http.StatusUnprocessableEntity, "title required", ErrValidationFailed},
		{"400_validation", http.StatusBadRequest, "bad input", ErrValidationFailed},
		{"500_server", http.StatusInternalServerError, "boom", ErrServerError},
		{"503_server", http.StatusServiceUnavailable, "down", ErrServerError},
		{"418_other", http.StatusTeapot, "weird", ErrServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := classifyHTTPStatus(context.Background(),
				"pad item list", tc.status, []byte(tc.body), nil)
			if !res.IsError {
				t.Errorf("expected IsError")
			}
			env := decodeEnvelope(t, res)
			if env.Error.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", env.Error.Code, tc.wantCode)
			}
			// Body fragment should appear somewhere in the envelope —
			// either in Hint (typical) or Message (server_error path).
			combined := env.Error.Hint + " " + env.Error.Message
			if tc.body != "" && !strings.Contains(combined, tc.body) {
				t.Errorf("envelope should preserve body %q somewhere; got message=%q hint=%q",
					tc.body, env.Error.Message, env.Error.Hint)
			}
		})
	}
}

// TestParseWorkspaceListJSON locks the JSON shape ExecDispatcher's
// ListWorkspaces consumes. Emits the same fields `pad workspace list
// --format json` would so the parser stays aligned with the CLI as
// the latter evolves.
func TestParseWorkspaceListJSON(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		body := `[{"slug":"docapp","name":"Pad","default":true},{"slug":"pad-web"}]`
		got, err := parseWorkspaceListJSON(body)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		if got[0].Slug != "docapp" || !got[0].Default || got[0].Name != "Pad" {
			t.Errorf("first hint mismatch: %+v", got[0])
		}
		if got[1].Slug != "pad-web" {
			t.Errorf("second hint slug = %q, want pad-web", got[1].Slug)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		got, err := parseWorkspaceListJSON("")
		if err != nil || got != nil {
			t.Errorf("empty body should return (nil, nil); got (%v, %v)", got, err)
		}
	})

	t.Run("null body", func(t *testing.T) {
		got, err := parseWorkspaceListJSON("null")
		if err != nil || got != nil {
			t.Errorf("null body should return (nil, nil); got (%v, %v)", got, err)
		}
	})

	t.Run("entry without slug skipped", func(t *testing.T) {
		body := `[{"name":"orphan"},{"slug":"valid"}]`
		got, err := parseWorkspaceListJSON(body)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(got) != 1 || got[0].Slug != "valid" {
			t.Errorf("expected [valid], got %+v", got)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		_, err := parseWorkspaceListJSON("not json {")
		if err == nil {
			t.Errorf("expected error on malformed JSON")
		}
	})
}

// TestExtractUnknownWorkspaceSlug covers the small regex helper used
// to pull a slug name out of CLI stderr like "workspace 'foo' does
// not exist". Only QUOTED slugs match — Codex review on PR #357
// caught that bare-word matching captured stop-words like "not" out
// of generic "Workspace not found" responses, which would push agents
// toward retrying with a bogus slug. The regex now requires single or
// double quotes around the slug; bare-word phrasings yield empty
// (the caller's message handles that gracefully).
func TestExtractUnknownWorkspaceSlug(t *testing.T) {
	cases := map[string]string{
		"Error: workspace 'foo' does not exist": "foo",
		"Error: workspace \"bar\" not found":    "bar",
		"workspace 'docapp' is gone":            "docapp",
		// Bare-word phrasings — slug NOT extractable; empty result.
		"unknown workspace baz":        "",
		"workspace docapp not visible": "",
		"Workspace not found":          "",
		// Unrelated input.
		"completely unrelated message": "",
		"":                             "",
	}
	for stderr, want := range cases {
		t.Run(stderr, func(t *testing.T) {
			if got := extractUnknownWorkspaceSlug(stderr); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// decodeEnvelope helper extracts the ErrorEnvelope from an MCP error
// result. Uses textOf (the shared helper in catalog_test.go) to
// retrieve the JSON body from the result's content blocks.
func decodeEnvelope(t *testing.T, res *mcp.CallToolResult) ErrorEnvelope {
	t.Helper()
	body := textOf(res)
	if body == "" {
		t.Fatalf("error result has empty text body")
	}
	var env ErrorEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode envelope: %v\nbody=%s", err, body)
	}
	return env
}
