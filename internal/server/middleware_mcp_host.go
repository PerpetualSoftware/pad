package server

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// requireConfiguredHost is PLAN-2310 DR-6's Host allowlist. Off cloud it
// answers 421 Misdirected Request to any request on /mcp, /oauth/* or
// /.well-known/oauth-* whose Host does not name one of the configured
// addresses: the public origin, the MCP URL and the auth-server URL.
//
// It closes DNS rebinding. A page on an attacker's domain can point that
// domain at a LAN box running Pad, and the browser then sends same-origin
// requests carrying the attacker's hostname; this refuses them before
// auth, so nothing is authenticated, audited or rate-charged for such a
// request. It does not replace TLS (PLAN-2310, known gaps).
//
// It runs after the availability gate, so while MCP is off every one of
// these paths still answers the gate's 404, whatever the Host (the U2
// upgrade guarantee).
//
// Matching is case-insensitive on the host, with the port compared after
// defaulting (443 for https, 80 for http, taken from the configured URL's
// scheme when the request names no port). When a configured host is
// loopback, every loopback spelling (localhost, 127.0.0.0/8, ::1) with
// the configured port matches, since a local client may use any of them.
// An empty Host is refused: HTTP/1.0 requests carry none either, and
// exempting it would be an exemption anyone can claim.
//
// The /api/v1 routes are deliberately NOT wrapped. They keep the regular
// API perimeter, and the in-process MCP dispatcher sends them requests
// with an empty Host (internal/mcp/dispatch_http.go), which this would
// refuse.
//
// Cloud keeps its behaviour: the allowlist is off-cloud only (Dave:
// "off cloud"); extending it to cloud is a follow-up.
//
// mcp-go's own check stays disabled (internal/mcp/transport.go): it has
// no allowlist and refuses a legitimate local reverse proxy, and this is
// strictly stronger off cloud.
func (s *Server) requireConfiguredHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cloudMode || s.hostIsConfigured(r.Host) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusMisdirectedRequest, "misdirected_request",
			"This host is not configured to serve MCP. Use the configured address.")
	})
}

// hostIsConfigured reports whether host (a request's Host header) names
// one of the configured MCP addresses, per requireConfiguredHost's rules.
func (s *Server) hostIsConfigured(host string) bool {
	if host == "" {
		return false
	}
	ep := s.mcpEndpoints
	for _, raw := range []string{ep.Origin, ep.ResourceURL, ep.AuthServerURL} {
		if raw != "" && hostMatchesURL(host, raw) {
			return true
		}
	}
	return false
}

// hostMatchesURL compares a request Host with the host and port of a
// configured URL.
func hostMatchesURL(reqHost, configured string) bool {
	u, err := url.Parse(strings.TrimSpace(configured))
	if err != nil || u.Hostname() == "" {
		return false
	}
	defPort := "80"
	if strings.EqualFold(u.Scheme, "https") {
		defPort = "443"
	}
	cfgHost, cfgPort := strings.ToLower(u.Hostname()), u.Port()
	if cfgPort == "" {
		cfgPort = defPort
	}

	h, p, err := net.SplitHostPort(reqHost)
	if err != nil {
		// No port (or not host:port at all). A bracketed IPv6 literal
		// without a port keeps its brackets here; strip them.
		h, p = strings.TrimSuffix(strings.TrimPrefix(reqHost, "["), "]"), ""
	}
	h = strings.ToLower(h)
	if p == "" {
		p = defPort
	}
	if p != cfgPort {
		return false
	}
	if h == cfgHost {
		return true
	}
	return isLoopbackHost(cfgHost) && isLoopbackHost(h)
}

// isLoopbackHost reports whether h is "localhost" or a loopback IP.
func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
