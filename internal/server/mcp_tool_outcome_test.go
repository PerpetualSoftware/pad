package server

import (
	"context"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3879: how a failed tool call's structured error code becomes the
// audit row's (result_status, error_kind).
func TestClassifyMCPToolOutcome(t *testing.T) {
	cases := []struct {
		code, reason string
		wantStatus   models.MCPAuditResultStatus
		wantKind     string
	}{
		{"permission_denied", "", models.MCPAuditResultDenied, "permission_denied"},
		{"permission_denied", "token_scope", models.MCPAuditResultDenied, "permission_denied:token_scope"},
		// email_not_verified is its own code (BUG-3864), and a refusal.
		{"email_not_verified", "", models.MCPAuditResultDenied, "email_not_verified"},
		{"auth_required", "", models.MCPAuditResultDenied, "auth_required"},
		{"rate_limited", "", models.MCPAuditResultDenied, "rate_limited"},
		{"validation_failed", "", models.MCPAuditResultError, "validation_failed"},
		{"item_not_found", "", models.MCPAuditResultError, "item_not_found"},
		{"server_error", "", models.MCPAuditResultError, "server_error"},
		// No readable envelope.
		{"", "", models.MCPAuditResultError, "tool_error"},
		// Neither field may write free text into the audit log.
		{"Permission Denied!", "", models.MCPAuditResultError, "tool_error"},
		{"permission_denied", "a reason with spaces", models.MCPAuditResultDenied, "permission_denied"},
	}
	for _, tc := range cases {
		status, kind := classifyMCPToolOutcome(tc.code, tc.reason)
		if status != tc.wantStatus || kind != tc.wantKind {
			t.Errorf("classifyMCPToolOutcome(%q, %q) = (%q, %q), want (%q, %q)",
				tc.code, tc.reason, status, kind, tc.wantStatus, tc.wantKind)
		}
	}
}

func TestRecordMCPToolOutcome_RefusalOutranksError(t *testing.T) {
	for _, order := range [][2]string{{"validation_failed", "permission_denied"}, {"permission_denied", "validation_failed"}} {
		ctx, sink := WithMCPToolOutcomeSink(context.Background())
		RecordMCPToolOutcome(ctx, order[0], "")
		RecordMCPToolOutcome(ctx, order[1], "")
		status, kind, ok := sink.Outcome()
		if !ok || status != models.MCPAuditResultDenied || kind != "permission_denied" {
			t.Errorf("after %v: (%q, %q, %v), want the refusal", order, status, kind, ok)
		}
	}
	// Two plain errors: the first stands.
	ctx, sink := WithMCPToolOutcomeSink(context.Background())
	RecordMCPToolOutcome(ctx, "item_not_found", "")
	RecordMCPToolOutcome(ctx, "validation_failed", "")
	if _, kind, _ := sink.Outcome(); kind != "item_not_found" {
		t.Errorf("two errors kept %q, want the first", kind)
	}
}

func TestRecordMCPToolOutcome_NoSinkIsANoOp(t *testing.T) {
	RecordMCPToolOutcome(context.Background(), "permission_denied", "") // must not panic
	_, sink := WithMCPToolOutcomeSink(context.Background())
	if _, _, ok := sink.Outcome(); ok {
		t.Fatal("a fresh sink reports an outcome")
	}
}
