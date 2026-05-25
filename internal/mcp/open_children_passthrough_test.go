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
pad-structured-error/v1: {"error":{"code":"open_children","message":"cannot mark PLAN-5 completed: 1 open child still in a non-terminal state. Pass --force to override.","details":{"open_children":[{"ref":"TASK-7","title":"x","status":"open","collection_slug":"tasks"}],"hidden_blocker_count":0,"done_field":"status","attempted_value":"completed"}}}
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

// TestClassifyExecError_UnknownStructuredCodeFallsThrough covers
// Codex round-3 P3 marker hardening: even with a well-formed
// pad-structured-error/v1 marker, a code that's NOT in the allow-list
// must not be surfaced — fall back to regex classification instead.
// Prevents a CLI bug or third-party tool from smuggling an
// unsanitized code past the MCP boundary.
func TestClassifyExecError_UnknownStructuredCodeFallsThrough(t *testing.T) {
	stderr := `pad-structured-error/v1: {"error":{"code":"made_up_code","message":"x"}}
Error: invalid status value
`
	res := classifyExecError(context.Background(),
		[]string{"item", "update"},
		errors.New("exit status 1"),
		stderr,
		nil)
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code == ErrorCode("made_up_code") {
		t.Errorf("unknown structured code must not be surfaced; got %q", env.Error.Code)
	}
}

// TestClassifyExecError_OldMarkerVersionIgnored confirms that an
// older (or unrecognized future) marker version is ignored rather
// than parsed. Validates the "version token is parsed, not just
// prefix-matched" property — agents on the new mcp build won't be
// confused by stderr from a stale CLI binary that still uses the
// pre-round-3 unversioned `pad-error:` shape.
func TestClassifyExecError_OldMarkerVersionIgnored(t *testing.T) {
	stderr := `pad-error: {"error":{"code":"open_children","message":"x"}}
Error: invalid status value
`
	res := classifyExecError(context.Background(),
		[]string{"item", "update"},
		errors.New("exit status 1"),
		stderr,
		nil)
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code == ErrOpenChildren {
		t.Errorf("pre-v1 unversioned marker must not be lifted; got %q", env.Error.Code)
	}
}

// TestClassifyExecError_MarkerEmbeddedMidLineIgnored ensures a marker
// substring inside a quoted log message can't impersonate a real
// structured error. The classifier requires the marker at the line's
// start (after whitespace trim) — embedded variants fall through.
func TestClassifyExecError_MarkerEmbeddedMidLineIgnored(t *testing.T) {
	stderr := `Error: backend logged "pad-structured-error/v1: {\"error\":{\"code\":\"open_children\"}}"
`
	res := classifyExecError(context.Background(),
		[]string{"item", "update"},
		errors.New("exit status 1"),
		stderr,
		nil)
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code == ErrOpenChildren {
		t.Errorf("embedded marker must not be lifted; got %q", env.Error.Code)
	}
}

// TestClassifyExecError_LastMarkerWins confirms multiple markers
// resolve to the LAST one — a malicious earlier line can't pre-empt
// the CLI's actual final classification.
func TestClassifyExecError_LastMarkerWins(t *testing.T) {
	stderr := `pad-structured-error/v1: {"error":{"code":"made_up_code","message":"first"}}
pad-structured-error/v1: {"error":{"code":"open_children","message":"second","details":{"open_children":[]}}}
`
	res := classifyExecError(context.Background(),
		[]string{"item", "update"},
		errors.New("exit status 1"),
		stderr,
		nil)
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code != ErrOpenChildren {
		t.Errorf("expected the later marker to win; got %q (message=%q)",
			env.Error.Code, env.Error.Message)
	}
}

// TestClassifyHTTPStatus_UnknownConflictCodeFallsBackToErrConflict
// covers Codex round-4 P2: HTTP and stdio must agree on the closed
// set of structured codes they surface. Pre-fix the HTTP path
// forwarded ANY non-"conflict" code; stdio whitelisted only
// open_children. The two transports diverged on what an agent saw
// from an upstream that emitted `code=some_future_code`.
//
// Post-fix: both consult allowedStructuredErrorCodes. An upstream
// 409 with a code NOT in the whitelist collapses to generic
// ErrConflict on the HTTP path, mirroring what the stdio path does
// for an unknown-code structured marker.
func TestClassifyHTTPStatus_UnknownConflictCodeFallsBackToErrConflict(t *testing.T) {
	body := []byte(`{
		"error": {
			"code": "some_future_code",
			"message": "x",
			"details": {"anything":1}
		}
	}`)
	res := classifyHTTPStatus(context.Background(), "item update", 409, body, nil)
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code != ErrConflict {
		t.Errorf("unknown upstream code must collapse to ErrConflict on HTTP path; got %q", env.Error.Code)
	}
	if len(env.Error.Details) != 0 {
		t.Errorf("details from un-whitelisted code must not leak; got %s", string(env.Error.Details))
	}
}

// TestStructuredErrorCodeParityAcrossTransports asserts that the same
// "unknown code" rejection produces the same envelope shape from
// both transports — ie HTTP doesn't widen the enum behind stdio's
// back. Round-4 P2.
func TestStructuredErrorCodeParityAcrossTransports(t *testing.T) {
	httpRes := classifyHTTPStatus(context.Background(), "item update", 409,
		[]byte(`{"error":{"code":"made_up_code","message":"x"}}`), nil)
	httpEnv := httpRes.StructuredContent.(ErrorEnvelope)

	stdioRes := classifyExecError(context.Background(),
		[]string{"item", "update"},
		errors.New("exit status 1"),
		`pad-structured-error/v1: {"error":{"code":"made_up_code","message":"x"}}
`, nil)
	stdioEnv := stdioRes.StructuredContent.(ErrorEnvelope)

	if httpEnv.Error.Code == ErrorCode("made_up_code") {
		t.Errorf("HTTP path leaked unknown code: %q", httpEnv.Error.Code)
	}
	if stdioEnv.Error.Code == ErrorCode("made_up_code") {
		t.Errorf("stdio path leaked unknown code: %q", stdioEnv.Error.Code)
	}
	// Both transports must agree: neither surfaces the unknown code.
	// They CAN classify the fallback differently (HTTP → ErrConflict
	// for 409, stdio → ErrServerError for an unknown stderr line)
	// because the upstream signals are genuinely different — the
	// parity contract is "no transport widens the enum beyond the
	// whitelist," not "both transports produce identical envelopes."
}

// ─── TASK-788: plan_limit_exceeded — HTTP and stdio pass-through ──────────────

// TestClassifyHTTPStatus_PlanLimitPreservesCodeAndDetails exercises the HTTP
// MCP path: a 403 with code=plan_limit_exceeded must surface ErrPlanLimitExceeded
// (not the generic ErrPermissionDenied) and preserve the details blob.
func TestClassifyHTTPStatus_PlanLimitPreservesCodeAndDetails(t *testing.T) {
	body := []byte(`{
		"error": {
			"code": "plan_limit_exceeded",
			"message": "You've reached the 3-member limit on the free plan.",
			"details": {
				"feature": "members_per_workspace",
				"limit": 3,
				"current": 3,
				"plan": "free",
				"upgrade_url": "/console/billing"
			}
		}
	}`)

	res := classifyHTTPStatus(context.Background(), "workspace invite", 403, body, nil)
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("expected ErrorEnvelope, got %T", res.StructuredContent)
	}
	if env.Error.Code != ErrPlanLimitExceeded {
		t.Fatalf("code: got %q, want %q", env.Error.Code, ErrPlanLimitExceeded)
	}
	if env.Error.Message == "" {
		t.Errorf("message should be preserved from upstream, got empty")
	}
	if len(env.Error.Details) == 0 {
		t.Fatal("details should be preserved as raw JSON, got empty")
	}

	// Round-trip the details to confirm the shape survives the HTTP-classifier
	// transit unchanged.
	var got cli.PlanLimitDetails
	if err := json.Unmarshal(env.Error.Details, &got); err != nil {
		t.Fatalf("details should round-trip into PlanLimitDetails: %v", err)
	}
	if got.Feature != "members_per_workspace" {
		t.Errorf("feature: got %q, want members_per_workspace", got.Feature)
	}
	if got.Limit != 3 {
		t.Errorf("limit: got %d, want 3", got.Limit)
	}
	if got.Plan != "free" {
		t.Errorf("plan: got %q, want free", got.Plan)
	}
	if got.UpgradeURL != "/console/billing" {
		t.Errorf("upgrade_url: got %q, want /console/billing", got.UpgradeURL)
	}
}

// TestClassifyHTTPStatus_Generic403FallsToPermissionDenied confirms the inverse:
// a plain 403 without a plan_limit_exceeded code still collapses to
// ErrPermissionDenied so we don't break the existing permission-check contract.
func TestClassifyHTTPStatus_Generic403FallsToPermissionDenied(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no error code", `{"error":{"message":"not a member of this workspace"}}`},
		{"explicit permission_denied code", `{"error":{"code":"permission_denied","message":"not a member"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := classifyHTTPStatus(context.Background(), "workspace read", 403, []byte(tc.body), nil)
			env := res.StructuredContent.(ErrorEnvelope)
			if env.Error.Code != ErrPermissionDenied {
				t.Errorf("code: got %q, want %q", env.Error.Code, ErrPermissionDenied)
			}
		})
	}
}

// TestClassifyExecError_PlanLimitMarkerLiftsStructuredPayload exercises the
// CLI-stdio MCP path (TASK-788 Finding A): when cli.WritePlanLimitError writes
// the structured marker to stderr, classifyExecError must surface
// ErrPlanLimitExceeded with the correct details — NOT a generic server_error.
func TestClassifyExecError_PlanLimitMarkerLiftsStructuredPayload(t *testing.T) {
	stderr := `Error: connecting to backend
pad-structured-error/v1: {"error":{"code":"plan_limit_exceeded","message":"You've reached the 3-member limit on the free plan.","details":{"feature":"members_per_workspace","limit":3,"current":3,"plan":"free","upgrade_url":"/console/billing"}}}
item creation blocked: plan limit reached
`
	res := classifyExecError(context.Background(),
		[]string{"item", "create"},
		errors.New("exit status 1"),
		stderr,
		nil)
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("expected ErrorEnvelope, got %T", res.StructuredContent)
	}
	if env.Error.Code != ErrPlanLimitExceeded {
		t.Fatalf("code: got %q, want %q", env.Error.Code, ErrPlanLimitExceeded)
	}
	if env.Error.Message == "" {
		t.Errorf("message should be non-empty")
	}

	var got cli.PlanLimitDetails
	if err := json.Unmarshal(env.Error.Details, &got); err != nil {
		t.Fatalf("details should round-trip into PlanLimitDetails: %v", err)
	}
	if got.Feature != "members_per_workspace" {
		t.Errorf("feature: got %q, want members_per_workspace", got.Feature)
	}
	if got.Limit != 3 {
		t.Errorf("limit: got %d, want 3", got.Limit)
	}
}

// TestClassifyExecError_PlanLimitWithoutMarkerFallsThrough confirms that a plain
// "plan limit" text in stderr (no structured marker) does NOT produce
// ErrPlanLimitExceeded — it falls through to the regex classifiers so old CLI
// binaries don't accidentally get the new code.
func TestClassifyExecError_PlanLimitWithoutMarkerFallsThrough(t *testing.T) {
	stderr := "Error: plan limit reached\n"
	res := classifyExecError(context.Background(),
		[]string{"item", "create"},
		errors.New("exit status 1"),
		stderr,
		nil)
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code == ErrPlanLimitExceeded {
		t.Errorf("plain text stderr must not be classified as plan_limit_exceeded")
	}
}

// TestClassifyExecError_PlanLimitWorkspaceCreate exercises the workspace-create
// CLI-stdio path (TASK-788 codex R2 Finding 2): WritePlanLimitError on the
// workspace-create command must round-trip through classifyExecError with
// ErrPlanLimitExceeded and populated details.
func TestClassifyExecError_PlanLimitWorkspaceCreate(t *testing.T) {
	// Simulates stderr produced by WritePlanLimitError when workspace creation
	// hits the workspaces cap for a free-tier user.
	stderr := `Error: connecting to backend
pad-structured-error/v1: {"error":{"code":"plan_limit_exceeded","message":"You've reached the 3-workspace limit on the free plan.","details":{"feature":"workspaces","limit":3,"current":3,"plan":"free","upgrade_url":"/console/billing"}}}
workspace creation blocked: plan limit reached
`
	res := classifyExecError(context.Background(),
		[]string{"workspace", "init"},
		errors.New("exit status 1"),
		stderr,
		nil)
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("expected ErrorEnvelope, got %T", res.StructuredContent)
	}
	if env.Error.Code != ErrPlanLimitExceeded {
		t.Fatalf("code: got %q, want %q", env.Error.Code, ErrPlanLimitExceeded)
	}
	if env.Error.Message == "" {
		t.Errorf("message should be non-empty")
	}

	var got cli.PlanLimitDetails
	if err := json.Unmarshal(env.Error.Details, &got); err != nil {
		t.Fatalf("details should round-trip into PlanLimitDetails: %v", err)
	}
	if got.Feature != "workspaces" {
		t.Errorf("feature: got %q, want workspaces", got.Feature)
	}
	if got.Limit != 3 {
		t.Errorf("limit: got %d, want 3", got.Limit)
	}
	if got.UpgradeURL != "/console/billing" {
		t.Errorf("upgrade_url: got %q, want /console/billing", got.UpgradeURL)
	}
}
