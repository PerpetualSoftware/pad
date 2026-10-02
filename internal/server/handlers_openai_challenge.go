package server

import "net/http"

// handleOpenAIAppsChallenge serves OpenAI's plugin domain verification
// token at /.well-known/openai-apps-challenge (TASK-3321 G4). OpenAI fetches
// it from the MCP server's host and requires "only the exact token — not
// JSON or a list of tokens", so the body is the configured token and
// nothing else: no trailing newline, text/plain.
//
// It answers only on cloud, only when PAD_OPENAI_APPS_CHALLENGE resolved to
// a token, and only on the MCP URL's host (mcp.getpad.dev); everything else
// gets the same JSON 404 as any unowned /.well-known path. It is not behind
// the MCP or OAuth availability gates, because verification happens before
// the plugin's server is switched on. The host is the effective one
// requireConfiguredHost reads (X-Forwarded-Host only from a trusted proxy).
func (s *Server) handleOpenAIAppsChallenge(w http.ResponseWriter, r *http.Request) {
	token := s.mcpEndpoints.AppsChallenge
	host, _ := s.effectiveMCPHost(r)
	if !s.cloudMode || token == "" || s.mcpEndpoints.ResourceURL == "" || !hostMatchesURL(host, s.mcpEndpoints.ResourceURL) {
		writeWellKnownNotFound(w)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(token))
}

// writeWellKnownNotFound is the JSON 404 for a /.well-known path this server
// does not publish (TASK-3321 U0a).
func writeWellKnownNotFound(w http.ResponseWriter) {
	// writeError leaves Content-Type to the /api/v1 middleware, which
	// does not run here.
	w.Header().Set("Content-Type", "application/json")
	writeError(w, http.StatusNotFound, "not_found", "Not found")
}
