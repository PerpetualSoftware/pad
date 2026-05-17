package mcp

// IDEA-1494 R2 — MCP-level coverage for the open-children code/details
// pass-through, both HTTP and stdio.
//
// The contract:
//
//   - HTTP path: when the upstream PATCH returns 409 with code=
//     "open_children" and a populated details body, the dispatcher
//     surfaces ErrOpenChildren (not the generic ErrConflict) and
//     preserves the details RawMessage verbatim. Agents branch on
//     the code, then re-parse details against the known shape.
//   - stdio path: when the CLI's stderr carries a `pad-error: {json}`
//     marker line, classifyExecError detects it, lifts the structured
//     payload, and surfaces it in the same shape — independent of
//     transport.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

func TestClassifyHTTPStatus_OpenChildrenPreservesCodeAndDetails(t *testing.T) {
	body := []byte(`{
		"error": {
			"code": "open_children",
			"message": "cannot mark PLAN-5 completed: 2 open children still in a non-terminal state. Pass --force to override.",
			"details": {
				"open_children": [
					{"ref":"TASK-7","title":"a","status":"open","collection_slug":"tasks"},
					{"ref":"TASK-8","title":"b","status":"open","collection_slug":"tasks"}
				],
				"hidden_blocker_count": 0,
				"done_field": "status",
				"attempted_value": "completed"
			}
		}
	}`)

	res := classifyHTTPStatus(context.Background(), "item update", 409, body, nil)
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("expected ErrorEnvelope, got %T", res.StructuredContent)
	}
	if env.Error.Code != ErrOpenChildren {
		t.Fatalf("code: got %q, want %q", env.Error.Code, ErrOpenChildren)
	}
	if env.Error.Message == "" {
		t.Errorf("message should be preserved from upstream, got empty")
	}
	if len(env.Error.Details) == 0 {
		t.Fatal("details should be preserved as raw JSON, got empty")
	}

	// Round-trip the details into the shared CLI struct (the same one
	// MCP-driven agents would unmarshal against). Confirms the shape
	// survives the HTTP-classifier transit unchanged.
	var got cli.OpenChildrenDetails
	if err := json.Unmarshal(env.Error.Details, &got); err != nil {
		t.Fatalf("details should round-trip into OpenChildrenDetails: %v", err)
	}
	if len(got.OpenChildren) != 2 {
		t.Errorf("open_children len: got %d, want 2", len(got.OpenChildren))
	}
	if got.AttemptedValue != "completed" {
		t.Errorf("attempted_value: got %q, want completed", got.AttemptedValue)
	}
	if got.DoneField != "status" {
		t.Errorf("done_field: got %q, want status", got.DoneField)
	}
}

// TestClassifyHTTPStatus_ConflictWithoutCodeFallsToErrConflict pins
// the inverse: a 409 WITHOUT an upstream code (or with code="conflict"
// explicitly) still collapses to ErrConflict so we don't break the
// existing classifier contract for ordinary version-mismatch 409s.
func TestClassifyHTTPStatus_ConflictWithoutCodeFallsToErrConflict(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no code at all", `{"error":{"message":"version mismatch"}}`},
		{"explicit conflict code", `{"error":{"code":"conflict","message":"version mismatch"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := classifyHTTPStatus(context.Background(), "item update", 409, []byte(tc.body), nil)
			env := res.StructuredContent.(ErrorEnvelope)
			if env.Error.Code != ErrConflict {
				t.Errorf("code: got %q, want %q", env.Error.Code, ErrConflict)
			}
			if len(env.Error.Details) != 0 {
				t.Errorf("details should be empty for generic 409, got %s", string(env.Error.Details))
			}
		})
	}
}

func TestClassifyExecError_OpenChildrenMarkerLiftsStructuredPayload(t *testing.T) {
	stderr := `Error: connecting to backend
pad-error: {"error":{"code":"open_children","message":"cannot mark PLAN-5 completed: 1 open child still in a non-terminal state. Pass --force to override.","details":{"open_children":[{"ref":"TASK-7","title":"x","status":"open","collection_slug":"tasks"}],"hidden_blocker_count":0,"done_field":"status","attempted_value":"completed"}}}
cannot mark PLAN-5 completed: 1 open child still in a non-terminal state. Pass --force to override.
  TASK-7 — x (status=open)
Pass --force to override.
`
	res := classifyExecError(context.Background(),
		[]string{"item", "update"},
		errors.New("exit status 1"),
		stderr,
		nil)
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("expected ErrorEnvelope, got %T", res.StructuredContent)
	}
	if env.Error.Code != ErrOpenChildren {
		t.Fatalf("code: got %q, want %q", env.Error.Code, ErrOpenChildren)
	}
	var details cli.OpenChildrenDetails
	if err := json.Unmarshal(env.Error.Details, &details); err != nil {
		t.Fatalf("details did not round-trip: %v", err)
	}
	if len(details.OpenChildren) != 1 || details.OpenChildren[0].Ref != "TASK-7" {
		t.Errorf("open_children mismatch: %+v", details.OpenChildren)
	}
}

// TestClassifyExecError_NoMarkerFallsThrough confirms stderr WITHOUT
// the marker classifies via the existing regex matchers (validation /
// auth / item-not-found / generic) — i.e. the marker detection is
// purely additive and doesn't break the pre-IDEA-1494 stderr-classify
// contracts.
func TestClassifyExecError_NoMarkerFallsThrough(t *testing.T) {
	stderr := "Error: invalid status value\n"
	res := classifyExecError(context.Background(),
		[]string{"item", "update"},
		errors.New("exit status 1"),
		stderr,
		nil)
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code == ErrOpenChildren {
		t.Errorf("plain validation stderr must not be classified as open_children")
	}
}
