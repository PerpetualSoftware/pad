package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRequestIsLoopback_IgnoresRealIPRewrite verifies that the bootstrap
// loopback check reads the untampered TCP peer captured by
// CapturePeerAddr, not r.RemoteAddr. A proxied attacker setting
// X-Forwarded-For: 127.0.0.1 must not be able to trick the check.
func TestRequestIsLoopback_IgnoresRealIPRewrite(t *testing.T) {
	tests := []struct {
		name         string
		peer         string
		spoofedXFF   string
		spoofedXRI   string // X-Real-IP
		trustedCIDRs string
		wantLoopback bool
	}{
		{
			name:         "direct loopback peer, no proxy headers: allowed",
			peer:         "127.0.0.1:54321",
			wantLoopback: true,
		},
		{
			name:         "direct LAN peer: rejected",
			peer:         "192.168.1.5:54321",
			wantLoopback: false,
		},
		{
			name:         "loopback peer WITH X-Forwarded-For: rejected (proxy relay)",
			peer:         "127.0.0.1:54321",
			spoofedXFF:   "203.0.113.8",
			wantLoopback: false,
		},
		{
			name:         "loopback peer WITH X-Real-IP: rejected (proxy relay)",
			peer:         "127.0.0.1:54321",
			spoofedXRI:   "203.0.113.8",
			wantLoopback: false,
		},
		{
			name:         "loopback peer with spoofed XFF=127.0.0.1: rejected (any XFF rejects)",
			peer:         "127.0.0.1:54321",
			spoofedXFF:   "127.0.0.1",
			wantLoopback: false,
		},
		{
			name:         "trusted proxy forwarding spoofed XFF=127.0.0.1: rejected",
			peer:         "10.0.0.5:54321",
			spoofedXFF:   "127.0.0.1",
			trustedCIDRs: "10.0.0.0/8",
			wantLoopback: false,
		},
		{
			name:         "untrusted peer with spoofed XFF=127.0.0.1: rejected",
			peer:         "203.0.113.7:54321",
			spoofedXFF:   "127.0.0.1",
			trustedCIDRs: "10.0.0.0/8",
			wantLoopback: false,
		},
		{
			name:         "IPv6 loopback peer, no proxy headers: allowed",
			peer:         "[::1]:54321",
			wantLoopback: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got bool
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = requestIsLoopback(r)
			})
			// Replicate the production middleware order: CapturePeerAddr, then TrustedProxyRealIP.
			cidrs := ParseTrustedProxyCIDRs(tt.trustedCIDRs)
			chain := CapturePeerAddr(TrustedProxyRealIP(cidrs)(handler))

			req := httptest.NewRequest("GET", "/api/v1/auth/bootstrap", nil)
			req.RemoteAddr = tt.peer
			if tt.spoofedXFF != "" {
				req.Header.Set("X-Forwarded-For", tt.spoofedXFF)
			}
			if tt.spoofedXRI != "" {
				req.Header.Set("X-Real-IP", tt.spoofedXRI)
			}
			chain.ServeHTTP(httptest.NewRecorder(), req)

			if got != tt.wantLoopback {
				t.Fatalf("requestIsLoopback = %v, want %v", got, tt.wantLoopback)
			}
		})
	}
}
