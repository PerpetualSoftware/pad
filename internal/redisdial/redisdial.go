// Package redisdial supplies the dialer Pad installs on every go-redis client.
//
// IT EXISTS FOR ONE DEFECT, and the defect is in go-redis's default dialer
// rather than in anything Pad wrote (BUG-2754). NewDialer reads, in v9.22.0's
// options.go:
//
//	netDialer := &net.Dialer{Timeout: opt.DialTimeout, KeepAliveConfig: ...}
//	if opt.TLSConfig == nil {
//		return netDialer.DialContext(ctx, network, addr)
//	}
//	return tls.DialWithDialer(netDialer, network, addr, opt.TLSConfig)
//
// The plaintext branch honours the caller's context. The TLS branch does not:
// tls.DialWithDialer takes no context at all, so a cancelled caller cannot
// shorten the dial and it is bounded only by DialTimeout.
//
// WHY THAT MATTERS TO PAD SPECIFICALLY. BUG-2749 put SSE subscription
// establishment on the request's context so a client that disconnects stops
// holding its admission slots, global and per-user. On plaintext that covers
// the dial. On TLS the dial was the one segment cancellation could not reach,
// so the guarantee shrank from "released at once" to "released after up to
// DialTimeout" — and a managed Redis is a rediss:// URL, which is the normal
// production shape rather than an exotic one.
//
// It is a CLIENT-CONSTRUCTION concern, not a subscription one: the same dialer
// serves Publish, the Lua scripts, the presence registry and the watch bus's
// reads. Fixing it inside internal/events would have fixed one caller of a
// shared defect.
package redisdial

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"time"
)

// New returns a dialer that honours the caller's context on TLS as well as
// plaintext, for the given TLS config and dial timeout.
//
// tlsConfig may be nil, which means plaintext; dialTimeout may be zero, which
// means "no per-dial bound of our own" and matches net.Dialer's semantics.
//
// THREE THINGS IT HAS TO GET RIGHT, each of which fails quietly rather than
// loudly if it does not:
//
//  1. SERVERNAME. tls.DialWithDialer infers ServerName from the dialled
//     address when the config leaves it empty (crypto/tls/tls.go: "If no
//     ServerName is set, infer the ServerName from the hostname we're
//     connecting to"). A hand-rolled tls.Client does NOT, and an empty
//     ServerName means certificate verification has no name to check against.
//     That would turn a latency fix into a silent authentication regression,
//     which is the worst trade available here. Replicated below, on a CLONE —
//     mutating the caller's config would leak one host's name into every
//     later dial that shares it.
//
//     redis.ParseURL does populate ServerName for a rediss:// URL (v9.22.0,
//     options.go:708), so Pad's own path does not depend on this fallback
//     today. It is here because the fallback is what the code being replaced
//     did, and a future construction path that leaves ServerName empty must
//     not silently lose verification.
//
//  2. THE TIMEOUT MUST BOUND THE HANDSHAKE, not just the TCP connect. A TLS
//     server that accepts and then stalls mid-handshake would otherwise hang
//     for as long as the context lives — trading a bounded failure for an
//     unbounded one, which is worse than the bug being fixed.
//
//  3. IT MUST NOT EXTEND AN EARLIER DEADLINE. context.WithTimeout takes the
//     sooner of the two, so a caller that is already closer to giving up keeps
//     its own bound. go-redis's pool makes the same promise about DialTimeout
//     ("Apply DialTimeout per attempt, but never extend an existing earlier
//     deadline") and this must not quietly disagree with it.
func New(tlsConfig *tls.Config, dialTimeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		netDialer := &net.Dialer{Timeout: dialTimeout}
		conn, err := netDialer.DialContext(ctx, network, addr)
		if err != nil || tlsConfig == nil {
			return conn, err
		}

		cfg := tlsConfig
		if cfg.ServerName == "" {
			cfg = cfg.Clone()
			cfg.ServerName = hostnameFor(addr)
		}

		handshakeCtx := ctx
		if dialTimeout > 0 {
			var cancel context.CancelFunc
			handshakeCtx, cancel = context.WithTimeout(ctx, dialTimeout)
			defer cancel()
		}

		tlsConn := tls.Client(conn, cfg)
		if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return tlsConn, nil
	}
}

// hostnameFor strips the port from a dial address, matching what
// crypto/tls.DialWithDialer does — LastIndex on ":", so an IPv6 literal keeps
// its brackets exactly as the standard library leaves them. Copied in shape
// rather than improved on: the point is to behave identically to the code this
// replaces, not better than it.
func hostnameFor(addr string) string {
	colon := strings.LastIndex(addr, ":")
	if colon == -1 {
		return addr
	}
	return addr[:colon]
}
