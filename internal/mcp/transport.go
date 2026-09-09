package mcp

import (
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ServedProtocolVersions is the set of MCP wire protocol revisions pad's
// remote transport advertises: the LEGACY era only, meaning every revision
// that still uses the initialize/initialized handshake.
//
// WHY THIS IS RESTRICTED AT ALL (TASK-2977). mcp-go 1.0 implements the
// stateless protocol core introduced in 2026-07-28 — no handshake, no
// sessions, per-request identity in `_meta` — and its Streamable HTTP
// transport advertises EVERY revision it implements by default, serving both
// eras concurrently on one endpoint and deciding the era per request. So the
// library bump alone would have had pad advertising the modern era through
// server/discover, while pad://_meta/version publishes 2025-11-25 as the
// maximum revision this server can negotiate. That mismatch is the defect:
// the advertisement is the promise a client acts on, and nothing here has
// been read against the modern revision, let alone tested.
//
// It is also not merely a documentation gap. pad_set_workspace pins a session
// default workspace, and the stateless era has no sessions for that pin to
// live in — so the modern era is not something pad happens not to advertise,
// it is something pad is not known to be able to serve. Establishing what it
// would take is a separate unit; until then the honest advertisement is the
// era pad was built against and is tested against.
//
// DERIVED, NOT LISTED, and that is load-bearing. mcp.LegacyProtocolVersions()
// is the SDK's own answer to "which revisions use the handshake", so a future
// SDK that adds another legacy revision includes it here automatically and one
// that adds another modern revision excludes it automatically. A hand-written
// list would silently mean the wrong thing after either bump, which is the
// shape of the defect this function exists to close.
func ServedProtocolVersions() []string {
	return mcp.LegacyProtocolVersions()
}

// NewRemoteTransport builds the Streamable HTTP transport pad serves at /mcp
// in cloud mode.
//
// It lives here rather than inline at the call site so the option set is
// reachable from a test: the advertised protocol versions are only correct if
// the option is actually PASSED, and a test that constructs its own transport
// vouches for the option and not for the binding (team CONVE-19).
//
// sessionIDs is the caller's session-id manager. It is a parameter because the
// generate-only manager pad uses exists for the active-sessions tracker in
// cmd/pad, and the reason it is not mcp-go's WithStateLess(true) is documented
// there.
func NewRemoteTransport(srv *server.MCPServer, sessionIDs server.SessionIdManager) *server.StreamableHTTPServer {
	return server.NewStreamableHTTPServer(
		srv,
		server.WithEndpointPath("/mcp"),
		server.WithSessionIdManager(sessionIDs),
		// See ServedProtocolVersions: without this the transport advertises
		// the stateless 2026-07-28 core through server/discover.
		server.WithStreamableHTTPProtocolVersions(ServedProtocolVersions()...),
		// mcp-go v0.56 turns on DNS-rebinding protection by default: a request
		// whose accept socket is loopback but whose Host header is non-loopback
		// gets a 403. pad-cloud's mcp.getpad.dev vhost sits behind a reverse
		// proxy that forwards to this process over 127.0.0.1 while preserving
		// the original Host, so the default would reject every real request.
		// Disable it to keep the pre-v0.56 behaviour — the browser-driven
		// rebinding threat it guards against doesn't apply here: this transport
		// only mounts in cloud mode and every request is Bearer/OAuth
		// authenticated.
		server.WithDisableLocalhostProtection(true),
	)
}
