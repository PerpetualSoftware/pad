package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-1933 DR-4 — the remote MCP write perimeter. /mcp mounts outside
// the /api/v1 middleware stack, so RequireVerifiedEmail can't cover it as
// a chi middleware; the dispatcher carries its own gate (the
// RequireVerifiedEmail hook, wired in cmd/pad/main.go to
// `srv.IsCloud() && user != nil && !user.IsEmailVerified()`). These tests
// exercise the gate directly through the dispatcher, mirroring the
// scope-enforcement tests next door.

// veUnverifiedGate simulates the production hook for a cloud instance:
// block iff the user's email is unverified.
func veUnverifiedGate(user *models.User) bool {
	return user != nil && !user.IsEmailVerified()
}

func TestHTTPHandlerDispatcher_VerifiedEmail_BlocksUnverifiedWrite(t *testing.T) {
	user := &models.User{ID: "u1", Name: "Unv", Email: "unv@example.com"} // EmailVerifiedAt == "" → unverified
	rec := &recordingHandler{t: t}

	d := &HTTPHandlerDispatcher{
		Handler:              rec,
		UserResolver:         fixedUserResolver(user),
		RequireVerifiedEmail: veUnverifiedGate,
	}

	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":  "docapp",
		"collection": "tasks",
		"title":      "Should Be Blocked",
	})

	res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError for unverified write; got success: %#v", res)
	}
	if rec.requestCount != 0 {
		t.Errorf("handler must NOT be invoked when the verified-email gate fires; got %d calls", rec.requestCount)
	}
	var msg string
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(mcp.TextContent); ok {
			msg = tc.Text
		}
	}
	// Surfaced as permission_denied (closed-set code) with the
	// email_not_verified marker in the message.
	if !strings.Contains(msg, "permission_denied") {
		t.Errorf("error envelope missing permission_denied marker: %q", msg)
	}
	if !strings.Contains(msg, "email_not_verified") {
		t.Errorf("error envelope missing email_not_verified marker: %q", msg)
	}
}

func TestHTTPHandlerDispatcher_VerifiedEmail_AllowsVerifiedWrite(t *testing.T) {
	user := &models.User{ID: "u2", Name: "Ver", Email: "ver@example.com", EmailVerifiedAt: "2026-01-01T00:00:00Z"}
	rec := &recordingHandler{t: t}

	d := &HTTPHandlerDispatcher{
		Handler:              rec,
		UserResolver:         fixedUserResolver(user),
		RequireVerifiedEmail: veUnverifiedGate,
	}

	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":  "docapp",
		"collection": "tasks",
		"title":      "Allowed",
	})

	res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.IsError {
		t.Fatalf("verified write should succeed; got error: %#v", res)
	}
	if rec.requestCount != 1 || rec.gotMethod != http.MethodPost {
		t.Errorf("expected one POST to reach the handler; got count=%d method=%q", rec.requestCount, rec.gotMethod)
	}
}

func TestHTTPHandlerDispatcher_VerifiedEmail_AllowsUnverifiedRead(t *testing.T) {
	user := &models.User{ID: "u3", Name: "Unv", Email: "unv@example.com"} // unverified
	rec := &recordingHandler{t: t, wantStatus: http.StatusOK, respBody: `{"items":[]}`}

	d := &HTTPHandlerDispatcher{
		Handler:              rec,
		UserResolver:         fixedUserResolver(user),
		RequireVerifiedEmail: veUnverifiedGate,
	}

	ctx := WithDispatchInput(context.Background(), map[string]any{"workspace": "docapp"})

	// item list → GET. Reads stay open even for unverified users.
	res, err := d.Dispatch(ctx, []string{"item", "list"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.IsError {
		t.Fatalf("unverified read should succeed; got error: %#v", res)
	}
	if rec.gotMethod != http.MethodGet {
		t.Errorf("expected GET, got %q", rec.gotMethod)
	}
}

func TestHTTPHandlerDispatcher_VerifiedEmail_NilHookIsNoOp(t *testing.T) {
	user := &models.User{ID: "u4", Name: "Unv", Email: "unv@example.com"} // unverified
	rec := &recordingHandler{t: t}

	// No RequireVerifiedEmail hook wired — the self-host / stdio / test
	// default. An unverified user's write passes straight through.
	d := &HTTPHandlerDispatcher{
		Handler:      rec,
		UserResolver: fixedUserResolver(user),
	}

	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":  "docapp",
		"collection": "tasks",
		"title":      "No Gate",
	})

	res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.IsError {
		t.Fatalf("nil-hook write should succeed; got error: %#v", res)
	}
	if rec.requestCount != 1 {
		t.Errorf("expected the write to reach the handler with no gate; got %d calls", rec.requestCount)
	}
}
