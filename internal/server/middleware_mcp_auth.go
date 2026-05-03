package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ory/fosite"

	"github.com/PerpetualSoftware/pad/internal/oauth"
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
// Two token paths (sub-PR E, TASK-1027):
//
//   - PAT (`pad_<60-hex-chars>`) → store.ValidateToken. The original
//     TASK-950 path; PATs predate the OAuth server and continue to
//     work as a developer / CLI escape hatch.
//   - OAuth opaque (anything else) → s.oauthServer.IntrospectToken.
//     fosite-issued from the /oauth/token flow that sub-PRs A-D
//     wired. RFC 8707 audience binding is enforced here so a token
//     issued for a different resource can't replay against /mcp.
//
// The branch is chosen on token shape, not on header content: the
// PAT prefix-and-length check is cheap and unambiguous (no real
// OAuth opaque token starts with `pad_` because fosite uses base64
// of the HMAC, never that prefix). PATs that don't validate fall
// through to the same 401 envelope the OAuth path uses — clients
// re-running discovery will see the same WWW-Authenticate pointer
// either way.
//
// Why introspect server-side rather than via the public /oauth/introspect
// HTTP endpoint: pad-cloud is the resource server AND the auth
// server, so a roundtrip would be pointless overhead. Calling
// fosite's IntrospectToken directly skips HTTP parsing, client-auth
// negotiation, and JSON encoding/decoding — and doesn't require
// minting a separate "introspection bearer" for the resource server.
// The public HTTP endpoint exists for spec-compliant external
// clients but isn't on the hot path here.
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
		// Capture the entry timestamp so denied-but-resolved requests
		// (e.g. valid bearer rate-limited at the per-token bucket) can
		// still emit an audit row with real latency. The audit row
		// emit path lives in middleware_mcp_audit.go's
		// emitMCPAuditDenied — see that helper for the rationale on
		// why we audit those branches directly here rather than from
		// the wrapping middleware (Codex review on PR #389 round 1).
		mcpAuthStart := time.Now().UTC()

		token, ok := extractBearer(r.Header.Get("Authorization"))
		if !ok {
			s.writeMCPUnauthorized(w, r, "missing_token", "Bearer token required.")
			return
		}

		// PAT path: prefix `pad_` + 68 chars total (4 prefix + 64 hex
		// secret). Cheap shape gate before the DB lookup. Anything
		// else falls into the OAuth introspection branch.
		if strings.HasPrefix(token, "pad_") && len(token) == 68 {
			s.handleMCPPATAuth(w, r, token, next, mcpAuthStart)
			return
		}

		// OAuth introspection path. Requires sub-PR B's NewServer
		// + sub-PRs A/C/D for the storage and flow endpoints to be
		// wired. Cloud-mode-without-OAuth deployments (rare:
		// PAD_MCP_PUBLIC_URL set but no encryption-key wiring) get a
		// clean reject rather than a confusing fall-through.
		if s.oauthServer == nil {
			s.writeMCPUnauthorized(w, r, "invalid_token", "Token format not recognized.")
			return
		}
		s.handleMCPOAuthAuth(w, r, token, next, mcpAuthStart)
	})
}

// handleMCPPATAuth runs the original TASK-950 PAT validation path.
// Extracted from MCPBearerAuth so the OAuth branch reads cleanly
// without a deeply-nested if/else; behaviour identical to the
// pre-sub-PR-E single-path version.
func (s *Server) handleMCPPATAuth(w http.ResponseWriter, r *http.Request, token string, next http.Handler, mcpAuthStart time.Time) {
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

	// Per-token rate limit (TASK-959). Runs AFTER ALL PAT validation
	// gates pass: ValidateToken, the legacy-workspace-scoped guard,
	// and the GetUser lookup. Codex review #378 round 3 caught the
	// gap where rate-limiting between ValidateToken and these later
	// gates would create limiter buckets for active-but-not-
	// authorized tokens (legacy workspace-scoped tokens with no
	// UserID, tokens whose user row was deleted between issuance
	// and use). Symmetric to the OAuth path's positioning — both
	// paths run the rate limit at the very end of their happy path.
	if !s.checkMCPRateLimit(w, r, token) {
		// Resolved user + token but rate-limited: emit an audit row
		// directly. The wrapping MCPAuditLog never sees this branch
		// because we return before next.ServeHTTP. Codex review on
		// PR #389 round 1 — see emitMCPAuditDenied for the design
		// rationale.
		s.emitMCPAuditDenied(r, user, "pat", apiToken.ID, "rate_limited", mcpAuthStart)
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
	// Stash the token identity so the audit middleware (TASK-960)
	// can record which PAT drove the call. Audit logging is
	// independent of every other gate above and below — even calls
	// that the inner handler later rejects produce a row.
	ctx = WithMCPTokenIdentity(ctx, "pat", apiToken.ID)

	// Surface near-expiry warning headers the same way TokenAuth
	// does (middleware_auth.go:125). MCP clients can read them,
	// but more importantly: the existing handlers_auth.go logic
	// expects the warning to fire consistently regardless of
	// transport.
	setTokenExpiryWarning(w, apiToken)

	next.ServeHTTP(w, r.WithContext(ctx))
}

// handleMCPOAuthAuth runs introspection against the OAuth server
// for any non-PAT bearer. Validates:
//
//   - Token is recognized (storage lookup) and active.
//   - Token kind is access_token (refresh tokens can't authorize a
//     resource call — RFC 6749 §1.5 calls them "credentials used to
//     obtain access tokens," not bearers themselves).
//   - Granted audience contains the canonical MCP URL (RFC 8707
//     anti-replay). A token issued for a different resource MUST
//     NOT pass even if otherwise valid.
//   - Subject is set + resolves to a real user row.
//
// On success, attaches user + scopes to context exactly like the
// PAT path. The dispatcher's per-tool scope check
// (TokenScopeAllows from middleware_auth.go) recognizes the OAuth
// scope vocabulary (pad:read / pad:write / pad:admin) alongside
// the legacy PAT vocabulary, so MCP tools see one uniform policy
// regardless of which transport issued the bearer.
func (s *Server) handleMCPOAuthAuth(w http.ResponseWriter, r *http.Request, token string, next http.Handler, mcpAuthStart time.Time) {
	ar, tokenUse, err := s.oauthServer.IntrospectToken(r.Context(), token)
	if err != nil {
		// fosite returns ErrInactiveToken / ErrNotFound for unknown
		// or revoked tokens; ErrInvalidTokenFormat for "not even an
		// HMAC value." All of those collapse to the same 401 here —
		// we don't need to distinguish for the client (re-running
		// discovery + re-auth handles every recovery path).
		// Storage errors (DB outage, etc.) ALSO collapse here rather
		// than to 500: from a security standpoint we can't validate
		// the token, so the client must NOT proceed; from a client
		// UX standpoint a transient 401 + retry is no worse than a
		// 500 + retry, and the spec-shape WWW-Authenticate keeps
		// discovery agents on rails.
		if !errors.Is(err, fosite.ErrInactiveToken) && !errors.Is(err, fosite.ErrNotFound) {
			// Log non-validation errors so ops can spot DB / config
			// problems via existing alerting.
			slog.Warn("oauth introspect failed", "error", err)
		}
		s.writeMCPUnauthorized(w, r, "invalid_token", "Token is invalid or expired.")
		return
	}
	if ar == nil {
		// Defensive: fosite's contract says either err or ar is set,
		// but a mocked dispatcher could violate that. 401 on the
		// safe side.
		s.writeMCPUnauthorized(w, r, "invalid_token", "Token is invalid or expired.")
		return
	}

	// Refresh tokens are NOT valid bearers for resource calls. RFC
	// 6749 §1.5: "refresh tokens are credentials used to obtain
	// access tokens." A client misconfigured to send the refresh
	// token in the Authorization header would otherwise look
	// authenticated — reject explicitly.
	if tokenUse != fosite.AccessToken {
		s.writeMCPUnauthorized(w, r, "invalid_token", "Refresh tokens cannot authorize MCP requests; use the access token.")
		return
	}

	// RFC 8707 audience binding (anti-replay). The canonical
	// audience is what /authorize/decide grants on every token
	// (audience.go's audienceMatchingStrategy enforces it on the
	// grant side); this check is the corresponding resource-server
	// gate. Without it, a token issued for resource=https://other.example
	// would silently authorize on /mcp because we have no other
	// boundary that distinguishes resources. Belt-and-suspenders:
	// the grant-side check should already prevent foreign audiences,
	// but the resource server validating its own incoming tokens is
	// the spec's primary defense.
	canonical := s.oauthServer.AllowedAudience()
	if canonical == "" {
		// Misconfigured server — no canonical audience to check
		// against. Fail-closed: refuse the token rather than
		// fall through to "any audience is fine."
		slog.Error("oauth server has no canonical audience; refusing all OAuth tokens at /mcp")
		s.writeMCPUnauthorized(w, r, "invalid_token", "OAuth server is misconfigured.")
		return
	}
	if !audienceContains(ar.GetGrantedAudience(), canonical) {
		s.writeMCPUnauthorized(w, r, "invalid_token", "Token audience does not match this resource.")
		return
	}

	// Subject is the pad user ID, set in /authorize/decide via
	// oauth.NewSession(user.ID). An empty subject would mean the
	// grant flow shipped without a user identity, which sub-PR C's
	// renderConsentStub guards against (only logged-in users reach
	// /authorize/decide). Defense in depth: reject anyway.
	session := ar.GetSession()
	if session == nil {
		s.writeMCPUnauthorized(w, r, "invalid_token", "Token has no session.")
		return
	}
	userID := session.GetSubject()
	if userID == "" {
		s.writeMCPUnauthorized(w, r, "invalid_token", "Token has no subject.")
		return
	}
	user, err := s.store.GetUser(userID)
	if err != nil || user == nil {
		// User was deleted between grant and use, OR the storage
		// layer is failing. Either way, the bearer can't represent
		// a valid identity — reject.
		s.writeMCPUnauthorized(w, r, "invalid_token", "Token references an unknown user.")
		return
	}

	// Per-token rate limit (TASK-959). Runs AFTER ALL OAuth
	// validation gates pass: introspection, refresh-vs-access
	// check, RFC 8707 audience match, subject presence, user
	// lookup. Codex review #378 round 2 caught the gap where
	// rate-limiting between IntrospectToken and these later gates
	// would create limiter buckets for active-but-not-authorized
	// tokens (refresh tokens used as bearers, wrong-audience
	// tokens, deleted users). Moving the check to the very end
	// of the OAuth happy path ensures the limiter map only
	// contains tokens that would otherwise reach next.ServeHTTP.
	if !s.checkMCPRateLimit(w, r, token) {
		// Resolved user + OAuth connection but rate-limited: emit
		// an audit row directly. Wrapping MCPAuditLog never sees
		// this branch because we return before next.ServeHTTP.
		// Codex review on PR #389 round 1.
		s.emitMCPAuditDenied(r, user, "oauth", ar.GetID(), "rate_limited", mcpAuthStart)
		return
	}

	ctx := WithCurrentUser(r.Context(), user)
	ctx = context.WithValue(ctx, ctxIsAPIToken, true)
	// Translate fosite's space-separated scope string into the
	// JSON-array form the existing scope-policy check expects.
	// tokenScopeAllows recognizes pad:read / pad:write / pad:admin
	// alongside the legacy PAT scopes, so the same per-method
	// policy applies to OAuth-issued tokens.
	ctx = WithTokenScopes(ctx, oauthScopesToJSON(ar.GetGrantedScopes()))
	// Stash the OAuth connection identity (request_id chain) for
	// the audit middleware (TASK-960). request_id is preserved
	// across refresh-token rotations, so it's the stable identifier
	// for a single user-authorized connection — exactly what the
	// connected-apps page (TASK-954) keys revoke + last-used on.
	ctx = WithMCPTokenIdentity(ctx, "oauth", ar.GetID())

	// Stash the workspace allow-list set at consent time (TASK-952)
	// so RequireWorkspaceAccess can gate workspace access per-token
	// (TASK-953). Reading from session.Extra goes through the typed
	// AllowedWorkspaces accessor so JSON round-trip handling
	// ([]interface{} vs []string) is centralized in oauth.Session.
	//
	// Three shapes arrive here:
	//   - nil — pre-TASK-952 token (no consent payload). Treated by
	//     RequireWorkspaceAccess as "no token-level gate"; standard
	//     membership applies.
	//   - ["*"] — wildcard. Same effective behaviour as nil for the
	//     gate: any workspace the user is a member of is allowed.
	//   - [slug-a, slug-b, ...] — explicit allow-list. Workspaces
	//     NOT in this set are rejected before the membership check
	//     even runs.
	if oauthSession, ok := session.(*oauth.Session); ok {
		if allowed := oauthSession.AllowedWorkspaces(); allowed != nil {
			ctx = WithTokenAllowedWorkspaces(ctx, allowed)
		}
	}

	next.ServeHTTP(w, r.WithContext(ctx))
}

// audienceContains reports whether haystack contains needle, treating
// scheme-equivalent forms as equal via oauth.NormalizeAudience.
//
// Why normalize: real OAuth clients reconstruct the resource indicator
// from the URL the user pasted, and URL parsing canonicalizes empty
// path → "/" — so a client given `https://mcp.example` may emit
// `https://mcp.example/` as the audience. RFC 3986 §6.2.3 declares
// these equivalent for HTTP scheme; pad's AS-side audienceMatchingStrategy
// applies the same trim. Mirroring the rule here keeps the AS and RS
// in lockstep — without it, tokens the AS minted (with the requested
// audience stored verbatim) would fail validation at /mcp.
func audienceContains(haystack []string, needle string) bool {
	needleNorm := oauth.NormalizeAudience(needle)
	for _, a := range haystack {
		if oauth.NormalizeAudience(a) == needleNorm {
			return true
		}
	}
	return false
}

// oauthScopesToJSON serializes fosite's granted-scope slice to the
// JSON array form tokenScopeAllows expects.
//
// Crucial fail-closed: nil / empty granted scopes map to JSON `null`
// (which tokenScopeAllows denies via the "scopes == nil" branch),
// NOT to `[]` (which the same function would accept as the legacy
// "unrestricted" PAT form). Codex review #375 round 1 caught the
// bug: OAuth's `scope` parameter is optional per RFC 6749 §3.3, so
// a client can run the auth-code flow without requesting any scopes
// — fosite hands back a token with no granted scopes, and without
// this guard MCPBearerAuth would treat that token as unrestricted
// (because `[]` is the legacy PAT shape). Mapping empty OAuth
// scopes to `null` instead routes them through the deny path.
//
// Note: in the production setup, sub-PR C's handleOAuthRegister
// defaults a registered client's scope set to `pad:read pad:write`
// when DCR omits it, and audienceMatchingStrategy refuses any
// authorize-side request with an unrecognized audience. So the
// "empty granted scopes" path is hard to hit through the public
// endpoints — but defense-in-depth at the resource server is the
// right policy regardless.
//
// json.Marshal can't fail on a []string, so we ignore the error
// and the result string is always valid JSON.
func oauthScopesToJSON(scopes []string) string {
	if len(scopes) == 0 {
		// Fail closed: route empty OAuth scopes through
		// tokenScopeAllows's deny path, NOT the legacy `[]`
		// unrestricted-PAT path.
		return "null"
	}
	b, _ := json.Marshal(scopes)
	return string(b)
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
