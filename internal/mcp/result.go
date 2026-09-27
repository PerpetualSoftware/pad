package mcp

import "github.com/mark3labs/mcp-go/mcp"

// This file is the SDK seam for tool results (TASK-2306). Every non-test
// dispatcher names the result type and builds results through what is
// declared here, so a future SDK swap (the modelcontextprotocol/go-sdk port)
// changes this file rather than every call site. The wire bytes are pinned
// by cmd/pad/mcp_wire_golden_test.go.

// CallToolResult is the result every tool handler and dispatcher returns.
// It is an alias, not a new type, so values move to and from mcp-go
// without conversion.
type CallToolResult = mcp.CallToolResult

// textResult is a successful result carrying plain text.
func textResult(text string) *CallToolResult {
	return mcp.NewToolResultText(text)
}

// structuredResult is a successful result carrying structured content plus
// its text rendering, for clients that read only one of the two.
func structuredResult(structured any, text string) *CallToolResult {
	return mcp.NewToolResultStructured(structured, text)
}

// errorResult is an error result carrying plain text. Prefer NewErrorResult
// (errors.go), which carries a structured envelope, wherever a code applies.
func errorResult(text string) *CallToolResult {
	return mcp.NewToolResultError(text)
}

// errorResultf is errorResult with formatting.
func errorResultf(format string, a ...any) *CallToolResult {
	return mcp.NewToolResultErrorf(format, a...)
}
