package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// BUG-3098: an invitation accept refused by the workspace's member cap is
// addressed to the invitee. The classifier must keep its own code and must not
// give it the plan-limit hint, which tells the caller to upgrade a plan they
// do not own.
func TestClassifyHTTPStatus_WorkspaceMemberLimit(t *testing.T) {
	body := []byte(`{"error":{"code":"workspace_member_limit",
		"message":"This workspace has reached its 3-member limit. Ask the workspace owner, Olive Owner, to make room or upgrade their plan, then accept this invitation again.",
		"details":{"feature":"members_per_workspace","limit":3,"current":3}}}`)

	res := classifyHTTPStatus(context.Background(), "workspace join", 403, body, nil)
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("expected ErrorEnvelope, got %T", res.StructuredContent)
	}
	if env.Error.Code != ErrWorkspaceMemberLimit {
		t.Fatalf("code: got %q, want %q (an unlisted code collapses to permission_denied)", env.Error.Code, ErrWorkspaceMemberLimit)
	}
	if strings.Contains(env.Error.Hint, "Upgrade") {
		t.Errorf("hint tells the invitee to upgrade: %q", env.Error.Hint)
	}
	if !strings.Contains(env.Error.Hint, "owner") {
		t.Errorf("hint should point at the owner: %q", env.Error.Hint)
	}
	var d map[string]any
	if err := json.Unmarshal(env.Error.Details, &d); err != nil {
		t.Fatalf("details: %v", err)
	}
	if d["feature"] != "members_per_workspace" || d["limit"] != float64(3) {
		t.Errorf("details = %v", d)
	}

	// Control: the plan-limit code keeps its upgrade hint.
	plan := []byte(`{"error":{"code":"plan_limit_exceeded","message":"x","details":{"feature":"items_per_workspace"}}}`)
	res = classifyHTTPStatus(context.Background(), "item create", 403, plan, nil)
	env = res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code != ErrPlanLimitExceeded || !strings.Contains(env.Error.Hint, "Upgrade") {
		t.Errorf("plan_limit_exceeded lost its code or hint: %+v", env.Error)
	}
}
