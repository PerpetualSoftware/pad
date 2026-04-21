package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
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

			// Peer is a trusted proxy — accept X-Real-IP or the first
			// entry of X-Forwarded-For as the client IP.
			var realIP string
			if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
				realIP = v
			} else if v := r.Header.Get("X-Forwarded-For"); v != "" {
				for _, p := range strings.Split(v, ",") {
					p = strings.TrimSpace(p)
					if p != "" {
						realIP = p
						break
					}
				}
			}

			if realIP != "" && net.ParseIP(realIP) != nil {
				r.RemoteAddr = realIP
			}
			next.ServeHTTP(w, r)
		})
	}
}

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
