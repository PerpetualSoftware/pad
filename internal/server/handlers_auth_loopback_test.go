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
		trustedCIDRs string
		wantLoopback bool
	}{
		{
			name:         "direct loopback peer returns true",
			peer:         "127.0.0.1:54321",
			wantLoopback: true,
		},
		{
			name:         "direct LAN peer returns false",
			peer:         "192.168.1.5:54321",
			wantLoopback: false,
		},
		{
			name:         "trusted proxy forwarding spoofed 127.0.0.1 is NOT loopback",
			peer:         "10.0.0.5:54321", // real peer in trusted CIDR
			spoofedXFF:   "127.0.0.1",      // attacker-controlled header
			trustedCIDRs: "10.0.0.0/8",
			wantLoopback: false,
		},
		{
			name:         "untrusted peer sending XFF=127.0.0.1 is NOT loopback",
			peer:         "203.0.113.7:54321",
			spoofedXFF:   "127.0.0.1",
			trustedCIDRs: "10.0.0.0/8",
			wantLoopback: false,
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
			chain.ServeHTTP(httptest.NewRecorder(), req)

			if got != tt.wantLoopback {
				t.Fatalf("requestIsLoopback = %v, want %v", got, tt.wantLoopback)
			}
		})
	}
}
