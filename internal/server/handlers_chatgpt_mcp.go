package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// The ChatGPT catalog's MCP mount (TASK-3321 U2b): the catalog
// internal/mcp/chatgpt_catalog.go defines, served at its own URL, the
// ChatGPT resource (https://mcp.getpad.dev/mcp/chatgpt on cloud, ruled
// host A). It reuses /mcp's perimeter (configured host, bearer auth,
// audit) with three differences:
//
//   - its OAuth audience is its own (WithMCPResource), so a /mcp token is
//     refused here and a token minted for it is refused at /mcp (U2a);
//   - it is OAuth-ONLY: a personal access token is refused with 401, since
//     ChatGPT signs in through OAuth (lead's ruling);
//   - it is served only while chatGPTAvailable says so: OAuth available,
//     the operator switch on, and the catalog's prerequisites wired.

// SetChatGPTMCPTransport wires the ChatGPT catalog's transport and its
// canonical resource. enabled is the operator switch
// (PAD_CHATGPT_MCP_ENABLED); a nil transport or empty resource leaves the
// mount unavailable whatever the switch says.
func (s *Server) SetChatGPTMCPTransport(transport http.Handler, resource string, enabled bool, knownCallName func(string) bool) {
	s.chatGPTTransport = transport
	s.chatGPTResource = strings.TrimRight(resource, "/")
	s.chatGPTEnabled = enabled
	if knownCallName != nil {
		prev := s.mcpCallNameKnown
		s.mcpCallNameKnown = func(n string) bool {
			return knownCallName(n) || (prev != nil && prev(n))
		}
	}
}

// chatGPTAvailable reports whether the ChatGPT mount is served: OAuth is
// available (the mount is OAuth-only), the operator switched it on, and a
// transport and resource are wired.
func (s *Server) chatGPTAvailable() bool {
	return s.chatGPTEnabled && s.chatGPTTransport != nil && s.chatGPTResource != "" && s.oauthAvailable()
}

func (s *Server) requireChatGPTAvailable(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.chatGPTAvailable() {
			writeError(w, http.StatusNotFound, "not_found", "Not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refusePATs answers 401 for a personal access token: the ChatGPT mount is
// OAuth-only. It runs before MCPBearerAuth, whose PAT branch would
// otherwise accept the token without the OAuth audience check.
func (s *Server) refusePATs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, ok := extractBearer(r.Header.Get("Authorization")); ok && strings.HasPrefix(token, "pad_") {
			s.writeMCPUnauthorized(w, r, "invalid_token",
				"This endpoint accepts OAuth sign-in only; personal access tokens are not accepted here.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) registerChatGPTMCPRoutes(r chi.Router) {
	transport := s.chatGPTTransport
	if transport == nil {
		transport = http.NotFoundHandler()
	}
	// The resource is fixed when the route is built: the mount's audience
	// is configuration, never a request's.
	// limitMCPBody: the same transport, the same uncapped read (BUG-3534),
	// placed as on /mcp: after auth and the audit log.
	r.With(s.requireChatGPTAvailable, s.requireConfiguredHost, WithMCPResource(s.chatGPTResource),
		s.refusePATs, s.MCPBearerAuth, s.countMCPRequest("chatgpt"), s.MCPAuditLog, s.limitMCPBody).Mount("/mcp/chatgpt", transport)
	// Path-aware RFC 9728 document for this resource (the /mcp ones are
	// unchanged).
	r.With(s.requireChatGPTAvailable, s.requireConfiguredHost).
		Get("/.well-known/oauth-protected-resource/mcp/chatgpt", s.handleChatGPTProtectedResource)
}

func (s *Server) handleChatGPTProtectedResource(w http.ResponseWriter, _ *http.Request) {
	authServer := strings.TrimRight(s.mcpAuthServerURL, "/")
	if s.chatGPTResource == "" || authServer == "" {
		writeError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}
	doc := protectedResourceMetadata{
		Resource:               s.chatGPTResource,
		AuthorizationServers:   []string{authServer},
		BearerMethodsSupported: []string{"header"},
		ScopesSupported:        []string{"pad:read", "pad:write"},
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(doc)
}
