package server

import (
	"net/http"
	"strings"
)

// countMCPRequest counts each authenticated request reaching an MCP mount in
// pad_mcp_http_requests_total{mount, method, client} (TASK-2307). It sits
// right after MCPBearerAuth, so a request refused before a caller is known is
// left to pad_mcp_preauth_denied_total, and it counts on ENTRY: a GET that
// opens a server-sent-event stream is counted while the stream is open.
//
// The question it answers: a stateless go-sdk transport refuses GET with 405,
// which ends the standalone SSE stream. This shows whether any real client
// opens that stream, and which.
func (s *Server) countMCPRequest(mount string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s.metrics != nil {
				s.metrics.MCPHTTPRequestsTotal.WithLabelValues(
					mount, metricsMethodLabel(r.Method), mcpClientClass(r.UserAgent()),
				).Inc()
			}
			next.ServeHTTP(w, r)
		})
	}
}

// mcpClientRules maps a User-Agent substring (lowercased) to a client class,
// first match wins, so a more specific needle precedes a broader one
// ("claude-code" before "claude", "codex" before "openai"). The classes are
// the label's whole vocabulary: an unrecognised agent is "other", an absent
// one "none", so a caller cannot mint a series.
var mcpClientRules = []struct{ needle, class string }{
	{"claude-code", "claude-code"},
	{"claude", "claude"},
	{"codex", "codex"},
	{"chatgpt", "openai"},
	{"openai", "openai"},
	{"cursor", "cursor"},
	{"windsurf", "windsurf"},
	{"codeium", "windsurf"},
	{"visual studio code", "vscode"},
	{"vscode", "vscode"},
	{"copilot", "vscode"},
	{"mcp-remote", "mcp-remote"},
	{"inspector", "mcp-inspector"},
	{"python", "python"},
	{"httpx", "python"},
	{"aiohttp", "python"},
	{"undici", "node"},
	{"node", "node"},
	{"axios", "node"},
	{"go-http-client", "go"},
	{"curl", "curl"},
	{"okhttp", "okhttp"},
}

// mcpClientClass returns the client class for a User-Agent header value.
func mcpClientClass(userAgent string) string {
	ua := strings.ToLower(strings.TrimSpace(userAgent))
	if ua == "" {
		return "none"
	}
	for _, r := range mcpClientRules {
		if strings.Contains(ua, r.needle) {
			return r.class
		}
	}
	return "other"
}
