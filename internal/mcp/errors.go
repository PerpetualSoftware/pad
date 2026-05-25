package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// ─────────────────────────────────────────────────────────────────────
// Structured MCP error envelopes (TASK-973)
//
// Replaces raw CLI stderr passthrough with a closed taxonomy of error
// codes the model can branch on. The agent receives a JSON envelope
// like:
//
//   {
//     "error": {
//       "code": "no_workspace",
//       "message": "No workspace context — pass workspace=<slug> or call pad_set_workspace first.",
//       "hint": "Available workspaces: docapp, pad-web. Or run pad workspace init.",
//       "available_workspaces": [{"slug": "docapp", "default": true}, ...]
//     }
//   }
//
// instead of an opaque string like "no workspace linked. Run 'pad
// workspace init'". Closed code set means the model can implement
// recovery logic per-code rather than parsing free-form text.
//
// Both ExecDispatcher (stderr classification) and HTTPHandlerDispatcher
// (HTTP status mapping) feed into the same taxonomy. Mirror impl on
// the remote side is TASK-977 — it inherits the type definitions from
// here and adds a privacy-preserving available_workspaces filter.
// ─────────────────────────────────────────────────────────────────────

// ErrorCode is one of the closed-set MCP error codes. Enumerated
// constants below; do not introduce a new code without updating the
// docs (server instructions block + getpad.dev/mcp/local).
type ErrorCode string

const (
	// ErrNoWorkspace fires when no workspace context resolves
	// (no explicit param, no session default, no CWD .pad.toml).
	// Populates available_workspaces from `pad workspace list`.
	ErrNoWorkspace ErrorCode = "no_workspace"

	// ErrUnknownWorkspace fires when a slug is supplied but doesn't
	// match any workspace the user can read. Same available_workspaces
	// hint as ErrNoWorkspace.
	ErrUnknownWorkspace ErrorCode = "unknown_workspace"

	// ErrAuthRequired fires when no valid credentials are present —
	// CLI: ~/.pad/credentials.json missing or expired; HTTP: 401.
	ErrAuthRequired ErrorCode = "auth_required"

	// ErrPermissionDenied fires when authentication succeeds but role
	// is insufficient for the operation. HTTP: 403.
	ErrPermissionDenied ErrorCode = "permission_denied"

	// ErrItemNotFound fires when an item ref / slug doesn't resolve.
	// Distinct from ErrNotFound: this code is reserved for the
	// specific case where a tool was looking up an item by ref
	// (e.g. pad_item show TASK-7 against a missing ref). The hint
	// then references pad_item search / list as recovery paths.
	ErrItemNotFound ErrorCode = "item_not_found"

	// ErrNotFound fires for resource-shaped 404s that AREN'T item
	// lookups — workspace doesn't exist, collection list returns
	// 404, dashboard route 404s, etc. Pre-TASK-1078 these all
	// collapsed to ErrItemNotFound, which misled agents into
	// thinking an item was the missing thing. Recovery is
	// "verify the slug / path you passed", not "search for items."
	ErrNotFound ErrorCode = "not_found"

	// ErrValidationFailed fires on bad input — required field missing,
	// enum value out of range, malformed JSON. HTTP: 422.
	ErrValidationFailed ErrorCode = "validation_failed"

	// ErrConflict fires when an operation collides with concurrent
	// state (e.g. version mismatch on update). HTTP: 409.
	ErrConflict ErrorCode = "conflict"

	// ErrWorkspaceRequired fires when a tool needs a workspace
	// context but the OAuth token's allow-list contains zero or
	// multiple workspaces (no unambiguous auto-default available
	// per TASK-1076). The hint points at pad_workspace list +
	// passing workspace= explicitly. Distinct from ErrNoWorkspace
	// (which is the local-CLI "no .pad.toml found" case).
	ErrWorkspaceRequired ErrorCode = "workspace_required"

	// ErrBackendUnreachable fires when the dispatcher's synthesized
	// HTTP request never reaches a meaningful response — connection
	// refused, DNS failure, upstream service down. Pre-TASK-1078
	// these collapsed into ErrServerError with bare status text;
	// distinguishing them lets agents decide "retry" vs "tell the
	// user the backend is down" vs "fix the request."
	ErrBackendUnreachable ErrorCode = "backend_unreachable"

	// ErrUpstreamError fires on 5xx responses from the backend
	// where a structured body was returned. Distinct from
	// ErrBackendUnreachable (transport never completed) and from
	// ErrServerError (catch-all for anything we don't have a code
	// for). Hint includes a body excerpt so the agent has a chance
	// of recognizing transient vs persistent failures.
	ErrUpstreamError ErrorCode = "upstream_error"

	// ErrServerError is the catch-all for unexpected failures —
	// dispatcher internal errors (build request failed, parse
	// response failed, marshal failed), unknown stderr patterns
	// from exec, etc. The wrapped message preserves the underlying
	// detail for debugging without promising any structured shape.
	ErrServerError ErrorCode = "server_error"

	// ErrOpenChildren fires when handleUpdateItem rejects a
	// non-terminal → terminal done-field transition because the
	// item still has non-terminal children (IDEA-1494). The upstream
	// 409 body carries a `details.open_children` array (refs +
	// titles + statuses + collection_slug) plus a
	// `details.hidden_blocker_count` for blockers the caller can't
	// see. We surface the upstream `code` and `details` verbatim
	// rather than collapsing to ErrConflict so MCP-driven agents
	// can self-recover (ship the listed children, then retry — or
	// pass `force: true` if the children should be intentionally
	// orphaned). The hint mentions the same escape hatch.
	ErrOpenChildren ErrorCode = "open_children"

	// ErrRateLimited fires on HTTP 429 responses — either the
	// general API limiter (per-user, 600/min, burst 60) or the
	// MCP per-token limiter (60/min/token, burst 60 post-BUG-1430)
	// rejecting a synthesized request. Distinct from
	// ErrServerError so agents implementing exponential backoff
	// can switch on code without parsing free-form text. Pre-
	// BUG-1430 this collapsed into ErrServerError, which led the
	// triggering agent to describe a 429 burst as "backend 500s
	// on parallel writes" — see BUG-1409. The hint includes the
	// upstream message so agents have a chance of recognizing
	// transient vs persistent throttling.
	ErrRateLimited ErrorCode = "rate_limited"

	// ErrPlanLimitExceeded fires on HTTP 403 responses that carry
	// error.code="plan_limit_exceeded" — a free-tier plan enforcement
	// gate (e.g. items_per_workspace, members_per_workspace, workspaces,
	// api_tokens). Distinct from ErrPermissionDenied (role/RBAC) so
	// MCP-driven agents can surface an "upgrade to Pro" signal rather
	// than reporting a permission error. Details carries feature/limit/
	// current/plan/upgrade_url from the server (TASK-788).
	ErrPlanLimitExceeded ErrorCode = "plan_limit_exceeded"
)

// ErrorEnvelope is the wire shape returned to MCP clients on tool
// failures. The outer key is `error` so the JSON unambiguously signals
// "this is the error path"; clients that want to switch on code can do
// so without inspecting IsError separately.
type ErrorEnvelope struct {
	Error ErrorPayload `json:"error"`
}

// ErrorPayload is the structured error body. Optional fields use
// pointers / `omitempty` so they only appear when populated, keeping
// success-case-shaped clients happy. Per-code fields documented inline.
type ErrorPayload struct {
	// Code is one of the ErrorCode constants. Stable across versions.
	Code ErrorCode `json:"code"`

	// Message is a short, human-readable summary suitable for direct
	// display. Avoid PII / token values here — the message may end up
	// in logs.
	Message string `json:"message"`

	// Hint is a longer suggestion for self-recovery. May reference
	// commands, alternate values, or follow-up reads. Optional.
	Hint string `json:"hint,omitempty"`

	// AvailableWorkspaces is populated for ErrNoWorkspace /
	// ErrUnknownWorkspace so the agent can pick a valid slug without
	// a human round-trip. Empty array means lookup failed (e.g. no
	// auth) — agents should treat this as "no workspace listing
	// available" rather than "no workspaces exist."
	AvailableWorkspaces []WorkspaceHint `json:"available_workspaces,omitempty"`

	// Field / Expected / Got populate ErrValidationFailed when the
	// underlying error pinpoints a specific input.
	Field    string `json:"field,omitempty"`
	Expected string `json:"expected,omitempty"`
	Got      string `json:"got,omitempty"`

	// RequiredRole / CurrentRole populate ErrPermissionDenied so
	// agents see why the call was rejected.
	RequiredRole string `json:"required_role,omitempty"`
	CurrentRole  string `json:"current_role,omitempty"`

	// Details carries the upstream error body's `details` object
	// verbatim when the upstream sets one. Used by ErrOpenChildren
	// (IDEA-1494) so MCP-driven agents get the same machine-readable
	// payload — open_children list, hidden_blocker_count, done_field,
	// attempted_value — that the HTTP API surfaces. json.RawMessage
	// because the shape is code-specific; clients re-parse against
	// the known schema for their code branch.
	Details json.RawMessage `json:"details,omitempty"`
}

// WorkspaceHint is a minimal workspace summary surfaced in the
// no_workspace / unknown_workspace envelopes.
type WorkspaceHint struct {
	Slug    string `json:"slug"`
	Name    string `json:"name,omitempty"`
	Default bool   `json:"default,omitempty"`
}

// NewErrorResult packages an ErrorPayload as an MCP CallToolResult
// with IsError=true. Both the JSON envelope and a human-readable
// summary are returned: the envelope as structured content for clients
// that parse it (Claude Desktop, Cursor), the summary as text fallback.
func NewErrorResult(p ErrorPayload) *mcp.CallToolResult {
	envelope := ErrorEnvelope{Error: p}
	body, err := json.Marshal(envelope)
	if err != nil {
		// Marshal of a struct with only string + bool + slice fields
		// can't realistically fail; defensive fallback returns a plain
		// errorf so the agent at least sees something.
		return mcp.NewToolResultErrorf("%s: %s", p.Code, p.Message)
	}
	// NewToolResultStructured returns a result with content blocks
	// PLUS structured content. The IsError flag has to be set after
	// because the structured constructor doesn't accept it as a
	// parameter — set it here so MCP clients see both.
	res := mcp.NewToolResultStructured(envelope, string(body))
	res.IsError = true
	return res
}

// noWorkspaceResult builds the standard ErrNoWorkspace envelope with
// available_workspaces populated by the supplied lookup. Lookup is
// best-effort: failures (e.g. no auth) yield an envelope with empty
// AvailableWorkspaces rather than dropping the whole error.
func noWorkspaceResult(ctx context.Context, lookup WorkspaceLister) *mcp.CallToolResult {
	hints := bestEffortWorkspaceHints(ctx, lookup)
	return NewErrorResult(ErrorPayload{
		Code:                ErrNoWorkspace,
		Message:             "No workspace context. Pass `workspace` explicitly, call pad_set_workspace first, or run from a directory with .pad.toml.",
		Hint:                workspaceHintLine(hints),
		AvailableWorkspaces: hints,
	})
}

// unknownWorkspaceResult wraps a "workspace X doesn't exist" failure.
// Same available_workspaces enrichment as no_workspace. Empty slug
// emits a generic message rather than a misleading `Workspace ""`
// — happens when the source error doesn't explicitly name the slug.
func unknownWorkspaceResult(ctx context.Context, slug string, lookup WorkspaceLister) *mcp.CallToolResult {
	hints := bestEffortWorkspaceHints(ctx, lookup)
	message := "Workspace not visible to this session."
	if slug != "" {
		message = fmt.Sprintf("Workspace %q is not visible to this session.", slug)
	}
	return NewErrorResult(ErrorPayload{
		Code:                ErrUnknownWorkspace,
		Message:             message,
		Hint:                workspaceHintLine(hints),
		AvailableWorkspaces: hints,
	})
}

// workspaceHintLine returns a concise comma-joined slug list, or an
// empty string when no hints were resolved (avoids "Available
// workspaces: " trailing nothing).
func workspaceHintLine(hints []WorkspaceHint) string {
	if len(hints) == 0 {
		return ""
	}
	slugs := make([]string, 0, len(hints))
	for _, h := range hints {
		slugs = append(slugs, h.Slug)
	}
	return "Available workspaces: " + strings.Join(slugs, ", ")
}

// WorkspaceLister is the side-channel a dispatcher exposes so error
// helpers can populate available_workspaces hints. Returning an empty
// slice (rather than an error) when lookup fails is fine — callers
// already treat empty as "no listing available."
type WorkspaceLister interface {
	ListWorkspaces(ctx context.Context) ([]WorkspaceHint, error)
}

// bestEffortWorkspaceHints calls lookup.ListWorkspaces and swallows
// errors. The error envelope is more valuable than nothing even when
// the listing failed.
func bestEffortWorkspaceHints(ctx context.Context, lookup WorkspaceLister) []WorkspaceHint {
	if lookup == nil {
		return nil
	}
	hints, err := lookup.ListWorkspaces(ctx)
	if err != nil {
		return nil
	}
	return hints
}

// envelopeFrom reads the ErrorEnvelope back out of an MCP error
// result's structured content. Used by classifyHTTPStatus's 404
// workspace path so we can layer additional Hint detail on top of the
// envelope unknownWorkspaceResult already built. Returns a zero
// envelope when the structured content is missing or malformed —
// callers fall back gracefully.
func envelopeFrom(res *mcp.CallToolResult) ErrorEnvelope {
	if res == nil {
		return ErrorEnvelope{}
	}
	if env, ok := res.StructuredContent.(ErrorEnvelope); ok {
		return env
	}
	return ErrorEnvelope{}
}

// ─────────────────────────────────────────────────────────────────────
// ExecDispatcher classification
//
// The local subprocess emits stderr strings that we pattern-match
// against known cases. Unmatched output falls through to ErrServerError
// with the raw stderr preserved in Message.
// ─────────────────────────────────────────────────────────────────────

// structuredErrorMarker mirrors internal/cli.StructuredErrorMarker —
// the versioned line prefix the CLI writes to stderr when it surfaces
// a structured error. Duplicated (rather than imported) so the mcp
// classifier doesn't pull the cli package's dependency graph in for
// one string. Codex round-3 P3 introduced the versioned shape.
//
// IMPORTANT: keep in lockstep with cli.StructuredErrorMarker AND with
// allowedStructuredErrorCodes below. A wire-shape change requires
// bumping both packages and reviewing the allow-list.
const structuredErrorMarker = "pad-structured-error/v1: "

// allowedStructuredErrorCodes is the whitelist of upstream codes the
// MCP layer will surface verbatim. Two transports consult it:
//
//   - Stdio: extractStructuredCLIError gates the `pad-structured-error/v1:`
//     marker on this set (Codex round-3 P3).
//   - HTTP: classifyHTTPStatus's 409 branch gates the upstream
//     `error.code` pass-through on this set (Codex round-4 P2).
//
// Routing BOTH transports through the same whitelist keeps the
// ErrorCode enum's closed-contract honest — agents see the same set
// of structured codes regardless of which dispatcher delivered the
// response. Pre-round-4 the HTTP path forwarded any non-"conflict"
// upstream code, which silently widened the enum and let the two
// transports diverge.
//
// Adding a new structured code is a TWO-WAY change: the pad handler
// must emit it (and emit `details` with a stable, agent-safe shape)
// AND it must be added here. The matching ErrorCode constant in the
// declarations block at the top of this file should also be added
// for type safety in test assertions.
var allowedStructuredErrorCodes = map[string]struct{}{
	"open_children":       {}, // IDEA-1494
	"plan_limit_exceeded": {}, // TASK-788
}

// extractStructuredCLIError scans stderr for the
// `pad-structured-error/v1: {json}\n` marker line and lifts the
// structured error envelope into an MCP result. Returns nil when no
// marker is found OR the payload fails any of the hardening checks
// (caller falls back to the regex-based classifiers).
//
// Hardening (Codex round-3 P3):
//   - Versioned marker. Unknown versions return nil.
//   - Whitelisted codes. Unknown codes return nil.
//   - Last-marker-wins. If multiple markers appear in stderr (a
//     pathological case today, but possible if a future caller emits
//     several), we use the LAST one — that's the most recent
//     classification the CLI emitted, and a malicious earlier line
//     can't pre-empt it.
//   - Marker must start the line after trimming whitespace; markers
//     embedded mid-line (e.g. in a quoted log message) are ignored.
func extractStructuredCLIError(stderr string) *mcp.CallToolResult {
	if !strings.Contains(stderr, structuredErrorMarker) {
		return nil
	}
	var lastPayload string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, structuredErrorMarker) {
			continue
		}
		lastPayload = strings.TrimPrefix(line, structuredErrorMarker)
	}
	if lastPayload == "" {
		return nil
	}
	var env struct {
		Error struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details,omitempty"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(lastPayload), &env); err != nil {
		// Malformed marker payload — fall through to regex
		// classification rather than blowing up the agent.
		return nil
	}
	if env.Error.Code == "" {
		return nil
	}
	if _, ok := allowedStructuredErrorCodes[env.Error.Code]; !ok {
		// Unknown code. Don't forward — regex classifier handles it.
		return nil
	}
	return NewErrorResult(ErrorPayload{
		Code:    ErrorCode(env.Error.Code),
		Message: env.Error.Message,
		Details: env.Error.Details,
	})
}

// classifyExecError turns an exec.Cmd failure (err + stderr) into a
// structured envelope. lookup is optional — when supplied, no_workspace
// errors get available_workspaces enrichment.
func classifyExecError(ctx context.Context, cmdPath []string, runErr error, stderr string, lookup WorkspaceLister) *mcp.CallToolResult {
	// IDEA-1494 R2: the CLI emits a single `pad-error: {json}` line
	// on stderr when surfacing the open-children rejection (see
	// internal/cli/client.go::WriteOpenChildrenError). Detect it
	// before regex classification so we lift the structured `code`
	// + `details` straight through to the MCP envelope instead of
	// pattern-matching free-form text and downgrading to
	// validation_failed. Matched on a literal prefix to keep the
	// classifier cheap and the contract obvious.
	if structured := extractStructuredCLIError(stderr); structured != nil {
		return structured
	}

	// BUG-987 bug 11: cobra automatically appends a "Usage: ..." block
	// to stderr when a command fails with a runtime error. That help
	// text uses the OLD CLI verb names (e.g. `pad item block`) which
	// confuses agents using the v0.2 catalog (`pad_item action=link
	// link_type=blocks`) and bloats error messages. Strip the Usage
	// block before classification so neither matchers nor envelope
	// content carry it.
	stderr = stripCobraUsageBlock(stderr)
	stderr = strings.TrimSpace(stderr)
	lower := strings.ToLower(stderr)

	switch {
	case execStderrMatchesNoWorkspace(lower):
		return noWorkspaceResult(ctx, lookup)
	case execStderrMatchesUnknownWorkspace(lower):
		// Stderr typically embeds the slug — try to extract it. If
		// extraction fails, the envelope still carries the message;
		// the slug just won't appear in the hint line.
		slug := extractUnknownWorkspaceSlug(stderr)
		return unknownWorkspaceResult(ctx, slug, lookup)
	case execStderrMatchesAuthRequired(lower):
		return NewErrorResult(ErrorPayload{
			Code:    ErrAuthRequired,
			Message: "Authentication required. Run `pad auth login` to sign in.",
			Hint:    stripErrorPrefix(stderr),
		})
	case execStderrMatchesPermissionDenied(lower):
		return NewErrorResult(ErrorPayload{
			Code:    ErrPermissionDenied,
			Message: "Permission denied for this operation.",
			Hint:    stripErrorPrefix(stderr),
		})
	case execStderrMatchesItemNotFound(lower):
		return NewErrorResult(ErrorPayload{
			Code:    ErrItemNotFound,
			Message: "Item not found.",
			Hint:    stripErrorPrefix(stderr),
		})
	case execStderrMatchesValidation(lower):
		return NewErrorResult(ErrorPayload{
			Code:    ErrValidationFailed,
			Message: "Validation failed.",
			Hint:    stripErrorPrefix(stderr),
		})
	}

	// Fallback: unstructured server error. Surface the cleaned stderr
	// directly without the legacy "pad <cmd> failed:" prefix — that
	// prefix referenced OLD CLI verb names (e.g. `pad item block`)
	// that don't match the v0.2 catalog action names agents see, and
	// the cmdPath is already implicit from which tool was called
	// (BUG-987 bug 11 round 2).
	msg := stderr
	if msg == "" && runErr != nil {
		msg = runErr.Error()
	}
	if msg == "" {
		msg = "unknown error"
	}
	return NewErrorResult(ErrorPayload{
		Code:    ErrServerError,
		Message: stripErrorPrefix(msg),
	})
}

// stripErrorPrefix removes the leading "Error:" prefix that the CLI
// (and many of its dependencies) emit on stderr lines. Cosmetic — the
// envelope's error.code already carries the "this is an error" signal
// so the prefix is redundant noise.
func stripErrorPrefix(msg string) string {
	msg = strings.TrimSpace(msg)
	for _, prefix := range []string{"Error: ", "error: ", "ERROR: "} {
		if strings.HasPrefix(msg, prefix) {
			return strings.TrimSpace(msg[len(prefix):])
		}
	}
	return msg
}

// Stderr-pattern matchers. Compiled at init for cost-free
// classification. Patterns are case-insensitive against `lower`
// (the caller pre-lowercases for performance).
var (
	reNoWorkspace       = regexp.MustCompile(`no workspace.*(linked|configured)`)
	reUnknownWorkspaceA = regexp.MustCompile(`workspace .* (does not exist|not found)`)
	reUnknownWorkspaceB = regexp.MustCompile(`unknown workspace`)
	reAuthRequired      = regexp.MustCompile(`(not authenticated|authentication required|please log in|run pad auth login|invalid token|expired token)`)
	rePermissionDenied  = regexp.MustCompile(`(permission denied|forbidden|insufficient (permissions|role))`)
	reItemNotFound      = regexp.MustCompile(`(item.*not found|no such item|unknown ref)`)
	// reValidationFailed catches generic validation phrasings. Includes
	// "cannot ..." (e.g. "cannot link an item to itself", "cannot
	// modify archived item") which are validation-shaped server
	// rejections previously falling through to server_error
	// (BUG-987 bug 11 round 2).
	reValidationFailed = regexp.MustCompile(`(invalid|missing required|must be one of|validation|cannot )`)
	// Only match QUOTED slugs to avoid capturing stop-words like "not"
	// in generic "Workspace not found" / "workspace not visible"
	// messages. Quoted forms come from CLI stderr ("workspace 'foo'
	// does not exist") and from JSON error bodies that explicitly name
	// the slug. Unquoted phrasings yield an empty slug — the
	// unknownWorkspaceResult message handles that gracefully without
	// emitting a misleading `Workspace "not"` line.
	reUnknownWorkspaceID = regexp.MustCompile(`workspace ['"]([a-z0-9][a-z0-9-]*)['"]`)
)

func execStderrMatchesNoWorkspace(lower string) bool { return reNoWorkspace.MatchString(lower) }
func execStderrMatchesUnknownWorkspace(lower string) bool {
	return reUnknownWorkspaceA.MatchString(lower) || reUnknownWorkspaceB.MatchString(lower)
}
func execStderrMatchesAuthRequired(lower string) bool { return reAuthRequired.MatchString(lower) }
func execStderrMatchesPermissionDenied(lower string) bool {
	return rePermissionDenied.MatchString(lower)
}
func execStderrMatchesItemNotFound(lower string) bool { return reItemNotFound.MatchString(lower) }
func execStderrMatchesValidation(lower string) bool   { return reValidationFailed.MatchString(lower) }

// extractUnknownWorkspaceSlug pulls the slug from a CLI stderr like
// "workspace 'foo' does not exist" so the envelope can name it.
// Returns empty string if nothing matches; callers fall back to a
// generic "this slug" phrasing.
func extractUnknownWorkspaceSlug(stderr string) string {
	m := reUnknownWorkspaceID.FindStringSubmatch(strings.ToLower(stderr))
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// validationFailedFromBuildErr wraps a BuildCLIArgs error (typically
// "missing required argument %q" or "flag %q: <type-mismatch>") as a
// structured validation_failed envelope. Best-effort extraction of
// the offending field name out of Go's error wrapping conventions —
// when the regex misses, the message itself still carries the
// underlying text.
//
// BUG-987 bug 12: previously BuildCLIArgs failures came out of
// env.Dispatch as bare mcp.NewToolResultErrorf strings, breaking the
// structured-envelope invariant that every other error path follows.
func validationFailedFromBuildErr(cmdPath string, err error) *mcp.CallToolResult {
	msg := err.Error()
	field := extractValidationField(msg)
	payload := ErrorPayload{
		Code:    ErrValidationFailed,
		Message: fmt.Sprintf("validation failed for `%s`: %s", cmdPath, msg),
		Field:   field,
	}
	return NewErrorResult(payload)
}

// reValidationField matches the field-name token in BuildCLIArgs's
// error strings. Both `argument "x"` and `flag "x"` formats cover
// ~all of its err paths (see internal/mcp/dispatch.go's BuildCLIArgs).
var reValidationField = regexp.MustCompile(`(?:argument|flag)\s+"([^"]+)"`)

// extractValidationField pulls the field name out of a BuildCLIArgs
// error message, returning empty string when no match is found.
func extractValidationField(msg string) string {
	m := reValidationField.FindStringSubmatch(msg)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// stripCobraUsageBlock removes cobra's auto-appended "Usage: ..."
// help block from a stderr string. cobra emits this block on any
// runtime error from a RunE handler — useful for human users running
// the CLI directly, but noise in MCP error envelopes (and worse,
// references CLI verb names like `pad item block` that aren't part
// of the v0.2 MCP catalog at all, BUG-987 bug 11).
//
// The block is recognizable: a line containing exactly "Usage:"
// (with optional surrounding whitespace) followed by the help text.
// Truncate at the first such line. If no Usage block is present
// (the typical no-cobra-help error path), the input is returned
// unchanged.
func stripCobraUsageBlock(stderr string) string {
	idx := indexOfUsageLine(stderr)
	if idx < 0 {
		return stderr
	}
	return strings.TrimRight(stderr[:idx], " \t\r\n")
}

// indexOfUsageLine returns the byte offset of the line containing
// "Usage:" (cobra's help-block prefix), or -1 when absent. The match
// is anchored to a line start (preceded by '\n' or string start) so
// in-message mentions of the word "Usage:" don't accidentally
// truncate.
func indexOfUsageLine(s string) int {
	const marker = "Usage:"
	idx := 0
	for {
		rel := strings.Index(s[idx:], marker)
		if rel < 0 {
			return -1
		}
		abs := idx + rel
		// Anchor: must be at the start of a line. A leading newline
		// (with optional whitespace before the marker) qualifies.
		if abs == 0 || isLineStart(s, abs) {
			return abs
		}
		idx = abs + len(marker)
	}
}

// isLineStart returns true when the character at byte position i is
// preceded by a newline (with arbitrary leading whitespace allowed
// between the newline and i).
func isLineStart(s string, i int) bool {
	for j := i - 1; j >= 0; j-- {
		c := s[j]
		if c == ' ' || c == '\t' {
			continue
		}
		return c == '\n'
	}
	return true
}

// ─────────────────────────────────────────────────────────────────────
// HTTPHandlerDispatcher classification
//
// HTTP status codes map cleanly to the taxonomy. Body parsing is
// best-effort: when the handler returns a structured payload we
// surface its message; otherwise the status text serves as the
// envelope's Message.
// ─────────────────────────────────────────────────────────────────────

// ResourceKind hints what shape of resource a dispatcher was reading
// when it hit an error. Drives 404 classification (item_not_found vs
// not_found) and the contextual hint text per code (TASK-1078 +
// TASK-1079).
//
// The dispatcher knows exactly what it was reading (an item, a
// workspace, a collection list, etc.); the classifier doesn't need
// to guess from URL paths or body strings.
type ResourceKind string

const (
	// ResourceUnknown is the legacy "we don't know what was being
	// read" sentinel — call sites that haven't been retrofitted yet
	// pass this and get the pre-TASK-1078 behaviour (404 →
	// item_not_found by default unless body says workspace).
	ResourceUnknown ResourceKind = ""

	// ResourceItem signals an item-by-ref lookup (pad_item show /
	// update / delete / star / etc.). 404 → item_not_found with
	// pad_item search / list as the recovery hint.
	ResourceItem ResourceKind = "item"

	// ResourceWorkspace signals a workspace-level read or write
	// (pad_workspace * actions, pad_project dashboard, etc.).
	// 404 → unknown_workspace with available_workspaces enrichment.
	ResourceWorkspace ResourceKind = "workspace"

	// ResourceCollection signals a collection-shaped read
	// (pad_collection list / show, item-list-by-collection).
	// 404 → not_found with the collection-name in the hint.
	ResourceCollection ResourceKind = "collection"

	// ResourceListing signals a generic listing endpoint where
	// 404 means the parent route doesn't exist (NOT "no rows" —
	// list endpoints return 200 with [] for that case). Used for
	// pad_workspace list, pad_role list, pad_project dashboard.
	ResourceListing ResourceKind = "listing"

	// ResourceLink signals a link create/delete operation. 404 →
	// not_found with the link-target ref in the hint.
	ResourceLink ResourceKind = "link"

	// ResourceAttachment signals an attachment metadata read.
	// 404 → not_found with the attachment_id in the hint.
	ResourceAttachment ResourceKind = "attachment"
)

// classifyHTTPStatus is the legacy entry point preserved for callers
// that don't know their resource kind. New callers should use
// classifyHTTPStatusKind directly.
func classifyHTTPStatus(ctx context.Context, cmdKey string, status int, body []byte, lookup WorkspaceLister) *mcp.CallToolResult {
	return classifyHTTPStatusKind(ctx, cmdKey, "", status, body, lookup, ResourceUnknown, "")
}

// classifyHTTPStatusKind turns an HTTP error response into a structured
// envelope, using the dispatcher's known resource context to emit the
// right error code and an actionable hint (TASK-1078 / TASK-1079).
//
// Parameters:
//
//   - ctx, cmdKey, lookup — same as classifyHTTPStatus.
//   - route — the URL path the dispatcher hit, e.g.
//     "/api/v1/workspaces/foo/items/TASK-7". Used in hint text so
//     agents know which path failed without parsing the message.
//     May be empty when the caller doesn't have a route handy
//     (legacy classifyHTTPStatus path).
//   - status, body — same as classifyHTTPStatus.
//   - kind — what shape of resource was being read. Drives both
//     the code selection (404 splits item_not_found / unknown_workspace
//     / not_found) and the hint text per code.
//   - refOrSlug — the specific identifier the dispatcher was looking
//     up (a TASK-N ref for items, a workspace slug for workspaces,
//     etc.). Used in the hint so an agent reading "Item ref TASK-7
//     not found in workspace foo" knows exactly what to fix. May be
//     empty.
func classifyHTTPStatusKind(
	ctx context.Context,
	cmdKey, route string,
	status int,
	body []byte,
	lookup WorkspaceLister,
	kind ResourceKind,
	refOrSlug string,
) *mcp.CallToolResult {
	bodyText := strings.TrimSpace(string(body))
	bodyMessage := extractUpstreamMessage(bodyText)
	if bodyText == "" {
		bodyText = http.StatusText(status)
	}

	switch status {
	case http.StatusUnauthorized:
		return NewErrorResult(ErrorPayload{
			Code:    ErrAuthRequired,
			Message: "Authentication required.",
			Hint:    authHintFor(bodyMessage, route),
		})
	case http.StatusForbidden:
		// TASK-788: when the handler emitted a plan_limit_exceeded structured body,
		// surface it with the dedicated code + details so MCP-driven agents can
		// present an upgrade-to-Pro signal instead of a generic permission error.
		// Whitelisted through allowedStructuredErrorCodes (same pattern as
		// open_children on 409) so unknown 403 codes still collapse to
		// ErrPermissionDenied.
		upstream403 := extractUpstreamErrorEnvelope(bodyText)
		if _, allowed := allowedStructuredErrorCodes[upstream403.Code]; allowed {
			return NewErrorResult(ErrorPayload{
				Code:    ErrorCode(upstream403.Code),
				Message: upstream403.Message,
				Hint:    planLimitHintFor(upstream403.Message, route),
				Details: upstream403.Details,
			})
		}
		return NewErrorResult(ErrorPayload{
			Code:    ErrPermissionDenied,
			Message: "Permission denied for this operation.",
			Hint:    permissionHintFor(bodyMessage, route),
		})
	case http.StatusNotFound:
		return classify404(ctx, cmdKey, route, bodyText, bodyMessage, lookup, kind, refOrSlug)
	case http.StatusConflict:
		// IDEA-1494 R2/R4 P2: preserve the upstream `code` + `details`
		// when the handler emitted a structured rejection — but ONLY
		// for codes that appear in the shared allow-list
		// (allowedStructuredErrorCodes, defined below). The stdio
		// classifier already gates on the same set; routing both
		// transports through the same whitelist keeps the
		// ErrorCode enum's closed contract honest (round-4 P2:
		// HTTP was forwarding any non-"conflict" code, diverging
		// from stdio).
		//
		// New structured codes get added to allowedStructuredErrorCodes
		// in one place; both transports adopt them in lockstep.
		// Unknown codes fall through to generic ErrConflict here —
		// matching what stdio does for an unknown-code marker.
		upstream := extractUpstreamErrorEnvelope(bodyText)
		if _, allowed := allowedStructuredErrorCodes[upstream.Code]; allowed {
			return NewErrorResult(ErrorPayload{
				Code:    ErrorCode(upstream.Code),
				Message: upstream.Message,
				Hint:    conflictHintFor(bodyMessage, route),
				Details: upstream.Details,
			})
		}
		return NewErrorResult(ErrorPayload{
			Code:    ErrConflict,
			Message: "Conflict — current state changed beneath this update.",
			Hint:    conflictHintFor(bodyMessage, route),
		})
	case http.StatusUnprocessableEntity, http.StatusBadRequest:
		return NewErrorResult(ErrorPayload{
			Code:    ErrValidationFailed,
			Message: "Validation failed.",
			Hint:    validationHintFor(bodyMessage, route),
		})
	case http.StatusTooManyRequests:
		// BUG-1430: pre-existing behavior collapsed 429 into the
		// generic ErrServerError "other 4xx" bucket, which led
		// the triggering agent to report a parallel-write burst
		// as "backend 500s." Surface 429 as a first-class
		// ErrRateLimited so agents can implement code-based
		// backoff instead of parsing free-form text. The hint
		// mentions the Retry-After header (set on the response
		// by writeRateLimitResponse / writeMCPRateLimit) since
		// the dispatcher's body-only signature loses the headers
		// — a future enhancement could surface the parsed value
		// directly in the envelope.
		return NewErrorResult(ErrorPayload{
			Code:    ErrRateLimited,
			Message: fmt.Sprintf("pad %s rate-limited (HTTP 429)", cmdKey),
			Hint:    rateLimitHintFor(bodyMessage, route),
		})
	}

	if status >= 500 {
		return NewErrorResult(ErrorPayload{
			Code:    ErrUpstreamError,
			Message: fmt.Sprintf("pad %s failed: backend returned %d", cmdKey, status),
			Hint:    upstreamHintFor(bodyMessage, route, status),
		})
	}
	// Other 4xx without a specific mapping — surface as server_error
	// with the raw body so debugging is still possible. Avoid silently
	// promoting them to validation_failed; that would mislead callers.
	return NewErrorResult(ErrorPayload{
		Code:    ErrServerError,
		Message: fmt.Sprintf("pad %s failed (HTTP %d)", cmdKey, status),
		Hint:    serverHintFor(bodyMessage, route, status),
	})
}

// classify404 splits HTTP 404 by ResourceKind into the right code +
// hint. Called from classifyHTTPStatusKind only — extracted so the
// 404-shaped logic doesn't dominate the parent switch statement.
func classify404(
	ctx context.Context,
	cmdKey, route, bodyText, bodyMessage string,
	lookup WorkspaceLister,
	kind ResourceKind,
	refOrSlug string,
) *mcp.CallToolResult {
	// Workspace-shaped 404s: route through the existing
	// unknown_workspace path so available_workspaces enrichment fires.
	// Body-string sniffing is the legacy fallback for callers that
	// pass ResourceUnknown.
	if kind == ResourceWorkspace ||
		(kind == ResourceUnknown && strings.Contains(strings.ToLower(bodyText), "workspace")) {
		slug := refOrSlug
		if slug == "" {
			slug = extractUnknownWorkspaceSlug(bodyText)
		}
		res := unknownWorkspaceResult(ctx, slug, lookup)
		env := envelopeFrom(res)
		// Layer in a route-aware hint suffix so the agent sees the
		// path that 404'd in addition to the workspace-list hint.
		extra := workspaceMissingHint(slug, route, bodyMessage)
		if env.Error.Hint == "" {
			env.Error.Hint = extra
		} else if extra != "" {
			env.Error.Hint = extra + " — " + env.Error.Hint
		}
		return NewErrorResult(env.Error)
	}
	switch kind {
	case ResourceItem:
		return NewErrorResult(ErrorPayload{
			Code:    ErrItemNotFound,
			Message: "Item not found.",
			Hint:    itemMissingHint(refOrSlug, route, bodyMessage),
		})
	case ResourceUnknown:
		// Legacy fallback — preserved so call sites that haven't
		// been retrofitted yet keep the pre-TASK-1078 code mapping.
		// Use itemMissingHint (NOT raw bodyText) so the safe-extracted
		// bodyMessage is the only upstream content forwarded, matching
		// the privacy contract extractUpstreamMessage establishes
		// (Codex review #387 round 2 caught the residual leak from
		// the previous bare-`bodyText` Hint).
		return NewErrorResult(ErrorPayload{
			Code:    ErrItemNotFound,
			Message: "Item not found.",
			Hint:    itemMissingHint(refOrSlug, route, bodyMessage),
		})
	default:
		// All other resource kinds (collection, listing, link,
		// attachment) get the generic not_found code with a
		// kind-aware hint.
		return NewErrorResult(ErrorPayload{
			Code:    ErrNotFound,
			Message: notFoundMessageFor(kind, refOrSlug),
			Hint:    notFoundHintFor(kind, refOrSlug, route, bodyMessage),
		})
	}
}

// extractUpstreamMessage pulls a human-readable message out of an
// upstream error body. Three return shapes:
//
//   - Pad's structured envelope ({error:{message:...}}) → return the
//     inner message verbatim. This is the common case for any 4xx /
//     5xx pad emits internally (every handler uses writeError which
//     produces this shape).
//   - chi's default 404 body ("404 page not found") → return "" so
//     the hint omits the upstream-message clause entirely. The
//     literal text has zero diagnostic value (Bug 17) and forwarding
//     it doesn't help anyone.
//   - Anything else → return "". Per Codex review #387 round 1, the
//     pre-fix fallback returned the raw body — which would forward
//     non-envelope upstream JSON / HTML / debug dumps, including any
//     tokens / passwords / internal-only fields the backend might
//     surface in a 5xx. Refusing to forward unstructured bodies is
//     the safer default; operators debugging a non-envelope upstream
//     can read the pad container logs, agents don't need the raw
//     body to recover.
//
// Trailing whitespace is trimmed before classification because chi's
// "404 page not found" body ships with a trailing newline and the
// EqualFold check needs to match it byte-for-byte.
func extractUpstreamMessage(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	if strings.EqualFold(body, "404 page not found") {
		return ""
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	return ""
}

// upstreamErrorEnvelope is the broader extractor used when the
// dispatcher needs to pass through the upstream `code` / `details`
// fields (not just the message). The pad backend uses the same
// `{"error":{"code":..., "message":..., "details":{...}}}` shape
// across every handler that returns writeError-style bodies; this
// parser lifts those fields when present and reports zero-valued
// strings / nil RawMessage when not. Single source of truth for the
// pass-through cases in classifyHTTPStatusKind.
type upstreamErrorEnvelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

func extractUpstreamErrorEnvelope(body string) upstreamErrorEnvelope {
	body = strings.TrimSpace(body)
	if body == "" {
		return upstreamErrorEnvelope{}
	}
	if strings.EqualFold(body, "404 page not found") {
		return upstreamErrorEnvelope{}
	}
	var env struct {
		Error upstreamErrorEnvelope `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err == nil {
		return env.Error
	}
	return upstreamErrorEnvelope{}
}

// itemMissingHint is the per-error-code hint generator for
// ErrItemNotFound. References the actual ref + route so the agent
// can pin the failure without re-parsing the message, plus points
// at the next-step recovery tools.
func itemMissingHint(ref, route, bodyMsg string) string {
	parts := []string{}
	if ref != "" {
		parts = append(parts, fmt.Sprintf("Item %q not found.", ref))
	} else {
		parts = append(parts, "Item not found at the requested ref.")
	}
	if route != "" {
		parts = append(parts, fmt.Sprintf("Route: %s.", route))
	}
	parts = append(parts, "Try `pad_item search` or `pad_item list` to find the right ref.")
	if bodyMsg != "" && !strings.EqualFold(bodyMsg, "404 page not found") {
		parts = append(parts, fmt.Sprintf("Backend: %s", bodyMsg))
	}
	return strings.Join(parts, " ")
}

// workspaceMissingHint generates the route-aware suffix appended to
// the standard unknown_workspace envelope's available_workspaces line.
func workspaceMissingHint(slug, route, bodyMsg string) string {
	parts := []string{}
	if slug != "" {
		parts = append(parts, fmt.Sprintf("Workspace %q not visible.", slug))
	}
	if route != "" {
		parts = append(parts, fmt.Sprintf("Route: %s.", route))
	}
	if bodyMsg != "" && !strings.EqualFold(bodyMsg, "404 page not found") {
		parts = append(parts, fmt.Sprintf("Backend: %s", bodyMsg))
	}
	return strings.Join(parts, " ")
}

// notFoundMessageFor returns a kind-aware short message for the
// not_found code. Falls back to a generic "Resource not found" when
// the kind doesn't match a specialized case.
func notFoundMessageFor(kind ResourceKind, refOrSlug string) string {
	switch kind {
	case ResourceCollection:
		if refOrSlug != "" {
			return fmt.Sprintf("Collection %q not found.", refOrSlug)
		}
		return "Collection not found."
	case ResourceLink:
		return "Link target not found."
	case ResourceAttachment:
		if refOrSlug != "" {
			return fmt.Sprintf("Attachment %q not found.", refOrSlug)
		}
		return "Attachment not found."
	case ResourceListing:
		return "Listing endpoint did not respond."
	}
	return "Resource not found."
}

// notFoundHintFor generates the actionable hint for ErrNotFound.
// References the route + ref/slug + suggests verification of the
// path the caller passed.
func notFoundHintFor(kind ResourceKind, refOrSlug, route, bodyMsg string) string {
	parts := []string{}
	switch kind {
	case ResourceCollection:
		parts = append(parts, "Verify the collection slug exists; use `pad_collection list` to enumerate.")
	case ResourceLink:
		parts = append(parts, fmt.Sprintf("Verify the target ref %q exists.", refOrSlug))
	case ResourceAttachment:
		parts = append(parts, "Verify the attachment_id from the parent item.")
	case ResourceListing:
		parts = append(parts, "Verify the route matches the server's API surface (build version may be stale).")
	default:
		parts = append(parts, "Verify the path you passed.")
	}
	if route != "" {
		parts = append(parts, fmt.Sprintf("Route: %s.", route))
	}
	if bodyMsg != "" && !strings.EqualFold(bodyMsg, "404 page not found") {
		parts = append(parts, fmt.Sprintf("Backend: %s", bodyMsg))
	}
	return strings.Join(parts, " ")
}

// authHintFor generates the actionable hint for ErrAuthRequired.
// Distinct from the upstream body — agents see "fix your auth", not
// the literal upstream JSON envelope (Bug 19).
func authHintFor(bodyMsg, route string) string {
	parts := []string{"Re-authenticate (Claude Desktop: reconnect the connector; CLI: `pad auth login`)."}
	if route != "" {
		parts = append(parts, fmt.Sprintf("Route: %s.", route))
	}
	if bodyMsg != "" {
		parts = append(parts, fmt.Sprintf("Backend: %s", bodyMsg))
	}
	return strings.Join(parts, " ")
}

// permissionHintFor generates the actionable hint for
// ErrPermissionDenied. References the route + the inner backend
// message (which often names the missing role / workspace).
func permissionHintFor(bodyMsg, route string) string {
	parts := []string{"Insufficient role for this operation."}
	if route != "" {
		parts = append(parts, fmt.Sprintf("Route: %s.", route))
	}
	if bodyMsg != "" {
		parts = append(parts, fmt.Sprintf("Backend: %s", bodyMsg))
	}
	return strings.Join(parts, " ")
}

// conflictHintFor generates the actionable hint for ErrConflict.
func conflictHintFor(bodyMsg, route string) string {
	parts := []string{"Re-read the current item state and retry the update."}
	if route != "" {
		parts = append(parts, fmt.Sprintf("Route: %s.", route))
	}
	if bodyMsg != "" {
		parts = append(parts, fmt.Sprintf("Backend: %s", bodyMsg))
	}
	return strings.Join(parts, " ")
}

// validationHintFor generates the actionable hint for
// ErrValidationFailed. The backend message usually pinpoints the
// field, so it's prioritized.
func validationHintFor(bodyMsg, route string) string {
	parts := []string{}
	if bodyMsg != "" {
		parts = append(parts, fmt.Sprintf("Backend: %s", bodyMsg))
	}
	if route != "" {
		parts = append(parts, fmt.Sprintf("Route: %s.", route))
	}
	parts = append(parts, "Adjust the input shape and retry.")
	return strings.Join(parts, " ")
}

// upstreamHintFor generates the actionable hint for ErrUpstreamError.
// 5xx responses are usually transient; the hint encodes that bias.
func upstreamHintFor(bodyMsg, route string, status int) string {
	parts := []string{
		fmt.Sprintf("Backend returned %d.", status),
		"Usually transient — retry once or check pad logs for the underlying error.",
	}
	if route != "" {
		parts = append(parts, fmt.Sprintf("Route: %s.", route))
	}
	if bodyMsg != "" && bodyMsg != http.StatusText(status) {
		parts = append(parts, fmt.Sprintf("Backend: %s", bodyMsg))
	}
	return strings.Join(parts, " ")
}

// serverHintFor generates the actionable hint for ErrServerError.
// Catch-all — surface enough for an agent to file a bug report.
func serverHintFor(bodyMsg, route string, status int) string {
	parts := []string{fmt.Sprintf("Unexpected status %d from backend.", status)}
	if route != "" {
		parts = append(parts, fmt.Sprintf("Route: %s.", route))
	}
	if bodyMsg != "" {
		parts = append(parts, fmt.Sprintf("Backend: %s", bodyMsg))
	}
	return strings.Join(parts, " ")
}

// planLimitHintFor generates the actionable hint for ErrPlanLimitExceeded.
// The upgrade_url is already in the Details blob; the hint surfaces it in
// prose so agents that only read Hint (not Details) still get the destination.
func planLimitHintFor(bodyMsg, route string) string {
	parts := []string{"Upgrade to Pro at /console/billing to remove this limit."}
	if route != "" {
		parts = append(parts, fmt.Sprintf("Route: %s.", route))
	}
	if bodyMsg != "" {
		parts = append(parts, fmt.Sprintf("Backend: %s", bodyMsg))
	}
	return strings.Join(parts, " ")
}

// rateLimitHintFor generates the actionable hint for ErrRateLimited.
// 429s are recoverable with backoff — the hint encodes that bias and
// points at the Retry-After response header. BUG-1430 added this
// helper alongside the dedicated case in classifyHTTPStatusKind so
// agents see a distinct envelope for "back off" vs ErrServerError's
// "file a bug" framing.
//
// The hint is intentionally generic about the cap. classifyHTTPStatusKind
// handles 429s from the dispatcher's SYNTHESIZED /api/v1/... requests,
// which can fire from several different limiters with different sizing
// (the general API limiter at 600/min/burst-60, the Search limiter at
// 30/min/burst-10, etc.) — Codex review #546 round 1 [P2] caught the
// first draft of this hint hard-coding the MCP per-token cap, which
// fires BEFORE the dispatcher runs and so never lands here. The
// Retry-After header carries the limiter-specific wait time; agents
// honoring it get correct backoff without needing the cap in prose.
func rateLimitHintFor(bodyMsg, route string) string {
	parts := []string{
		"Rate-limited by the backend. Retry after a backoff (see the Retry-After response header for the suggested wait).",
		"For burst-heavy workflows (agent onboarding, bulk import), space out tool calls or sequence them.",
	}
	if route != "" {
		parts = append(parts, fmt.Sprintf("Route: %s.", route))
	}
	if bodyMsg != "" {
		parts = append(parts, fmt.Sprintf("Backend: %s", bodyMsg))
	}
	return strings.Join(parts, " ")
}

// ─────────────────────────────────────────────────────────────────────
// Helpers for non-HTTP error paths (TASK-1077 — replace plain-string
// NewToolResultErrorf with structured envelopes uniformly).
// ─────────────────────────────────────────────────────────────────────

// validationFailedResult wraps a "missing required input" or other
// caller-input error into the structured envelope. cmdKey identifies
// which tool was invoked; msg is the human-readable issue (e.g.
// "workspace is required", "ref is required"); fixHint is the
// recovery suggestion.
func validationFailedResult(cmdKey, msg, fixHint string) *mcp.CallToolResult {
	return NewErrorResult(ErrorPayload{
		Code:    ErrValidationFailed,
		Message: fmt.Sprintf("%s: %s", cmdKey, msg),
		Hint:    fixHint,
	})
}

// dispatcherErrorResult wraps an internal dispatcher failure
// (build request, encode body, parse response, marshal merged
// fields, etc.) into the structured envelope. These shouldn't
// happen at runtime — they're usually programmer errors or
// genuinely unexpected I/O failures.
//
// op is a short verb describing what failed ("build prefetch
// request", "encode body", "parse current item"); err is the
// underlying error — its message goes in the hint so debugging
// info isn't lost.
func dispatcherErrorResult(cmdKey, op string, err error) *mcp.CallToolResult {
	hint := fmt.Sprintf("Internal: %s — %s", op, err.Error())
	return NewErrorResult(ErrorPayload{
		Code:    ErrServerError,
		Message: fmt.Sprintf("%s: %s failed", cmdKey, op),
		Hint:    hint,
	})
}

// upstreamHTTPErrorResult is the canonical wrapper for "the
// dispatcher made an HTTP call that returned a non-2xx" cases
// outside the main executeRequest/packageHTTPResponse pipeline
// (in-handler prefetches, link create/delete, project dispatchers,
// etc.). Routes the failure through classifyHTTPStatusKind so the
// shape matches the main pipeline exactly — agents see the same
// envelope regardless of which dispatcher path produced it.
func upstreamHTTPErrorResult(
	ctx context.Context,
	cmdKey, op, route string,
	status int,
	body []byte,
	lookup WorkspaceLister,
	kind ResourceKind,
	refOrSlug string,
) *mcp.CallToolResult {
	res := classifyHTTPStatusKind(ctx, cmdKey, route, status, body, lookup, kind, refOrSlug)
	// Layer the per-call op verb into the message so a "prefetch"
	// failure surfaces distinct from a top-level "execute" failure.
	env := envelopeFrom(res)
	if op != "" && env.Error.Message != "" {
		env.Error.Message = fmt.Sprintf("%s (%s)", env.Error.Message, op)
	}
	return NewErrorResult(env.Error)
}
