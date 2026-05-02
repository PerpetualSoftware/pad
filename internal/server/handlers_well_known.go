package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleOAuthProtectedResource serves RFC 9728 "OAuth 2.0 Protected
// Resource Metadata" at /.well-known/oauth-protected-resource on the
// MCP vhost (mcp.getpad.dev in production). Claude Desktop and other
// MCP clients fetch this in response to a 401 + WWW-Authenticate from
// /mcp; the document tells them which authorization server issues
// tokens for this resource and which scopes / methods we accept.
//
// Spec: https://datatracker.ietf.org/doc/html/rfc9728
//
// The companion document (authorization-server metadata, RFC 8414)
// lives on the auth-server vhost (app.getpad.dev) and is wired in
// TASK-951; for TASK-950 the corresponding handler is a 501 stub
// (see handleOAuthAuthorizationServerStub below) so MCP clients
// hitting the discovery chain get a clear "not yet" rather than a
// 404 that would look like a misconfigured deployment.
//
// This handler is mounted under the cloud-mode gate — self-hosted
// deployments don't expose it (they don't host a public OAuth surface).
// It's intentionally unauthenticated: discovery documents are public
// by design (RFC 9728 §3 "MUST be available without authentication").
//
// Output:
//
//	{
//	  "resource": "https://mcp.getpad.dev/mcp",
//	  "authorization_servers": ["https://app.getpad.dev"],
//	  "bearer_methods_supported": ["header"],
//	  "scopes_supported": ["pad:read", "pad:write", "pad:admin"]
//	}
//
// resource and authorization_servers come from runtime config wired at
// startup via SetMCPTransport (cmd/pad/main.go reads PAD_MCP_PUBLIC_URL
// and PAD_AUTH_SERVER_URL). When unset, we fall back to deriving from
// the request scheme + host so local testing without those env vars
// still produces a valid document; ops should always set them in
// production so the doc matches the public URL the cert covers.
func (s *Server) handleOAuthProtectedResource(w http.ResponseWriter, r *http.Request) {
	resource := strings.TrimRight(s.mcpPublicURL, "/")
	if resource == "" {
		resource = "https://" + r.Host
	}
	resource = resource + "/mcp"

	authServer := strings.TrimRight(s.mcpAuthServerURL, "/")
	if authServer == "" {
		// Fallback: assume the auth server lives at the same scheme +
		// host as the request, with the mcp.* subdomain rewritten to
		// app.* per PLAN-943 architecture. Imperfect — local dev
		// against a single host won't have the rewrite — but better
		// than emitting an empty list. Operators should set
		// PAD_AUTH_SERVER_URL explicitly in production.
		authServer = "https://" + rewriteMcpSubdomain(r.Host)
	}

	doc := protectedResourceMetadata{
		Resource:               resource,
		AuthorizationServers:   []string{authServer},
		BearerMethodsSupported: []string{"header"},
		// Scopes_supported is the full set we anticipate offering once
		// TASK-953 lands the per-token allow-list. Listed here even
		// though the PAT-first cut of TASK-950 doesn't enforce them —
		// MCP clients use this set during DCR (TASK-951) to decide
		// which scopes to request, so under-promising would force a
		// doc update + cache invalidation when scope-aware tokens land.
		ScopesSupported: []string{"pad:read", "pad:write", "pad:admin"},
	}

	w.Header().Set("Content-Type", "application/json")
	// Cache-Control on a small, deploy-static document keeps repeat
	// discovery requests off the backend. 1 hour is short enough that
	// a deploy fixing the doc propagates quickly, long enough to
	// absorb the chatter from a directory of MCP clients (Claude,
	// Cursor, ChatGPT, …) all probing in parallel.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(doc)
}

// handleOAuthAuthorizationServerStub is the placeholder for RFC 8414
// "OAuth 2.0 Authorization Server Metadata" on the auth-server vhost.
// TASK-951 will fill this in with the real /oauth/{authorize, token,
// register, revoke, introspect} endpoint URLs + supported response
// types, grant types, PKCE methods, etc.
//
// We mount the stub now so the discovery chain is complete from the
// MCP client's perspective: they hit /.well-known/oauth-protected-
// resource (this PR), follow authorization_servers[0] to /.well-known/
// oauth-authorization-server, and get a 501 with a clear message
// rather than a 404 that would suggest misconfiguration.
//
// Once TASK-951 ships, replace the body with the real metadata
// document.
func (s *Server) handleOAuthAuthorizationServerStub(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// Retry-After helps well-behaved clients back off rather than
	// hammering us until TASK-951 lands. 3600 seconds is generous
	// enough to absorb a deploy window without being annoyingly long.
	w.Header().Set("Retry-After", "3600")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":             "not_implemented",
		"error_description": "OAuth authorization server is not yet enabled on this deployment. Tracked under PLAN-943 TASK-951.",
	})
}

// protectedResourceMetadata is the RFC 9728 wire format. Field names
// match the spec; only the four fields we actually populate are
// declared — the rest of RFC 9728 (jwks_uri,
// resource_signing_alg_values_supported, etc.) is optional and not
// relevant to our opaque-token deployment.
type protectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ScopesSupported        []string `json:"scopes_supported"`
}

// rewriteMcpSubdomain best-effort rewrites "mcp.<rest>" to "app.<rest>"
// so the discovery doc's authorization_servers entry points at the
// auth vhost when the operator hasn't set PAD_AUTH_SERVER_URL. If the
// host doesn't start with "mcp.", returns it unchanged — useful for
// local development against a single host (localhost:7777) where the
// auth server and protected resource share an origin.
func rewriteMcpSubdomain(host string) string {
	const prefix = "mcp."
	if strings.HasPrefix(host, prefix) {
		return "app." + host[len(prefix):]
	}
	return host
}
