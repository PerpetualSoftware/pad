package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/models"
	padserver "github.com/PerpetualSoftware/pad/internal/server"
)

// BUG-3879: internal/server classifies a tool failure by code string, since
// it cannot import this package. These are the codes it must read as a
// refusal; a renamed constant would otherwise turn refusals into errors.
func TestToolOutcome_RefusalCodesAreDenied(t *testing.T) {
	for _, code := range []ErrorCode{ErrPermissionDenied, ErrAuthRequired, ErrRateLimited} {
		ctx, sink := padserver.WithMCPToolOutcomeSink(context.Background())
		padserver.RecordMCPToolOutcome(ctx, string(code), "")
		if status, kind, _ := sink.Outcome(); status != models.MCPAuditResultDenied || kind != string(code) {
			t.Errorf("%s audited as (%q, %q), want denied", code, status, kind)
		}
	}
}

func TestToolErrorCode(t *testing.T) {
	withReason := NewErrorResult(ErrorPayload{Code: ErrPermissionDenied, Message: "no", Details: json.RawMessage(`{"reason":"token_scope"}`)})
	listDetails := NewErrorResult(ErrorPayload{Code: ErrOpenChildren, Message: "open", Details: json.RawMessage(`[{"ref":"TASK-1"}]`)})
	textOnly := NewErrorResult(ErrorPayload{Code: ErrConflict, Message: "c"})
	textOnly.StructuredContent = nil
	cases := []struct {
		name              string
		res               *mcp.CallToolResult
		wantCode, wantWhy string
	}{
		{"structured, with reason", withReason, "permission_denied", "token_scope"},
		{"details that is not an object keeps the code", listDetails, "open_children", ""},
		{"text rendering only", textOnly, "conflict", ""},
		{"plain text error", errorResultf("boom"), "", ""},
	}
	for _, tc := range cases {
		code, why := toolErrorCode(tc.res)
		if code != tc.wantCode || why != tc.wantWhy {
			t.Errorf("%s: (%q, %q), want (%q, %q)", tc.name, code, why, tc.wantCode, tc.wantWhy)
		}
	}
}

// The middleware must be on every server NewServer builds, and outside
// WithRecovery, so a handler that panics or returns a Go error is still
// audited as a failure. Driven through the server's own message handling,
// as both transports do.
func TestToolOutcome_NewServerRecordsEveryEnding(t *testing.T) {
	srv := NewServer(Options{Version: "test"})
	srv.MCP().AddTool(mcp.NewTool("refuse"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return NewErrorResult(ErrorPayload{Code: ErrPermissionDenied, Message: "no"}), nil
	})
	srv.MCP().AddTool(mcp.NewTool("succeed"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return textResult("fine"), nil
	})
	srv.MCP().AddTool(mcp.NewTool("go_error"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, errors.New("handler failed")
	})
	srv.MCP().AddTool(mcp.NewTool("panics"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		panic("handler panicked")
	})
	cases := []struct {
		tool       string
		wantFailed bool
		wantStatus models.MCPAuditResultStatus
		wantKind   string
	}{
		{"refuse", true, models.MCPAuditResultDenied, "permission_denied"},
		{"succeed", false, "", ""},
		{"go_error", true, models.MCPAuditResultError, "server_error"},
		{"panics", true, models.MCPAuditResultError, "server_error"},
		// Refused by mcp-go before any middleware runs (hunter r1 M1).
		{"no_such_tool", true, models.MCPAuditResultError, "tool_not_found"},
	}
	for _, tc := range cases {
		ctx, sink := padserver.WithMCPToolOutcomeSink(context.Background())
		srv.MCP().HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tc.tool+`","arguments":{}}}`))
		status, kind, failed := sink.Outcome()
		if failed != tc.wantFailed || status != tc.wantStatus || kind != tc.wantKind {
			t.Errorf("%s: (%q, %q, failed=%v), want (%q, %q, failed=%v)", tc.tool, status, kind, failed, tc.wantStatus, tc.wantKind, tc.wantFailed)
		}
	}
}

// Lead ruling R2 on BUG-3879: the OnError hook is not scoped to tools/call,
// so a failed tools/list writes error, not ok. A cursor that is not one
// mcp-go issued fails the listing with INVALID_PARAMS.
func TestToolOutcome_AFailedToolsListIsAnError(t *testing.T) {
	srv := NewServer(Options{Version: "test"})
	srv.MCP().AddTool(mcp.NewTool("succeed"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return textResult("fine"), nil
	})
	ctx, sink := padserver.WithMCPToolOutcomeSink(context.Background())
	resp := srv.MCP().HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":"not-a-cursor-mcp-go-issued"}}`))
	raw, _ := json.Marshal(resp)
	if !json.Valid(raw) || !bytes.Contains(raw, []byte(`"error"`)) {
		t.Fatalf("precondition: tools/list did not fail: %s", raw)
	}
	if status, kind, failed := sink.Outcome(); !failed || status != models.MCPAuditResultError || kind != "jsonrpc_error" {
		t.Fatalf("failed tools/list audited as (%q, %q, failed=%v), want (error, jsonrpc_error)", status, kind, failed)
	}
	// And a tools/list that succeeds stays ok.
	ctx, sink = padserver.WithMCPToolOutcomeSink(context.Background())
	srv.MCP().HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	if _, _, failed := sink.Outcome(); failed {
		t.Fatal("a successful tools/list recorded a failure")
	}
}
