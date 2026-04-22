package server

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCLIAuthScheme(t *testing.T) {
	// Parse a trusted proxy CIDR once for reuse.
	trustedCIDRs := ParseTrustedProxyCIDRs("10.0.0.0/8")
	if len(trustedCIDRs) != 1 {
		t.Fatalf("fixture: expected 1 CIDR, got %d", len(trustedCIDRs))
	}

	cases := []struct {
		name         string
		tls          bool
		remoteAddr   string
		proto        string
		trustedCIDRs []*net.IPNet
		want         string
	}{
		{
			name:       "TLS terminated at server is always https",
			tls:        true,
			remoteAddr: "203.0.113.5:12345",
			proto:      "http", // ignored
			want:       "https",
		},
		{
			name:         "Plain HTTP with no trusted proxies ignores X-Forwarded-Proto",
			remoteAddr:   "203.0.113.5:12345",
			proto:        "https",
			trustedCIDRs: nil,
			want:         "http",
		},
		{
			name:         "Plain HTTP from untrusted peer ignores X-Forwarded-Proto",
			remoteAddr:   "203.0.113.5:12345",
			proto:        "https",
			trustedCIDRs: trustedCIDRs,
			want:         "http",
		},
		{
			name:         "Plain HTTP from trusted peer with X-Forwarded-Proto=https returns https",
			remoteAddr:   "10.0.0.1:12345",
			proto:        "https",
			trustedCIDRs: trustedCIDRs,
			want:         "https",
		},
		{
			name:         "Plain HTTP from trusted peer with X-Forwarded-Proto=http returns http",
			remoteAddr:   "10.0.0.1:12345",
			proto:        "http",
			trustedCIDRs: trustedCIDRs,
			want:         "http",
		},
		{
			name:         "Plain HTTP from trusted peer with X-Forwarded-Proto=HTTPS (case-insensitive)",
			remoteAddr:   "10.0.0.1:12345",
			proto:        "HTTPS",
			trustedCIDRs: trustedCIDRs,
			want:         "https",
		},
		{
			name:         "Plain HTTP from trusted peer with chained X-Forwarded-Proto takes first value",
			remoteAddr:   "10.0.0.1:12345",
			proto:        "https, http",
			trustedCIDRs: trustedCIDRs,
			want:         "https",
		},
		{
			name:         "Plain HTTP from trusted peer with garbage X-Forwarded-Proto falls back to http",
			remoteAddr:   "10.0.0.1:12345",
			proto:        "javascript:",
			trustedCIDRs: trustedCIDRs,
			want:         "http",
		},
		{
			name:         "Plain HTTP from trusted peer with empty X-Forwarded-Proto returns http",
			remoteAddr:   "10.0.0.1:12345",
			proto:        "",
			trustedCIDRs: trustedCIDRs,
			want:         "http",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/cli/sessions", nil)
			req.Host = "pad.example.com"
			req.RemoteAddr = tc.remoteAddr
			if tc.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			// Mirror what CapturePeerAddr would do in the real chain so that
			// rawPeerAddr returns the untampered peer.
			ctx := context.WithValue(req.Context(), peerAddrCtxKey{}, tc.remoteAddr)
			req = req.WithContext(ctx)

			got := cliAuthScheme(req, tc.trustedCIDRs)
			if got != tc.want {
				t.Errorf("cliAuthScheme() = %q, want %q", got, tc.want)
			}
		})
	}
}
