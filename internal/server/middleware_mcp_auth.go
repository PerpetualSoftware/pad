package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ory/fosite"

	"github.com/PerpetualSoftware/pad/internal/oauth"
	"github.com/PerpetualSoftware/pad/internal/store"
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
	// A bot's credential is no person's (TASK-3392): unknown, like a gone one.
	if err != nil || user == nil || user.IsApp() {
		s.writeMCPUnauthorized(w, r, "invalid_token", "Token references an unknown user.")
		return
	}
	// A disabled account's credential ends here too (BUG-3349): disabling
	// revokes its tokens, and this holds for one minted in between.
	if user.IsDisabled() {
		s.writeMCPUnauthorized(w, r, "invalid_token", "This account has been disabled.")
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
	// ...and the same auth KIND, for the same reason (BUG-3007, codex
	// round 2). The credential here IS a PAT in the Authorization
	// header, so the liveness predicate's existing api_token door
	// re-checks it verbatim — no new machinery, and this accept point
	// stops being a hole the predicate cannot see. The OAuth branch
	// below deliberately records no kind: its credential is an opaque
	// fosite token whose liveness door does not exist yet, so it fails
	// closed rather than exempting itself. Neither matters until an MCP
	// route is long-lived, and none is.
	ctx = context.WithValue(ctx, ctxAuthKind, authKindAPIToken)
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
	// The canonical audience is the MOUNT's (TASK-3321 U2a): /mcp checks
	// the /mcp resource, and the ChatGPT catalog's mount checks its own, so
	// a token bound to one is refused at the other. A route that sets no
	// resource is /mcp's.
	canonical := mcpResourceFromContext(r.Context())
	if canonical == "" {
		canonical = s.oauthServer.AllowedAudience()
	}
	if canonical == "" {
		// Misconfigured server — no canonical audience to check
		// against. Fail-closed: refuse the token rather than
		// fall through to "any audience is fine."
		slog.Error("oauth server has no canonical audience; refusing all OAuth tokens at /mcp")
		s.writeMCPUnauthorized(w, r, "invalid_token", "OAuth server is misconfigured.")
		return
	}
	if !audienceContains(ar.GetGrantedAudience(), canonical) {
		// TASK-961: audience-mismatch is a primary spec-compliance
		// signal — count it explicitly so dashboards distinguish
		// "client misconfigured the resource indicator" from generic
		// 401s. Fires BEFORE the response so a slow metrics path
		// can't delay the reject. The MCPAuditLog wrapper never sees
		// this branch because we return before next.ServeHTTP, so
		// the metric is the only real-time signal for these denials.
		if s.metrics != nil {
			s.metrics.MCPAuthzDenialsTotal.WithLabelValues("audience_mismatch").Inc()
		}
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
	if err != nil || user == nil || user.IsApp() {
		// User was deleted between grant and use, OR the storage
		// layer is failing. Either way, the bearer can't represent
		// a valid identity — reject. A bot subject is no person either
		// (TASK-3392).
		s.writeMCPUnauthorized(w, r, "invalid_token", "Token references an unknown user.")
		return
	}
	// A disabled account's grant ends here too (BUG-3349).
	if user.IsDisabled() {
		s.writeMCPUnauthorized(w, r, "invalid_token", "This account has been disabled.")
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

	// Stash the workspace allow-list onto the request context so
	// RequireWorkspaceAccess can gate workspace access per-token
	// (TASK-953). Two sources exist:
	//
	//   1. Legacy session.Extra path (TASK-952). Per-token, and
	//      carried forward unchanged on every refresh rotation.
	//   2. oauth_connections + oauth_connection_workspaces (PLAN-1519).
	//      Per-grant-chain, keyed by request_id, and the only place
	//      Connected Apps edits.
	//
	// When the chain HAS a connection row, that row is authoritative
	// and Extra is ignored (BUG-3337). The old OR-merge let a backfilled
	// connection's Extra keep granting what the user later removed in
	// Connected Apps: narrowing a wildcard or dropping a workspace never
	// took effect, and refresh preserved Extra, so it never would.
	// Startup runs BackfillOAuthConnections, which seeds a row for every
	// chain from its Extra, so ignoring Extra loses no access the user
	// did not themselves remove, with one exception: the backfill skips
	// a slug it cannot resolve, including a workspace soft-deleted when
	// it ran, so restoring that workspace does not restore the grant.
	// That fails closed, and the user re-adds it in Connected Apps.
	// Extra is read only for a chain with no row (a backfill that
	// failed for that chain), as before.
	//
	// Hot-path cost: one PK lookup against oauth_connections per
	// request, plus one indexed scan + small join when the
	// all_current_workspaces flag is false. Both queries hit indexed
	// columns; total overhead is sub-millisecond per call (see
	// PLAN-1519's Risks section + the bench in
	// internal/store/bench_oauth_connections_test.go).
	var extraAllowed []string
	if oauthSession, ok := session.(*oauth.Session); ok {
		extraAllowed = oauthSession.AllowedWorkspaces()
	}
	access, accessErr := s.store.GetOAuthConnectionAccess(ar.GetID())
	if accessErr != nil {
		// I/O error reading the connection — fail CLOSED. We cannot
		// know whether this grant is scoped or unrestricted without a
		// successful read; falling through to the legacy-Extra-only
		// path would silently widen a post-Phase-C connection (where
		// the new tables are authoritative and session.Extra is empty
		// by design) into "no allow-list," granting access to every
		// workspace the user is a member of. Codex review #581 round
		// 1 caught the regression.
		//
		// Mirrors the IntrospectToken storage-error policy a few
		// branches up: storage failures collapse to a 401 rather than
		// a 500 because (a) we cannot validate the grant, so the
		// client MUST not proceed, and (b) a spec-shaped 401 + retry
		// is the same recovery path the client would take for a
		// transient outage anyway. The slog.Warn lets ops spot the
		// underlying DB issue via existing alerting.
		slog.Warn("oauth_connections access lookup failed; failing closed",
			"request_id", ar.GetID(), "error", accessErr)
		if s.metrics != nil {
			s.metrics.MCPAuthzDenialsTotal.WithLabelValues("connection_lookup_error").Inc()
		}
		s.writeMCPUnauthorized(w, r, "invalid_token", "Token validation failed; please retry.")
		return
	}
	if allowed := mergeAllowedWorkspaces(extraAllowed, access); allowed != nil {
		ctx = WithTokenAllowedWorkspaces(ctx, allowed)
	}

	next.ServeHTTP(w, r.WithContext(ctx))
}

// mergeAllowedWorkspaces computes the effective workspace allow-list
// for an OAuth-authenticated MCP request from the two sources described
// in handleMCPOAuthAuth's stashing block.
//
// With a connection row (access.HasConnection) the row alone decides
// (BUG-3337): all_current_workspaces → nil (unrestricted), otherwise
// its slugs, sorted, as a non-nil slice; an empty one is fail-closed
// ("consent existed but scoped to nothing"). Extra is not consulted.
//
// Without a row, Extra decides as it did before PLAN-1519: nil or a
// "*" anywhere → nil, otherwise its deduplicated, sorted slugs.
//
// Pulled out as a free function so the policy can be unit-tested
// without spinning up the OAuth server. The integration with fosite
// + introspection lives in handleMCPOAuthAuth above.
func mergeAllowedWorkspaces(extra []string, access store.OAuthConnectionAccess) []string {
	source := extra
	if access.HasConnection {
		if access.AllCurrentWorkspaces {
			return nil
		}
		source = access.WorkspaceSlugs
	} else {
		if extra == nil {
			return nil
		}
		for _, s := range extra {
			if s == "*" {
				return nil
			}
		}
	}
	seen := make(map[string]struct{}, len(source))
	allowed := make([]string, 0, len(source))
	for _, s := range source {
		if s == "" || s == "*" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		allowed = append(allowed, s)
	}
	sort.Strings(allowed)
	return allowed
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
// With OAuth available the challenge also names scope (TASK-2308), the
// set a client should ask for at /oauth/authorize, and resource_metadata
// stays the LAST parameter. The 403 for a read-only OAuth connection is
// writeMCPInsufficientScope.
//
// resource_metadata (and scope) are present only when OAuth is available (PLAN-2310
// DR-5): with MCP on over http, which is PAT-only, the header is a bare
// `Bearer realm="pad"`, because there is no metadata document to point
// at and a client that follows one would start an OAuth flow that cannot
// complete. The URL comes from configuration, never the request's Host
// (DR-3); see protectedResourceMetadataURL for its shape.
//
// Every caller is a refusal before a caller was identified (missing,
// malformed or invalid bearer), so this is also where PLAN-2310 DR-9's
// pre-auth floor lives: each 401 draws from the per-address MCPPreAuth
// bucket, and once that is empty the answer is 429 instead. Each refusal
// is counted in pad_mcp_preauth_denied_total and writes nothing to the
// database (the DR-9 amendment on the plan: unauthenticated traffic must
// not cause writes).
func (s *Server) writeMCPUnauthorized(w http.ResponseWriter, r *http.Request, code, msg string) {
	if !s.chargeMCPPreAuth(w, r) {
		return
	}
	s.recordMCPPreAuthDenied(code)
	challenge := `Bearer realm="pad"`
	if meta := s.mcpChallengeMetadataURL(r); meta != "" {
		// scope tells a client what to ask for at /oauth/authorize
		// (TASK-2308). Without it Claude Code falls back to the metadata's
		// scopes_supported, which includes pad:admin.
		challenge += `, scope="` + mcpChallengeScope + `", resource_metadata="` + meta + `"`
	}
	w.Header().Set("WWW-Authenticate", challenge)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": msg,
		},
	})
}

// mcpChallengeScope is the scope set a /mcp WWW-Authenticate challenge
// names (TASK-2308): the tiers the consent screen offers a connection.
// pad:admin is left out because tokenScopeAllows grants it nothing
// pad:write does not.
const mcpChallengeScope = "pad:read pad:write"

// mcpChallengeMetadataURL is the protected-resource metadata URL a
// challenge on r names: THIS mount's document (TASK-3321 U2b; the
// ChatGPT mount stamps its own resource), else /mcp's. Empty when OAuth
// is unavailable or no resource is configured, and a challenge then
// names no metadata and no scope.
func (s *Server) mcpChallengeMetadataURL(r *http.Request) string {
	if !s.oauthAvailable() {
		return ""
	}
	resource := mcpResourceFromContext(r.Context())
	if resource == "" {
		resource = s.mcpPublicURL
	}
	return protectedResourceMetadataURL(resource)
}

// ctxMCPResource carries the canonical OAuth resource of the MCP mount a
// request arrived on (TASK-3321 U2a). Set only by WithMCPResource in the
// route table, never from the request.
const ctxMCPResource contextKey = "mcp_resource"

// WithMCPResource returns middleware stamping the mount's canonical resource
// on the request, for MCPBearerAuth's audience check. Install it BEFORE
// MCPBearerAuth on any MCP mount other than /mcp.
func WithMCPResource(resource string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxMCPResource, resource)))
		})
	}
}

func mcpResourceFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxMCPResource).(string)
	return v
}

// MCPResourceMetadataURLFromContext is the protected-resource metadata URL
// of the MCP mount the request arrived on, or "" off a mount. The ChatGPT
// catalog names it in a tool's insufficient_scope challenge (TASK-3321
// U2c).
func MCPResourceMetadataURLFromContext(ctx context.Context) string {
	return protectedResourceMetadataURL(mcpResourceFromContext(ctx))
}
