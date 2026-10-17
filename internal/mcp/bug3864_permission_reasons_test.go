package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// BUG-3864, the classifier half: the same 403 body reaches the same envelope
// on both transports. Remote reads the body; stdio reads the marker the CLI
// root writes from the same body. cmd/pad's end-to-end test drives the rows a
// fixture can reach; these are the ones it cannot (an OAuth allow-list, an
// older server, a newer server's reason).
func TestBUG3864_A403ReadsTheSameOnBothTransports(t *testing.T) {
	for _, tc := range []struct {
		name, code, message, reason string
		wantCode                    ErrorCode
		wantReason                  string
		wantHint                    string
	}{
		{"workspace allow-list", "permission_denied", "Token is not authorized for this workspace", "workspace_not_authorized",
			ErrPermissionDenied, "workspace_not_authorized", PermissionWorkspaceNotAuthorizedHint},
		{"role", "forbidden", "Insufficient permissions", "role", ErrPermissionDenied, "role", PermissionRoleHint},
		// An older server says no reason. The hint must not guess one.
		{"no reason", "forbidden", "Only the comment author can edit it.", "",
			ErrPermissionDenied, "", "The server refused this operation: Only the comment author can edit it."},
		// A newer server's reason this build has no words for: dropped, so
		// details.reason stays a closed vocabulary, and the message speaks.
		{"unknown reason", "forbidden", "Refused by a gate from the future", "future_gate",
			ErrPermissionDenied, "", "The server refused this operation: Refused by a gate from the future."},
		{"unverified email", "email_not_verified", "Verify your email address to perform this action.", "",
			ErrEmailNotVerified, "", EmailNotVerifiedHint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &cli.APIError{Code: tc.code, Message: tc.message, Status: http.StatusForbidden}
			if tc.reason != "" {
				api.Details, _ = json.Marshal(map[string]string{"reason": tc.reason})
			}
			body, _ := json.Marshal(map[string]any{"error": api})

			remote := decodeEnvelope(t, classifyHTTPStatus(context.Background(), "item create", http.StatusForbidden, body, nil))

			var stderr bytes.Buffer
			stderr.WriteString("Error: " + tc.message + "\n")
			if _, ok := cli.IsPermissionDenied(api); !ok {
				t.Fatalf("the CLI root would write no marker for code %q", tc.code)
			}
			cli.WritePermissionDeniedError(&stderr, api)
			lifted := extractStructuredCLIError(stderr.String())
			if lifted == nil {
				t.Fatalf("stdio did not lift the marker: %s", stderr.String())
			}
			stdio := decodeEnvelope(t, lifted)

			if remote.Error.Hint != stdio.Error.Hint || remote.Error.Code != stdio.Error.Code || string(remote.Error.Details) != string(stdio.Error.Details) {
				t.Errorf("the transports differ:\n remote %+v\n stdio  %+v", remote.Error, stdio.Error)
			}
			for door, env := range map[string]ErrorEnvelope{"remote": remote, "stdio": stdio} {
				if env.Error.Code != tc.wantCode || reasonOf(t, env) != tc.wantReason || !strings.HasPrefix(env.Error.Hint, tc.wantHint) {
					t.Errorf("%s: code=%q reason=%q hint=%q, want %q %q hint starting %q",
						door, env.Error.Code, reasonOf(t, env), env.Error.Hint, tc.wantCode, tc.wantReason, tc.wantHint)
				}
				if tc.wantReason != "role" && strings.Contains(strings.ToLower(env.Error.Hint), "role") {
					t.Errorf("%s: hint blames a role: %q", door, env.Error.Hint)
				}
			}
		})
	}
}

// Other 403 codes keep their own writers. A permission_denied marker from the
// root would come after theirs and, last marker winning, replace it.
func TestBUG3864_TheRootWriterLeavesOtherRefusalsAlone(t *testing.T) {
	for _, code := range []string{"plan_limit_exceeded", "workspace_member_limit", "insufficient_scope", "account_disabled"} {
		if _, ok := cli.IsPermissionDenied(&cli.APIError{Code: code, Status: http.StatusForbidden}); ok {
			t.Errorf("IsPermissionDenied claims %q", code)
		}
	}
	if _, ok := cli.IsPermissionDenied(&cli.APIError{Code: "forbidden", Status: http.StatusBadRequest}); ok {
		t.Error("IsPermissionDenied claims a non-403")
	}
}
