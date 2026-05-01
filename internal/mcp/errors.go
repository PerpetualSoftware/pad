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
	// Future enhancement: populate `available_collections` or recent
	// items as a hint (deferred to TASK-977).
	ErrItemNotFound ErrorCode = "item_not_found"

	// ErrValidationFailed fires on bad input — required field missing,
	// enum value out of range, malformed JSON. HTTP: 422.
	ErrValidationFailed ErrorCode = "validation_failed"

	// ErrConflict fires when an operation collides with concurrent
	// state (e.g. version mismatch on update). HTTP: 409.
	ErrConflict ErrorCode = "conflict"

	// ErrServerError is the catch-all for unexpected failures —
	// 5xx from HTTP, unknown stderr patterns from exec. The wrapped
	// message preserves the underlying detail for debugging without
	// promising any structured shape.
	ErrServerError ErrorCode = "server_error"
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
// Same available_workspaces enrichment as no_workspace.
func unknownWorkspaceResult(ctx context.Context, slug string, lookup WorkspaceLister) *mcp.CallToolResult {
	hints := bestEffortWorkspaceHints(ctx, lookup)
	return NewErrorResult(ErrorPayload{
		Code:                ErrUnknownWorkspace,
		Message:             fmt.Sprintf("Workspace %q is not visible to this session.", slug),
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

// classifyExecError turns an exec.Cmd failure (err + stderr) into a
// structured envelope. lookup is optional — when supplied, no_workspace
// errors get available_workspaces enrichment.
func classifyExecError(ctx context.Context, cmdPath []string, runErr error, stderr string, lookup WorkspaceLister) *mcp.CallToolResult {
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
			Hint:    stderr,
		})
	case execStderrMatchesPermissionDenied(lower):
		return NewErrorResult(ErrorPayload{
			Code:    ErrPermissionDenied,
			Message: "Permission denied for this operation.",
			Hint:    stderr,
		})
	case execStderrMatchesItemNotFound(lower):
		return NewErrorResult(ErrorPayload{
			Code:    ErrItemNotFound,
			Message: "Item not found.",
			Hint:    stderr,
		})
	case execStderrMatchesValidation(lower):
		return NewErrorResult(ErrorPayload{
			Code:    ErrValidationFailed,
			Message: "Validation failed.",
			Hint:    stderr,
		})
	}

	// Fallback: unstructured server error. Preserve the original
	// "pad <cmd> failed: <stderr>" shape so any agent that special-
	// cased the old text still has something to read.
	msg := stderr
	if msg == "" && runErr != nil {
		msg = runErr.Error()
	}
	if msg == "" {
		msg = "unknown error"
	}
	cmd := strings.Join(cmdPath, " ")
	return NewErrorResult(ErrorPayload{
		Code:    ErrServerError,
		Message: fmt.Sprintf("pad %s failed: %s", cmd, msg),
	})
}

// Stderr-pattern matchers. Compiled at init for cost-free
// classification. Patterns are case-insensitive against `lower`
// (the caller pre-lowercases for performance).
var (
	reNoWorkspace        = regexp.MustCompile(`no workspace.*(linked|configured)`)
	reUnknownWorkspaceA  = regexp.MustCompile(`workspace .* (does not exist|not found)`)
	reUnknownWorkspaceB  = regexp.MustCompile(`unknown workspace`)
	reAuthRequired       = regexp.MustCompile(`(not authenticated|authentication required|please log in|run pad auth login|invalid token|expired token)`)
	rePermissionDenied   = regexp.MustCompile(`(permission denied|forbidden|insufficient (permissions|role))`)
	reItemNotFound       = regexp.MustCompile(`(item.*not found|no such item|unknown ref)`)
	reValidationFailed   = regexp.MustCompile(`(invalid|missing required|must be one of|validation)`)
	reUnknownWorkspaceID = regexp.MustCompile(`workspace ['"]?([a-z0-9][a-z0-9-]*)['"]?`)
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

// ─────────────────────────────────────────────────────────────────────
// HTTPHandlerDispatcher classification
//
// HTTP status codes map cleanly to the taxonomy. Body parsing is
// best-effort: when the handler returns a structured payload we
// surface its message; otherwise the status text serves as the
// envelope's Message.
// ─────────────────────────────────────────────────────────────────────

// classifyHTTPStatus turns an HTTP error response into a structured
// envelope. body is the raw response body (may be empty); cmdKey is
// the dotted command path for debugging context. lookup is optional
// — supply the dispatcher's WorkspaceLister so 404 (workspace) /
// 403 / etc. can populate available_workspaces when relevant.
func classifyHTTPStatus(ctx context.Context, cmdKey string, status int, body []byte, lookup WorkspaceLister) *mcp.CallToolResult {
	bodyText := strings.TrimSpace(string(body))
	if bodyText == "" {
		bodyText = http.StatusText(status)
	}

	switch status {
	case http.StatusUnauthorized:
		return NewErrorResult(ErrorPayload{
			Code:    ErrAuthRequired,
			Message: "Authentication required.",
			Hint:    bodyText,
		})
	case http.StatusForbidden:
		return NewErrorResult(ErrorPayload{
			Code:    ErrPermissionDenied,
			Message: "Permission denied for this operation.",
			Hint:    bodyText,
		})
	case http.StatusNotFound:
		// Without inspecting the URL we can't tell workspace-404 vs
		// item-404 with certainty; the body usually says. Default to
		// item_not_found and let TASK-977 refine when the remote
		// transport can provide URL-aware classification.
		if strings.Contains(strings.ToLower(bodyText), "workspace") {
			// Try to pull the slug out of the body so the message
			// names it (e.g. body="workspace 'foo' not visible" →
			// "Workspace \"foo\" is not visible..."). Falls back to
			// the body in Hint when no slug parses.
			slug := extractUnknownWorkspaceSlug(bodyText)
			res := unknownWorkspaceResult(ctx, slug, lookup)
			// unknownWorkspaceResult uses the available_workspaces
			// hint line; preserve the original handler body in Hint
			// so debug detail isn't lost. Concatenate when both
			// exist so neither hides the other.
			env := envelopeFrom(res)
			if env.Error.Hint == "" {
				env.Error.Hint = bodyText
			} else {
				env.Error.Hint = bodyText + " — " + env.Error.Hint
			}
			return NewErrorResult(env.Error)
		}
		return NewErrorResult(ErrorPayload{
			Code:    ErrItemNotFound,
			Message: "Item not found.",
			Hint:    bodyText,
		})
	case http.StatusConflict:
		return NewErrorResult(ErrorPayload{
			Code:    ErrConflict,
			Message: "Conflict — current state changed beneath this update.",
			Hint:    bodyText,
		})
	case http.StatusUnprocessableEntity, http.StatusBadRequest:
		return NewErrorResult(ErrorPayload{
			Code:    ErrValidationFailed,
			Message: "Validation failed.",
			Hint:    bodyText,
		})
	}

	if status >= 500 {
		return NewErrorResult(ErrorPayload{
			Code:    ErrServerError,
			Message: fmt.Sprintf("pad %s failed: %s", cmdKey, bodyText),
		})
	}
	// Other 4xx without a specific mapping — surface as server_error
	// with the raw body so debugging is still possible. Avoid silently
	// promoting them to validation_failed; that would mislead callers.
	return NewErrorResult(ErrorPayload{
		Code:    ErrServerError,
		Message: fmt.Sprintf("pad %s failed (HTTP %d): %s", cmdKey, status, bodyText),
	})
}
