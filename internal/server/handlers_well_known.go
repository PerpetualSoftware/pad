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

// handleOAuthAuthorizationServer serves RFC 8414 "OAuth 2.0
// Authorization Server Metadata" at /.well-known/oauth-authorization-server.
//
// Replaces the 501 stub from TASK-950 with the real document
// describing pad's auth-code + refresh + PKCE-S256 flow. Claude
// Desktop and other MCP clients fetch this after following the
// authorization_servers[] pointer in the protected-resource doc;
// the document tells them which endpoint URLs to use, which grant
// types are accepted, what scopes the server understands, etc.
//
// What's enabled:
//
//   - response_types: ["code"]                — auth-code flow only
//   - grant_types:    ["authorization_code", "refresh_token"]
//   - PKCE:           required (S256 only — Config.EnablePKCEPlainChallengeMethod=false)
//   - client auth:    none (public clients only — PKCE-authenticated)
//   - resource indicators (RFC 8707): supported, mandatory per audienceMatchingStrategy
//   - registration:   open DCR (RFC 7591)
//
// Excluded:
//
//   - id_token / OpenID — not declared in scopes_supported
//   - implicit grant — deprecated in OAuth 2.1
//   - client credentials — public-clients-only model
//   - device-code grant — not relevant for MCP-over-HTTPS
//
// URLs are derived from cfg.AuthServerURL (PAD_AUTH_SERVER_URL) at
// startup; falls back to the request scheme + host so local-dev
// works without env vars.
//
// This handler is mounted under the cloud-mode gate (same group as
// /.well-known/oauth-protected-resource). It's intentionally
// unauthenticated — RFC 8414 §3 explicitly says metadata MUST be
// available without authentication.
func (s *Server) handleOAuthAuthorizationServer(w http.ResponseWriter, r *http.Request) {
	// Gate on oauthServer being mounted (Codex review #372 round 3):
	// the discovery doc lives in the MCP route group, while the
	// /oauth/* handlers live in the OAuth route group. Cloud
	// deployments without PAD_MCP_PUBLIC_URL set get the discovery
	// route mounted (registerMCPRoutes) but NOT the OAuth handlers
	// (registerOAuthRoutes nil-checks oauthServer). Without this
	// gate, the document would 200 with URLs that 404 — worse than
	// no document.
	//
	// 503 with a clear error code lets ops detect the
	// misconfiguration faster than a misleading 200 + clients can
	// retry with backoff.
	if s.oauthServer == nil {
		writeError(w, http.StatusServiceUnavailable, "config_error",
			"OAuth authorization server is not enabled on this deployment")
		return
	}

	issuer := s.authServerIssuerURL(r)
	if issuer == "" {
		// Same fail-loud branch as the protected-resource doc:
		// without an issuer the document would mislead clients
		// about the canonical URL. 503 lets ops detect the
		// misconfiguration faster than a 200 with stub URLs would.
		writeError(w, http.StatusServiceUnavailable, "config_error",
			"OAuth authorization server URL is not configured")
		return
	}

	// Sub-PR D (TASK-1026) wires /oauth/revoke + /oauth/introspect.
	//
	// revocation_endpoint_auth_methods_supported = ["none"] is honest:
	// fosite's NewRevocationRequest accepts a public client that
	// posts only `client_id` (no secret, no Bearer header), which
	// matches the "none" value from the OAuth Token Endpoint
	// Authentication Methods registry.
	//
	// introspection_endpoint_auth_methods_supported is intentionally
	// OMITTED. fosite's NewIntrospectionRequest insists on either
	// Bearer auth (a separate active access token, RFC 7662 §2.1's
	// "bearer token" branch) or HTTP Basic — and our public-clients-
	// only model has no Basic-auth path. "bearer" isn't a registered
	// value in the auth-methods registry, so listing "none" would
	// mislead clients into thinking they can post bare token+client_id
	// and have it accepted (the test in this PR locks in that a
	// missing Authorization header is rejected). RFC 8414 §2 marks
	// the field OPTIONAL; omitting it tells discovery clients
	// "negotiate auth out-of-band," which for our case is documented
	// in the public docs at getpad.dev/mcp/local. Codex review #373
	// round 1 caught the mismatch.
	doc := authServerMetadata{
		Issuer:                                 issuer,
		AuthorizationEndpoint:                  issuer + "/oauth/authorize",
		TokenEndpoint:                          issuer + "/oauth/token",
		RegistrationEndpoint:                   issuer + "/oauth/register",
		RevocationEndpoint:                     issuer + "/oauth/revoke",
		IntrospectionEndpoint:                  issuer + "/oauth/introspect",
		ResponseTypesSupported:                 []string{"code"},
		GrantTypesSupported:                    []string{"authorization_code", "refresh_token"},
		CodeChallengeMethodsSupported:          []string{"S256"},
		TokenEndpointAuthMethodsSupported:      []string{"none"},
		RevocationEndpointAuthMethodsSupported: []string{"none"},
		ScopesSupported:                        []string{"pad:read", "pad:write", "pad:admin"},
		ResourceIndicatorsSupported:            true,
		// authorization_response_iss_parameter_supported (RFC 9207)
		// is intentionally OMITTED. Advertising it would imply that
		// /authorize redirects carry iss=<issuer> in the response
		// query string — but fosite v0.49 doesn't add it natively
		// and we don't post-process WriteAuthorizeResponse to inject
		// it. RFC 9207-aware clients (currently rare; not yet
		// required by Claude Desktop) would treat the missing
		// parameter as a protocol violation and reject the response.
		// Codex review #372 round 2 caught the discrepancy; we'll
		// add the parameter in a future PR alongside any RFC 9207
		// requirement we encounter from a client.
	}
	w.Header().Set("Content-Type", "application/json")
	// Same 1-hour cache as the protected-resource doc.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(doc)
}

// authServerMetadata is the RFC 8414 wire shape. Only the fields
// pad populates are declared. RFC 8414 §2 lists many more
// (jwks_uri, ui_locales_supported, op_policy_uri, …) but they're
// optional and not relevant to opaque-token deployments.
//
// revocation_endpoint + introspection_endpoint were added in sub-PR D
// (TASK-1026) when the handlers shipped; keeping their JSON tags
// stable lets clients rely on them.
//
// introspection_endpoint_auth_methods_supported is deliberately not a
// field here. The introspection endpoint only accepts Bearer auth in
// our public-clients-only model, and "bearer" isn't a registered
// auth-methods value — see the comment in handleOAuthAuthorizationServer
// for the full reasoning. RFC 8414 §2 marks it OPTIONAL.
type authServerMetadata struct {
	Issuer                                 string   `json:"issuer"`
	AuthorizationEndpoint                  string   `json:"authorization_endpoint"`
	TokenEndpoint                          string   `json:"token_endpoint"`
	RegistrationEndpoint                   string   `json:"registration_endpoint"`
	RevocationEndpoint                     string   `json:"revocation_endpoint"`
	IntrospectionEndpoint                  string   `json:"introspection_endpoint"`
	ResponseTypesSupported                 []string `json:"response_types_supported"`
	GrantTypesSupported                    []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported          []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported      []string `json:"token_endpoint_auth_methods_supported"`
	RevocationEndpointAuthMethodsSupported []string `json:"revocation_endpoint_auth_methods_supported"`
	ScopesSupported                        []string `json:"scopes_supported"`
	ResourceIndicatorsSupported            bool     `json:"resource_indicators_supported"`
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
