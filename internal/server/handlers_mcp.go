package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// MCP transport state. Set at startup by cmd/pad via SetMCPTransport on
// every install (PLAN-2310 DR-4); whether a request may reach it is decided
// per request by mcpAvailable (DR-5).
//
// Why these fields live on Server (not in a separate sub-struct):
//
// They're cheap (one pointer + two strings), plumbed through
// setupRouter which already lives on Server, and read by both the
// /mcp middleware (mcpPublicURL for WWW-Authenticate) and the
// /.well-known handlers (both URLs for the discovery doc). A nested
// struct would add an indirection without saving any clarity.
//
// Why they aren't on the routes themselves (closure-captured):
//
// setupRouter runs once via routerOnce; SetMCPTransport must be called
// BEFORE setupRouter to take effect. We document that contract in
// SetMCPTransport's comment and rely on the cmd/pad/main.go startup
// order. Capturing in closure would make that ordering invisible —
// the Server-field path is easier to reason about.

// SetMCPTransport wires the Streamable HTTP MCP server onto pad's
// router. Called once at startup by cmd/pad/main.go after the MCP
// server, dispatcher, and tool catalog have been constructed.
//
// transport is the http.Handler returned by mcp-go's
// NewStreamableHTTPServer — it owns request decoding, JSON-RPC
// dispatch, and tool-call execution. This file's job is just to mount
// it under the right auth middleware on the right route.
//
// mcpPublicURL is the canonical public URL of the MCP vhost, e.g.
// "https://mcp.getpad.dev". Used by:
//
//   - handleOAuthProtectedResource to populate the "resource" field
//     of the discovery doc (concatenates "/mcp").
//   - writeMCPUnauthorized to populate the WWW-Authenticate header's
//     "resource_metadata" parameter (concatenates
//     "/.well-known/oauth-protected-resource").
//
// authServerURL is the canonical URL of the OAuth authorization
// server, e.g. "https://app.getpad.dev". Used by
// handleOAuthProtectedResource to populate "authorization_servers".
//
// Both come from configuration (config.ResolveMCPEndpoints), never from
// a request's Host (PLAN-2310 DR-3). Either may be empty when nothing is
// configured; the documents that need it then answer 404.
//
// MUST be called before the first request hits the server, i.e.
// before ListenAndServe. Setting this after setupRouter has already
// run is a no-op (chi routes are immutable post-mount); the Server
// silently accepts the call to avoid breaking tests that wire
// transport state into a Server they then never start, but the
// `routerOnce.Do` ordering in ensureRouter means production traffic
// would just see 404s on /mcp.
func (s *Server) SetMCPTransport(transport http.Handler, mcpPublicURL, authServerURL string, knownCallName func(string) bool) {
	s.mcpTransport = transport
	// Bounds the MCP metrics tool label (BUG-2817). A parameter rather than
	// a separate setter, so the transport cannot be mounted without deciding
	// it; nil records every name as "unknown".
	s.mcpCallNameKnown = knownCallName
	s.mcpPublicURL = mcpPublicURL
	s.mcpAuthServerURL = authServerURL

	// Spawn the audit log writer + retention sweeper now that the
	// MCP surface is wired (TASK-960). Idempotent — safe to call
	// from tests that flip transport state on/off via repeated
	// SetMCPTransport. The writer outlives the request flow; it's
	// stopped from Server.Stop via stopMCPAuditWriter.
	s.startMCPAuditWriter()

	// Spawn the session tracker + periodic sweeper (PLAN-943
	// TASK-1120). Replaces the naive +1/-1 active-sessions gauge
	// accounting that drifted upward on crashed clients. Idempotent
	// like startMCPAuditWriter. ttl + sweep interval are tuned via
	// PAD_MCP_SESSION_TTL / PAD_MCP_SESSION_SWEEP_INTERVAL by
	// cmd/pad through SetMCPSessionTrackerConfig before this call;
	// otherwise the package defaults (ttl=30m, sweep=5m) apply.
	s.startMCPSessionTracker()
}

// SetToolSurfaceHandler wires the MCP catalog→JSON serializer onto the
// server (PLAN-1888 / TASK-1891). Called once at startup by
// cmd/pad/main.go with mcp.ToolSurfaceJSON.
//
// Why injection rather than a direct import: internal/mcp imports
// internal/server (dispatch_http.go), so internal/server cannot import
// internal/mcp to build the catalog descriptor JSON itself — that would
// close the import cycle. This is the exact SetMCPTransport pattern:
// cmd/pad/main.go imports both packages and hands the serializer down.
//
// Unlike SetMCPTransport, the tool-surface endpoint mounts
// unconditionally inside the authed API group (it's available on both
// cloud and self-host — the browser WebMCP layer needs it everywhere),
// so this can be called any time before the first request. When the
// serializer is nil (never injected), handleMCPToolSurface returns 404.
func (s *Server) SetToolSurfaceHandler(fn func() ([]byte, error)) {
	s.toolSurfaceJSON = fn
}

// handleMCPToolSurface serves the MCP tool-surface descriptor JSON —
// the nine catalog tools, their actions (each with a read_only bool),
// and their input schemas. Backs GET /api/v1/mcp/tool-surface, mounted
// in the authed API group so it inherits the session/token auth chain.
//
// The body is built entirely by the injected serializer
// (mcp.ToolSurfaceJSON), which reads only the static MCP catalog. No
// request state, route table, or other server internals reach the
// response — the endpoint exposes catalog descriptors and nothing else.
func (s *Server) handleMCPToolSurface(w http.ResponseWriter, _ *http.Request) {
	if s.toolSurfaceJSON == nil {
		writeError(w, http.StatusNotFound, "not_found", "MCP tool surface not available")
		return
	}
	body, err := s.toolSurfaceJSON()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to serialize MCP tool surface")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// registerMCPRoutes installs the /mcp + /.well-known endpoints on r,
// gated by cloud mode. Called from setupRouter at infrastructure-
// middleware level (NOT inside the API group), because:
//
//   - /mcp uses Bearer auth (its own middleware), bypassing TokenAuth /
//     SessionAuth / CSRFProtect / RequireAuth — those are designed for
//     the SPA + CLI surface and produce a different 401 envelope.
//   - /.well-known/* are public discovery documents (RFC 9728 §3
//     "MUST be available without authentication"). Putting them inside
//     the auth-required API group would force a special-case exemption.
//
// The routes mount on every install and are gated per request (PLAN-2310
// DR-5): /mcp by mcpAvailable, the discovery documents by oauthAvailable.
// The router is built once, and the MCP setting changes while the process
// runs, so only a handler can consult it. Unavailable is a JSON 404 that
// runs before auth and audit, so a request to a disabled /mcp is neither
// authenticated nor recorded. The protected-resource document is
// oauthAvailable rather than mcpAvailable because it names an
// authorization server, and advertising one that does not exist is what
// misleads discovery clients.
//
// Why a Get on /mcp (not just Post): MCP Streamable HTTP supports
// GET for server-initiated SSE streams (mcp-go's StreamableHTTPServer
// handles both methods). Mounting via chi's Method-agnostic Mount
// would also catch DELETE (session-end notifications). Use the same
// Mount the streamable_http server expects.
func (s *Server) registerMCPRoutes(r chi.Router) {
	// A server with no transport (tests that never wire one) still mounts
	// the routes, so /mcp answers the JSON 404 rather than the SPA.
	transport := s.mcpTransport
	if transport == nil {
		transport = http.NotFoundHandler()
	}

	// /mcp — gated by Bearer auth, no CSRF (Bearer auth is immune),
	// no rate limit yet (TASK-959 wires that). The streamable HTTP
	// server handles all methods (POST handshake/calls, GET for SSE
	// streams, DELETE for session termination), so we Mount() rather
	// than registering per-method.
	//
	// MCPAuditLog (TASK-960) sits AFTER MCPBearerAuth so the audit
	// row carries the resolved user + token identity. It also runs
	// AFTER the rate-limit gates inside MCPBearerAuth so 429s get
	// captured as result_status="denied". The audit middleware is a
	// no-op when the writer hasn't been spawned (selfhost / test
	// builds), so the chain is safe to mount unconditionally.
	//
	// MCPInsufficientScope (TASK-2308) sits after MCPAuditLog so its 403
	// is audited as denied.
	//
	// limitMCPBody (BUG-3534) runs before auth and before anything reads the
	// body: the transport reads it whole and has no cap of its own.
	r.With(s.requireMCPAvailable, s.requireConfiguredHost, s.limitMCPBody, s.MCPBearerAuth, s.MCPAuditLog, s.MCPInsufficientScope).Mount("/mcp", transport)

	// Discovery endpoints — unauthenticated, oauthAvailable-gated. RFC 9728
	// (protected-resource) and RFC 8414 (auth-server) metadata.
	r.With(s.requireOAuthAvailable, s.requireConfiguredHost).Get("/.well-known/oauth-protected-resource", s.handleOAuthProtectedResource)
	// Path-aware RFC 9728 §3.1 variant (BUG-2266). A client configured
	// with the path-suffixed transport URL (https://mcp.getpad.dev/mcp —
	// the shape every FastMCP example uses) constructs its metadata URL
	// by inserting the well-known segment BEFORE the path:
	// /.well-known/oauth-protected-resource/mcp. Without this route the
	// request fell through to the SPA catch-all and discovery died
	// JSON-parsing HTML. Bounded to the two shapes a pasted transport
	// URL actually produces (/mcp and a trailing-slash /mcp/) rather
	// than a wildcard: the handler sets Cache-Control public max-age,
	// and a wildcard would hand a CDN one cacheable object per
	// attacker-chosen suffix (codex round 2). /mcp is the only
	// path-mounted resource, so nothing else needs the variant.
	r.With(s.requireOAuthAvailable, s.requireConfiguredHost).Get("/.well-known/oauth-protected-resource/mcp", s.handleOAuthProtectedResource)
	r.With(s.requireOAuthAvailable, s.requireConfiguredHost).Get("/.well-known/oauth-protected-resource/mcp/", s.handleOAuthProtectedResource)
	r.With(s.requireOAuthAvailable, s.requireConfiguredHost).Get("/.well-known/oauth-authorization-server", s.handleOAuthAuthorizationServer)
	// OpenAI's plugin domain verification (TASK-3321 G4). Ungated by
	// design: see handleOpenAIAppsChallenge.
	r.Get("/.well-known/openai-apps-challenge", s.handleOpenAIAppsChallenge)

	// Every other /.well-known/ path is a JSON 404 (TASK-3321 U0a). It used to
	// fall through to the SPA catch-all, which answers 200 text/html, so a
	// discovery client probing openid-configuration (or any well-known
	// document this server does not publish) got a success it then failed to
	// parse. chi prefers the exact routes above, so they are unaffected, and
	// this one carries no gate: "not published" is true on every install.
	r.HandleFunc("/.well-known/*", func(w http.ResponseWriter, _ *http.Request) {
		writeWellKnownNotFound(w)
	})
}
