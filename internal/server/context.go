package server

import (
	"context"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Exported context-key helpers for callers that need to synthesize an
// authenticated request without going through the auth middleware
// chain (e.g. the in-process MCP HTTP-handler dispatcher in
// internal/mcp/dispatch_http.go, which resolves OAuth users via the
// MCP middleware and then calls the API handler tree directly).
//
// The corresponding context keys (ctxCurrentUser, ctxIsAPIToken) are
// kept package-private so the rest of the codebase can't bypass the
// middleware accidentally — these functions are the controlled surface.

// WithCurrentUser returns ctx decorated with the resolved user, the
// same way TokenAuth / SessionAuth do during a normal authenticated
// request. Subsequent handler-tree code can reach the user via the
// existing currentUser(r) helper without any change.
//
// Pass nil to clear (rare; mostly useful in tests).
func WithCurrentUser(ctx context.Context, user *models.User) context.Context {
	return context.WithValue(ctx, ctxCurrentUser, user)
}

// WithAPITokenAuth marks ctx as authenticated via an API token, the
// same way TokenAuth does on the Bearer-token path. Required when
// dispatching through the in-process handler from the OAuth-protected
// /mcp endpoint, because the auth middleware's downstream checks
// (e.g. CSRF exemption for token-auth callers) read ctxIsAPIToken to
// distinguish from session-cookie traffic.
func WithAPITokenAuth(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxIsAPIToken, true)
}

// WithTokenWorkspaceID returns ctx decorated with a workspace-scope
// hint, mirroring TokenAuth's behaviour for workspace-scoped API
// tokens. Pass an empty string to clear (overrides any previous
// scope-binding to ""; downstream readers via tokenWorkspaceID(r)
// see the same "no scope" they would for a never-set context).
//
// The MCP dispatcher uses this to forward the OAuth-token's allowed
// workspace to the handler tree where existing access-control logic
// reads it via tokenWorkspaceID(r).
func WithTokenWorkspaceID(ctx context.Context, workspaceID string) context.Context {
	// Always overwrite — passing "" must clear a stale scope set
	// further up the context chain. Returning ctx unchanged on the
	// empty path was a bug Codex caught in PR #343 review round 4.
	return context.WithValue(ctx, ctxTokenWorkspaceID, workspaceID)
}

// CurrentUserFromContext returns the user attached by WithCurrentUser
// (or by the standard auth middleware), and a boolean signalling
// whether one was present. Read-only accessor — callers that need to
// set the user must use WithCurrentUser.
//
// Exported so out-of-package callers (notably internal/mcp's HTTP
// dispatcher tests) can verify the user round-trips correctly through
// the synthesized request without reaching into private helpers.
func CurrentUserFromContext(ctx context.Context) (*models.User, bool) {
	v, ok := ctx.Value(ctxCurrentUser).(*models.User)
	return v, ok && v != nil
}

// IsAPITokenFromContext reports whether the request was authenticated
// via an API token (vs. a session cookie or CLI session token).
// Mirrors the package-private isAPIToken(r) helper.
func IsAPITokenFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(ctxIsAPIToken).(bool)
	return v
}

// WithTokenScopes returns ctx decorated with the API token's
// JSON-encoded scopes string (e.g. `["read"]`, `["*"]`). Stashed by
// the MCP Bearer middleware so the in-process dispatcher
// (internal/mcp/dispatch_http.go) can enforce per-tool scope checks
// — the dispatcher bypasses the standard TokenAuth chain by setting
// WithCurrentUser directly, so the chain-level scope check at
// middleware_auth.go:TokenAuth doesn't run for synthesized requests.
//
// Without this, a PAT with scope `["read"]` could drive write MCP
// tools because the in-process request looks pre-authenticated to
// the handler tree. Codex review #369 round 1 flagged the gap.
//
// Empty string clears any previously set scope.
func WithTokenScopes(ctx context.Context, scopes string) context.Context {
	return context.WithValue(ctx, ctxTokenScopes, scopes)
}

// TokenScopesFromContext returns the JSON-encoded scopes attached by
// WithTokenScopes, or "" if none. Empty maps to "unrestricted" in
// TokenScopeAllows (the legacy behaviour) — callers wanting
// strict-deny on the unset path must check the empty-string branch
// before passing it on.
func TokenScopesFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxTokenScopes).(string)
	return v
}

// TokenScopeAllows is the exported wrapper around the package-private
// tokenScopeAllows: returns true iff the scopes JSON permits the given
// HTTP method against path. Public so internal/mcp/dispatch_http.go
// can re-check scopes on each synthesized tool call without
// reimplementing the policy.
//
// Policy summary (see tokenScopeAllows for the full doc):
//
//   - "" or `["*"]`        → allow all methods (legacy / explicit wildcard).
//   - `["read"]`           → allow GET / HEAD / OPTIONS only.
//   - `["write"]`          → allow all methods.
//   - explicit `[]`        → allow all methods (legacy unrestricted form).
//   - unparseable / null   → deny.
//   - unknown scope only   → deny (policy-relevant unknowns are logged).
//
// Caller is expected to be the in-process MCP dispatcher; the
// chain-level enforcement on /api/v1/* still runs through tokenScopeAllows
// directly.
func TokenScopeAllows(scopesJSON, method, path string) bool {
	return tokenScopeAllows(scopesJSON, method, path)
}

// WithTokenAllowedWorkspaces returns ctx decorated with the OAuth
// token's workspace allow-list set at consent time (TASK-952). The
// list is either a set of slugs (the user's specific selection) or
// `["*"]` (the wildcard checkbox). MCPBearerAuth's OAuth path stashes
// this on every request so RequireWorkspaceAccess can gate the
// resolved workspace against the allow-list (TASK-953) before
// running the standard membership check.
//
// nil clears any previously set allow-list — distinct from setting
// an empty slice, which would deny every workspace. PAT auth never
// calls this; the helper exists for the OAuth path only.
//
// Exported so the in-process MCP dispatcher can forward the same
// allow-list onto synthesized requests via Apply, matching the
// pattern WithTokenScopes uses (sub-PR E TASK-1027 round 1).
func WithTokenAllowedWorkspaces(ctx context.Context, slugs []string) context.Context {
	if slugs == nil {
		return context.WithValue(ctx, ctxTokenAllowedWorkspaces, []string(nil))
	}
	// Defensive copy — caller mutating after the call must not
	// corrupt the per-request token state.
	cp := make([]string, len(slugs))
	copy(cp, slugs)
	return context.WithValue(ctx, ctxTokenAllowedWorkspaces, cp)
}

// TokenAllowedWorkspacesFromContext returns the token's workspace
// allow-list, or nil if none was attached. Three return shapes
// matter to callers:
//
//   - nil — no allow-list set (PAT auth, or pre-TASK-952 OAuth
//     tokens). Caller should NOT apply any token-level gate; rely
//     on standard membership checks.
//   - []string{"*"} — wildcard. Caller should not apply per-slug
//     gating; standard membership applies.
//   - []string{"slug-a", ...} — explicit allow-list. Caller MUST
//     deny any workspace not in this set, even if the user is a
//     member of it.
//
// Returns a copy of the stored slice — callers can mutate without
// risk to the per-request context value.
func TokenAllowedWorkspacesFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(ctxTokenAllowedWorkspaces).([]string)
	if v == nil {
		return nil
	}
	out := make([]string, len(v))
	copy(out, v)
	return out
}
