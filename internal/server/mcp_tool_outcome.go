package server

import (
	"context"
	"regexp"
	"sync"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// A tool call that fails answers the /mcp POST with 200 and a tool-level
// error result, so the status MCPAuditLog reads from the response writer
// says nothing about it: a refused write was audited as ok (BUG-3879).
// The audit middleware therefore installs an outcome sink on the request
// context, and internal/mcp's tool-handler middleware, which sees every
// tool result on every MCP server pad builds, records a failed call's
// error code into it. The sink lives here rather than in internal/mcp
// because internal/mcp imports this package.

type mcpToolOutcomeKey struct{}

// MCPToolOutcomeSink holds the failure of the tool call a request carried.
// Guarded because the tool handler and the middleware that reads the sink
// are not ordered by anything the sink can see on every transport path.
type MCPToolOutcomeSink struct {
	mu     sync.Mutex
	set    bool
	status models.MCPAuditResultStatus
	kind   string
}

// WithMCPToolOutcomeSink returns ctx carrying a fresh sink, and the sink.
// MCPAuditLog is its production caller; it is exported for internal/mcp's
// tests, which drive the tool-handler middleware without the HTTP stack.
func WithMCPToolOutcomeSink(ctx context.Context) (context.Context, *MCPToolOutcomeSink) {
	sink := &MCPToolOutcomeSink{}
	return context.WithValue(ctx, mcpToolOutcomeKey{}, sink), sink
}

// Outcome reports the recorded failure, if any.
func (s *MCPToolOutcomeSink) Outcome() (models.MCPAuditResultStatus, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.kind, s.set
}

// RecordMCPToolOutcome records that the tool call on ctx failed with the
// structured error code (one of internal/mcp's ErrorCode values) and, when
// the error payload carries one, its details.reason. A refusal is recorded
// as denied, any other failure as error. A no-op when no audit sink is on
// ctx (stdio, or a server built without MCPAuditLog).
//
// One /mcp POST carries one tools/call, so a sink sees one outcome; if it
// ever sees more, a refusal is kept over a plain error, because the refusal
// is what the audit log exists to show.
func RecordMCPToolOutcome(ctx context.Context, code, reason string) {
	sink, _ := ctx.Value(mcpToolOutcomeKey{}).(*MCPToolOutcomeSink)
	if sink == nil {
		return
	}
	status, kind := classifyMCPToolOutcome(code, reason)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.set && (sink.status == models.MCPAuditResultDenied || status != models.MCPAuditResultDenied) {
		return
	}
	sink.set, sink.status, sink.kind = true, status, kind
}

// mcpErrorKindToken bounds what a tool's error code or reason may put in
// the audit row's error_kind: a short snake_case token. Anything else is
// replaced, so a payload can never write free text into the audit log.
var mcpErrorKindToken = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// classifyMCPToolOutcome maps a failed tool call's structured error code
// onto (audit_status, error_kind). The refusal codes are the ones the
// dispatcher gives a 401, 403 or 429 (or the pre-dispatch scope and
// verified-email gates that stand in for them): auth_required,
// permission_denied, email_not_verified and rate_limited are denied, as
// classifyMCPResult makes an outer 401/403/429. email_not_verified is its
// own code since BUG-3864, not a permission_denied reason. Every other code
// is an error; the plan and member limits stay errors (BUG-3894 owns that
// policy). error_kind is
// the code, plus ":<reason>" when the payload names the gate, so the cause
// survives rather than flattening to "forbidden".
func classifyMCPToolOutcome(code, reason string) (models.MCPAuditResultStatus, string) {
	if !mcpErrorKindToken.MatchString(code) {
		code = "tool_error"
	}
	kind := code
	if reason != "" && mcpErrorKindToken.MatchString(reason) {
		kind += ":" + reason
	}
	switch code {
	case "auth_required", "permission_denied", "email_not_verified", "rate_limited":
		return models.MCPAuditResultDenied, kind
	}
	return models.MCPAuditResultError, kind
}
