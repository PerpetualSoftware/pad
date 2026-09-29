package server

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
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
// Behind a proxy that rewrites Host to the backend's own name (Azure
// Application Gateway and App Service, several Kubernetes ingresses), the
// public host arrives in X-Forwarded-Host instead. That header is honoured
// ONLY when the TCP peer is a trusted proxy by the same source of trust as
// client-IP resolution (PAD_TRUSTED_PROXIES, see TrustedProxyRealIP and
// cliAuthScheme); from any other peer it is ignored, so a rebinding page
// cannot pass by sending it (lead ruling on TASK-2319). The refusal says
// which host it saw, what is configured, and how to fix a proxy, and the
// first refusal per host is logged at WARN, rate-limited overall.
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
		if s.cloudMode {
			next.ServeHTTP(w, r)
			return
		}
		host, source := s.effectiveMCPHost(r)
		if s.hostIsConfigured(host) {
			next.ServeHTTP(w, r)
			return
		}
		shown := truncateForMessage(host, 255)
		origin := s.mcpEndpoints.Origin
		s.mcpMisdirected.warnOnce(shown, source, origin, rawPeerAddr(r))
		writeError2(w, http.StatusMisdirectedRequest, "misdirected_request",
			fmt.Sprintf("The host %q (from the %s header) is not a configured address for MCP on this server; the configured origin is %s. "+
				"If Pad is behind a proxy, preserve the Host header, or list the proxy in PAD_TRUSTED_PROXIES so its X-Forwarded-Host is honoured.",
				shown, source, origin),
			map[string]interface{}{
				"received_host":     shown,
				"host_source":       source,
				"configured_origin": origin,
			})
	})
}

// effectiveMCPHost is the host requireConfiguredHost checks, and the header
// it came from: the first X-Forwarded-Host entry when the TCP peer is a
// trusted proxy, else Host. The first entry is the client-facing host, the
// same choice TrustedProxyRealIP makes for X-Forwarded-For.
func (s *Server) effectiveMCPHost(r *http.Request) (host, source string) {
	if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" && len(s.trustedProxyCIDRs) > 0 {
		if peer := peerAddr(rawPeerAddr(r)); peer != nil && ipInCIDRs(peer, s.trustedProxyCIDRs) {
			if first := strings.TrimSpace(strings.SplitN(xfh, ",", 2)[0]); first != "" {
				return first, "X-Forwarded-Host"
			}
		}
	}
	return r.Host, "Host"
}

// truncateForMessage bounds a caller-supplied string before it is echoed
// in a response or a log line.
func truncateForMessage(v string, max int) string {
	if len(v) <= max {
		return v
	}
	return v[:max] + "..."
}

// misdirectedHostLog logs the first 421 per received host at WARN, so an
// operator whose proxy rewrites Host can find the cause without a
// debugger. It remembers at most misdirectedHostLogCap hosts, and every
// line also draws from one overall limiter, so a client spraying distinct
// Host values cannot flood the log.
type misdirectedHostLog struct {
	mu      sync.Mutex
	seen    map[string]struct{}
	limiter *rate.Limiter
}

const misdirectedHostLogCap = 1024

func (l *misdirectedHostLog) warnOnce(host, source, origin, peer string) {
	l.mu.Lock()
	if l.seen == nil {
		l.seen = map[string]struct{}{}
		// Five lines at once, then one a minute.
		l.limiter = rate.NewLimiter(rate.Every(time.Minute), 5)
	}
	if _, ok := l.seen[host]; ok || !l.limiter.Allow() {
		l.mu.Unlock()
		return
	}
	if len(l.seen) < misdirectedHostLogCap {
		l.seen[host] = struct{}{}
	}
	l.mu.Unlock()
	slog.Warn("mcp: refused a request whose host is not a configured address (421); if Pad is behind a proxy, preserve the Host header or list the proxy in PAD_TRUSTED_PROXIES",
		"received_host", host, "host_source", source, "configured_origin", origin, "peer", peer)
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
