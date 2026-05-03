package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// MCP transport state. Set at startup by cmd/pad/main.go via
// SetMCPTransport when the deployment is in cloud mode (PAD_MODE=cloud).
// Self-hosted deployments leave mcpTransport nil and the routes below
// don't mount, so the binary stays free of MCP-server overhead unless
// it's actually serving the public /mcp surface.
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
// Both URLs may be left empty in development; the handlers fall back
// to the request's Host header with a best-effort mcp.→app. rewrite
// (see rewriteMcpSubdomain). Production deployments should always set
// PAD_MCP_PUBLIC_URL and PAD_AUTH_SERVER_URL so the discovery
// document's URLs match the cert + the URL agents paste into Claude.
//
// MUST be called before the first request hits the server, i.e.
// before ListenAndServe. Setting this after setupRouter has already
// run is a no-op (chi routes are immutable post-mount); the Server
// silently accepts the call to avoid breaking tests that wire
// transport state into a Server they then never start, but the
// `routerOnce.Do` ordering in ensureRouter means production traffic
// would just see 404s on /mcp.
func (s *Server) SetMCPTransport(transport http.Handler, mcpPublicURL, authServerURL string) {
	s.mcpTransport = transport
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
// Routes mount only when SetMCPTransport has been called AND cloud mode
// is enabled. Self-hosted deployments with no cloud secret skip this
// entirely; deployments running cloud mode without the MCP transport
// wired (e.g. someone testing a build with PAD_MCP_PUBLIC_URL unset)
// also skip. No 404 leaks in either case — the routes simply don't
// exist on the chi tree.
//
// Why a Get on /mcp (not just Post): MCP Streamable HTTP supports
// GET for server-initiated SSE streams (mcp-go's StreamableHTTPServer
// handles both methods). Mounting via chi's Method-agnostic Mount
// would also catch DELETE (session-end notifications). Use the same
// Mount the streamable_http server expects.
func (s *Server) registerMCPRoutes(r chi.Router) {
	if s.mcpTransport == nil || !s.IsCloud() {
		return
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
	r.With(s.requireCloudMode, s.MCPBearerAuth, s.MCPAuditLog).Mount("/mcp", s.mcpTransport)

	// Discovery endpoints — unauthenticated, cloud-mode-gated. RFC 9728
	// (protected-resource) gets the real metadata; RFC 8414 (auth-server)
	// is the 501 stub TASK-951 fills in.
	r.With(s.requireCloudMode).Get("/.well-known/oauth-protected-resource", s.handleOAuthProtectedResource)
	r.With(s.requireCloudMode).Get("/.well-known/oauth-authorization-server", s.handleOAuthAuthorizationServer)
}
