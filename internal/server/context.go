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
