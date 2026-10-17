package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	padserver "github.com/PerpetualSoftware/pad/internal/server"
)

// recordToolOutcome is the tool-handler middleware that tells the /mcp
// audit log how a tool call ended (BUG-3879). A failed call answers the
// POST with 200, so the audit middleware cannot read the failure from the
// response status; this records the result's structured error code into
// the sink that middleware put on ctx. It sits at the server, not at the
// dispatch sites, because refusals are produced in many places (the
// pre-dispatch scope and verified-email gates in buildAuthedRequest, an
// inner route's 403, the ChatGPT scope challenge) and every one of them
// leaves through a tool result. Over stdio there is no sink and this does
// nothing.
func recordToolOutcome(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := next(ctx, req)
		switch {
		case err != nil:
			padserver.RecordMCPToolOutcome(ctx, string(ErrServerError), "")
		case res != nil && res.IsError:
			code, reason := toolErrorCode(res)
			padserver.RecordMCPToolOutcome(ctx, code, reason)
		}
		return res, err
	}
}

// recordJSONRPCError is the OnError hook that records a JSON-RPC error
// response (BUG-3879 hunter r1 M1). mcp-go refuses an unknown tool, and
// answers a missing resource or prompt, before the tool-handler middleware
// is applied, with a JSON-RPC error inside a 200. A tool handler's own Go
// error reaches this hook too, after recordToolOutcome has recorded it;
// the sink keeps the first, so the row is unchanged. None of these is a
// refusal, so every code here audits as error. It fires for every method,
// not only tools/call: a failed initialize or tools/list did not succeed
// either (lead ruling R2 on BUG-3879). It does NOT see a message mcp-go
// rejects before dispatching it to a method (an unknown method, a bad
// jsonrpc version, a failed request-init): those still audit as ok, and no
// tool runs on them (hunter r2 L1).
func recordJSONRPCError(ctx context.Context, _ any, _ mcp.MCPMethod, _ any, err error) {
	code := "jsonrpc_error"
	switch {
	case errors.Is(err, server.ErrToolNotFound):
		code = "tool_not_found"
	case errors.Is(err, server.ErrResourceNotFound):
		code = "resource_not_found"
	case errors.Is(err, server.ErrPromptNotFound):
		code = "prompt_not_found"
	}
	padserver.RecordMCPToolOutcome(ctx, code, "")
}

// toolErrorCode reads the code and details.reason of a failed result's
// error envelope: the structured content when it has one, else the text
// rendering, which NewErrorResult makes the same JSON. A result with no
// readable envelope (errorResultf's plain text) returns "", which the
// audit records as a tool_error.
func toolErrorCode(res *mcp.CallToolResult) (code, reason string) {
	if res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			if code, reason, ok := envelopeCode(b); ok {
				return code, reason
			}
		}
	}
	for _, c := range res.Content {
		var text string
		switch t := c.(type) {
		case mcp.TextContent:
			text = t.Text
		case *mcp.TextContent:
			text = t.Text
		}
		if code, reason, ok := envelopeCode([]byte(text)); ok {
			return code, reason
		}
	}
	return "", ""
}

// envelopeCode decodes {"error":{"code":...,"details":{"reason":...}}}.
// details is code-specific (open_children carries a list), so it is decoded
// apart: a details that is not an object loses the reason, never the code.
func envelopeCode(b []byte) (code, reason string, ok bool) {
	var env struct {
		Error struct {
			Code    string          `json:"code"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &env) != nil || env.Error.Code == "" {
		return "", "", false
	}
	var d struct {
		Reason string `json:"reason"`
	}
	if len(env.Error.Details) > 0 && json.Unmarshal(env.Error.Details, &d) == nil {
		reason = d.Reason
	}
	return env.Error.Code, reason, true
}
