package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// MCPBearerAuth is the auth gate for the /mcp Streamable HTTP endpoint.
//
// Behaviour, in order:
//
//  1. No Authorization header, or one that doesn't start with "Bearer " →
//     401 with WWW-Authenticate per RFC 9728. The header points the
//     client at our protected-resource discovery doc so it can begin
//     the OAuth flow.
//  2. Bearer token present but not a recognized format / not in the
//     api_tokens table / expired → 401 with the same WWW-Authenticate
//     header (so a stale token doesn't drop the client into a "token
//     unrecognized" dead end — they re-discover and recover).
//  3. Valid token → user attached to context via WithCurrentUser; next
//     handler runs.
//
// Note for TASK-951:
//
// This middleware is the single integration point for OAuth-issued
// tokens. The PAT-first cut shipping with TASK-950 calls
// store.ValidateToken (the existing pad_*-prefixed PAT path); when
// TASK-951 lands, this function gains a branch that tries OAuth
// introspection first (RFC 7662 lookup against the auth-server's
// /oauth/introspect endpoint) and falls back to PAT validation. The
// 401 + WWW-Authenticate envelope below stays identical — only the
// token-validation step changes. Keeping the auth path centralized
// here means TASK-951 doesn't need to touch handlers_mcp.go.
//
// Why a dedicated middleware (not the existing TokenAuth):
//
// TokenAuth on /api/v1/* writes a JSON error envelope on 401 (so the
// SPA / CLI clients can render a friendly message) and never sets
// WWW-Authenticate. MCP clients expect the spec-shape: 401 with the
// WWW-Authenticate "resource_metadata" parameter pointing them at
// our discovery doc (RFC 9728 §5.3, MCP authorization spec
// 2025-11-25). Wrapping TokenAuth would mean rewriting its 401
// responses post-hoc — a layered hack. A standalone middleware is
// shorter and clearer.
//
// CSRF / rate limiting:
//
// /mcp uses Bearer auth (Authorization header), not session cookies,
// so CSRF doesn't apply (CSRF defends cookie-bearing requests). Rate
// limiting is wired separately via TASK-959; this middleware is auth
// only, intentionally.
func (s *Server) MCPBearerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := extractBearer(r.Header.Get("Authorization"))
		if !ok {
			s.writeMCPUnauthorized(w, r, "missing_token", "Bearer token required.")
			return
		}

		// Format gate matching TokenAuth's check (middleware_auth.go:103).
		// Rejecting at the prefix avoids a DB lookup for obviously-
		// malformed tokens — a small but meaningful win under any
		// volume of unauth probes.
		if !strings.HasPrefix(token, "pad_") || len(token) != 68 {
			s.writeMCPUnauthorized(w, r, "invalid_token", "Token format not recognized.")
			return
		}

		apiToken, err := s.store.ValidateToken(token)
		if err != nil {
			// A DB error during token validation is a server-side
			// problem, not the client's fault. 500 (not 401) so MCP
			// clients don't churn through reconnect loops on a backend
			// outage. No WWW-Authenticate header — re-running
			// discovery wouldn't help.
			writeError(w, http.StatusInternalServerError, "internal_error", "Token validation failed.")
			return
		}
		if apiToken == nil {
			s.writeMCPUnauthorized(w, r, "invalid_token", "Token is invalid or expired.")
			return
		}

		// Resolve the user. Workspace-scope binding (apiToken.WorkspaceID)
		// is intentionally NOT pinned here for user-owned PATs — the
		// downstream RequireWorkspaceAccess middleware (when handlers
		// run in-process via HTTPHandlerDispatcher) checks
		// GetWorkspaceMember instead, matching the v0.2 design where
		// a PAT grants access to whichever workspace its owning user
		// belongs to. TASK-953 introduces a per-token allowed_workspaces[]
		// allow-list which IS enforced here once it lands; for now,
		// membership is the gate.
		if apiToken.UserID == "" {
			// Legacy workspace-scoped tokens (no user_id) predate the
			// user-token refactor. They still authenticate the existing
			// API surface — see handlers_events.go:52 — but the MCP
			// transport requires a user identity (every audit log entry,
			// every "who created this item", etc.). Reject cleanly.
			s.writeMCPUnauthorized(w, r, "invalid_token", "MCP requires a user-owned token. Legacy workspace-scoped tokens are not supported here.")
			return
		}
		user, err := s.store.GetUser(apiToken.UserID)
		if err != nil || user == nil {
			s.writeMCPUnauthorized(w, r, "invalid_token", "Token references an unknown user.")
			return
		}

		ctx := WithCurrentUser(r.Context(), user)
		// Mirror TokenAuth's ctxIsAPIToken signal so downstream
		// handlers that distinguish session vs token auth see the same
		// shape they would on /api/v1/*. Cheap, future-proof.
		ctx = context.WithValue(ctx, ctxIsAPIToken, true)
		// Stash the token's scopes so the in-process MCP dispatcher
		// (internal/mcp/dispatch_http.go) can enforce per-tool scope
		// checks. Without this, a PAT with `["read"]` scope can drive
		// write MCP tools because the synthesized in-process request
		// looks pre-authenticated to the handler tree, bypassing
		// TokenAuth's chain-level scopeAllows check. Codex review
		// #369 round 1.
		ctx = WithTokenScopes(ctx, apiToken.Scopes)

		// Surface near-expiry warning headers the same way TokenAuth
		// does (middleware_auth.go:125). MCP clients can read them,
		// but more importantly: the existing handlers_auth.go logic
		// expects the warning to fire consistently regardless of
		// transport.
		setTokenExpiryWarning(w, apiToken)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// extractBearer parses an Authorization header value. Returns the
// token and true on success. Anything that isn't "Bearer <token>"
// (case-insensitive scheme, single space, non-empty token) returns
// "", false. Permissive on the scheme casing because RFC 6750 §2.1
// says it's case-insensitive; strict on the single-space separator
// to match the actual wire format mainstream clients send.
func extractBearer(h string) (string, bool) {
	if h == "" {
		return "", false
	}
	const prefix = "Bearer "
	if len(h) <= len(prefix) {
		return "", false
	}
	if !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// writeMCPUnauthorized emits the spec-shaped 401 every MCP client
// expects: WWW-Authenticate with realm + resource_metadata, plus a
// small JSON body so curl / log scrapers see the reason without
// needing to parse the header.
//
// resource_metadata points at the same URL handleOAuthProtectedResource
// serves — Claude Desktop, Cursor, etc. follow it to begin the OAuth
// discovery flow described in the MCP authorization spec.
//
// URL resolution for the resource_metadata parameter:
//
//  1. s.mcpPublicURL (set by SetMCPTransport from PAD_MCP_PUBLIC_URL).
//     The canonical case — production deployments always set this.
//  2. Fallback: derive from the request's Host header with "https://"
//     prefix. Matches handleOAuthProtectedResource's fallback so the
//     two URLs stay in sync for local dev without env vars set.
//
// Codex review #369 round 1 caught a regression where the fallback
// path dropped the WWW-Authenticate header entirely — that broke MCP
// client discovery on cloud-mode-without-PAD_MCP_PUBLIC_URL deploys
// because fresh clients rely on the header to find the metadata doc.
func (s *Server) writeMCPUnauthorized(w http.ResponseWriter, r *http.Request, code, msg string) {
	resourceBase := strings.TrimRight(s.mcpPublicURL, "/")
	if resourceBase == "" && r != nil && r.Host != "" {
		// Same fallback as handleOAuthProtectedResource — assume HTTPS
		// because RFC 9728 §3 + MCP authorization spec both require
		// HTTPS in production, and the test harness doesn't probe the
		// scheme. Operators on dev hosts running plain HTTP will see
		// "https://localhost:7777/..." in the header; the test rig
		// already pins the canonical case via SetMCPTransport.
		resourceBase = "https://" + r.Host
	}
	if resourceBase == "" {
		// No request and no configured URL — extremely rare (would
		// only fire if writeMCPUnauthorized is called from a path
		// that synthesizes a 401 without a request, which today none
		// do). Fall back to the plain JSON 401 so the response is
		// still well-formed; agents will get a generic "unauthorized"
		// rather than a discovery-pointing one.
		writeError(w, http.StatusUnauthorized, code, msg)
		return
	}
	resourceMeta := resourceBase + "/.well-known/oauth-protected-resource"
	w.Header().Set("WWW-Authenticate", `Bearer realm="pad", resource_metadata="`+resourceMeta+`"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": msg,
		},
	})
}
