package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
)

// peerAddrCtxKey carries the untampered TCP peer address (the original
// r.RemoteAddr) through the request context. Middleware downstream of
// TrustedProxyRealIP can't read the raw address off r.RemoteAddr anymore
// because the rewrite is already baked in.
type peerAddrCtxKey struct{}

// CapturePeerAddr must be installed BEFORE TrustedProxyRealIP in the chain.
// It snapshots the real TCP peer RemoteAddr into the request context so
// authenticity checks (e.g. bootstrap loopback) can verify the actual wire
// peer even when a trusted proxy has rewritten r.RemoteAddr.
func CapturePeerAddr(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), peerAddrCtxKey{}, r.RemoteAddr)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// rawPeerAddr returns the untampered TCP peer address captured by
// CapturePeerAddr, falling back to the current r.RemoteAddr if the
// middleware wasn't installed (e.g. in tests). Never use r.RemoteAddr
// directly for authenticity decisions — use this.
func rawPeerAddr(r *http.Request) string {
	if v, ok := r.Context().Value(peerAddrCtxKey{}).(string); ok && v != "" {
		return v
	}
	return r.RemoteAddr
}

// TrustedProxyRealIP returns middleware that rewrites r.RemoteAddr from
// X-Real-IP / X-Forwarded-For ONLY when the direct TCP peer is within one
// of the supplied trusted-proxy CIDRs. This replaces chimiddleware.RealIP
// which trusts proxy headers unconditionally — that is unsafe for any
// deployment directly exposed to untrusted networks, because any client
// can spoof X-Forwarded-For to bypass IP rate limits, the bootstrap
// loopback check, and IP-based audit logs.
//
// Pass cidrs == nil (the default when PAD_TRUSTED_PROXIES is unset) to
// disable proxy-header trust entirely; the TCP peer address is then used
// everywhere, which is the safe behavior for direct-exposed servers.
func TrustedProxyRealIP(cidrs []*net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(cidrs) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			peerIP := peerAddr(r.RemoteAddr)
			if peerIP == nil || !ipInCIDRs(peerIP, cidrs) {
				next.ServeHTTP(w, r)
				return
			}

			// Peer is a trusted proxy. X-Forwarded-For wins when present,
			// resolved by forwardedClientIP; X-Real-IP is read only when
			// there is no X-Forwarded-For (BUG-3323). The order matters: a
			// proxy that only APPENDS to X-Forwarded-For passes a client's
			// own X-Real-IP through untouched, so preferring X-Real-IP, or
			// taking the leftmost X-Forwarded-For entry as this used to,
			// let any client behind such a proxy choose its address.
			var realIP string
			// Every X-Forwarded-For field, in order: a proxy may append its
			// hop as a SECOND field rather than to the client's, and
			// Header.Get would read only the client's (BUG-3323 review).
			if v := strings.Join(r.Header.Values("X-Forwarded-For"), ","); strings.TrimSpace(v) != "" {
				realIP = forwardedClientIP(v, cidrs)
			} else if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
				realIP = v
			}

			if realIP != "" && net.ParseIP(realIP) != nil {
				r.RemoteAddr = realIP
			}
			// PLAN-2310 DR-9: every per-client limit keys on the address
			// resolved here. If a proxied request resolves to an address
			// that is itself inside the trusted range (the proxy forwards
			// no client address, or forwards its own), every client shares
			// one bucket and the limits throttle the whole deployment. Say
			// so once, loudly, on the first such request.
			if resolved := peerAddr(r.RemoteAddr); resolved != nil && ipInCIDRs(resolved, cidrs) {
				warnResolvedInTrustedRange.Do(func() {
					slog.Warn("rate limits: a proxied request resolved to an address inside PAD_TRUSTED_PROXIES, so per-client limits would key on the proxy and throttle every client together; make the proxy send X-Forwarded-For or X-Real-IP with the client address",
						"resolved", resolved.String())
				})
			}
			next.ServeHTTP(w, r)
		})
	}
}

// warnResolvedInTrustedRange makes TrustedProxyRealIP's misconfiguration
// warning fire once per process.
var warnResolvedInTrustedRange sync.Once

// ParseTrustedProxyCIDRs parses a comma-separated list of CIDRs or bare
// IPs from the PAD_TRUSTED_PROXIES setting. Bare IPs get /32 (IPv4) or
// /128 (IPv6). Invalid entries are logged and skipped so an operator typo
// can't crash startup — but if the result is empty, proxy headers remain
// untrusted.
func ParseTrustedProxyCIDRs(spec string) []*net.IPNet {
	if spec == "" {
		return nil
	}
	var out []*net.IPNet
	for _, raw := range strings.Split(spec, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// Accept bare IPs by appending /32 or /128.
		if !strings.Contains(raw, "/") {
			ip := net.ParseIP(raw)
			if ip == nil {
				slog.Warn("PAD_TRUSTED_PROXIES: skipping invalid entry", "entry", raw)
				continue
			}
			if ip.To4() != nil {
				raw += "/32"
			} else {
				raw += "/128"
			}
		}
		_, cidr, err := net.ParseCIDR(raw)
		if err != nil {
			slog.Warn("PAD_TRUSTED_PROXIES: skipping invalid CIDR", "entry", raw, "error", err)
			continue
		}
		out = append(out, cidr)
	}
	return out
}

// peerAddr extracts the IP from a RemoteAddr string, which may be either
// "host:port" (the stdlib default) or a bare "host" (what some middleware
// leaves behind after its own rewriting). Returns nil if parsing fails.
func peerAddr(remoteAddr string) net.IP {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	return net.ParseIP(host)
}

func ipInCIDRs(ip net.IP, cidrs []*net.IPNet) bool {
	for _, c := range cidrs {
		if c.Contains(ip) {
			return true
		}
	}
	return false
}

// forwardedClientIP resolves the client address from an X-Forwarded-For
// chain the way nginx's real_ip_recursive does (BUG-3323): walk it right to
// left, skipping hops inside the trusted CIDRs, and take the first address
// outside them. Every proxy APPENDS the address it received the request
// from, so only the entries to the right of the first untrusted one were
// written by proxies we trust; anything to its left came from the client.
// When every entry is trusted, the leftmost is the best answer, and the
// caller's in-trusted-range warning reports it. An entry that does not parse
// as an IP stops the walk: nothing to its left can be vouched for, so the
// last address that did parse is returned, and "" when none did.
func forwardedClientIP(xff string, cidrs []*net.IPNet) string {
	parts := strings.Split(xff, ",")
	last := ""
	for i := len(parts) - 1; i >= 0; i-- {
		ip := parseForwardedIP(parts[i])
		if ip == nil {
			return last
		}
		last = ip.String()
		if !ipInCIDRs(ip, cidrs) {
			return last
		}
	}
	return last
}

// parseForwardedIP reads one X-Forwarded-For entry. Some proxies append the
// peer's port, as "203.0.113.7:51234" or "[2001:db8::1]:51234" (Azure
// Application Gateway does), so an entry that is not a bare address is
// retried as host:port.
func parseForwardedIP(entry string) net.IP {
	entry = strings.TrimSpace(entry)
	if ip := net.ParseIP(entry); ip != nil {
		return ip
	}
	if host, _, err := net.SplitHostPort(entry); err == nil {
		return net.ParseIP(host)
	}
	return nil
}
