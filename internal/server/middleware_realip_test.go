package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseTrustedProxyCIDRs(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		wantLen  int
		contains []string // IPs that should match
		rejects  []string // IPs that should not match
	}{
		{
			name:    "empty spec disables proxy trust",
			spec:    "",
			wantLen: 0,
		},
		{
			name:     "single CIDR",
			spec:     "10.0.0.0/8",
			wantLen:  1,
			contains: []string{"10.0.0.1", "10.255.255.255"},
			rejects:  []string{"11.0.0.1", "192.168.1.1"},
		},
		{
			name:     "bare IPv4 becomes /32",
			spec:     "192.168.1.50",
			wantLen:  1,
			contains: []string{"192.168.1.50"},
			rejects:  []string{"192.168.1.51"},
		},
		{
			name:     "bare IPv6 becomes /128",
			spec:     "::1",
			wantLen:  1,
			contains: []string{"::1"},
			rejects:  []string{"::2"},
		},
		{
			name:     "mixed CIDRs and IPs",
			spec:     "10.0.0.0/8, 172.16.0.0/12, 192.168.1.1",
			wantLen:  3,
			contains: []string{"10.1.1.1", "172.16.0.5", "192.168.1.1"},
			rejects:  []string{"8.8.8.8", "192.168.1.2"},
		},
		{
			name:    "invalid entries are skipped",
			spec:    "10.0.0.0/8, not-an-ip, 999.999.999.999",
			wantLen: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseTrustedProxyCIDRs(tt.spec)
			if len(got) != tt.wantLen {
				t.Fatalf("len(cidrs) = %d, want %d", len(got), tt.wantLen)
			}
			for _, ip := range tt.contains {
				if !ipInCIDRs(net.ParseIP(ip), got) {
					t.Errorf("%s should match", ip)
				}
			}
			for _, ip := range tt.rejects {
				if ipInCIDRs(net.ParseIP(ip), got) {
					t.Errorf("%s should NOT match", ip)
				}
			}
		})
	}
}

func TestTrustedProxyRealIP_NoProxies_HeadersIgnored(t *testing.T) {
	// When no trusted proxies are configured, proxy headers are completely
	// ignored and the real TCP peer address wins.
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	})
	mw := TrustedProxyRealIP(nil)(next)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.5:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.7") // attacker-spoofed
	mw.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "203.0.113.5:12345" {
		t.Fatalf("RemoteAddr was rewritten despite empty trust list: %s", seen)
	}
}

func TestTrustedProxyRealIP_UntrustedPeer_HeadersIgnored(t *testing.T) {
	// Proxy headers from an untrusted peer must be ignored even when the
	// trust list is non-empty.
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	})
	cidrs := ParseTrustedProxyCIDRs("10.0.0.0/8")
	mw := TrustedProxyRealIP(cidrs)(next)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.5:12345" // NOT in 10/8
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	mw.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "203.0.113.5:12345" {
		t.Fatalf("RemoteAddr was rewritten for untrusted peer: %s", seen)
	}
}

func TestTrustedProxyRealIP_TrustedPeer_XRealIPUsed(t *testing.T) {
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	})
	cidrs := ParseTrustedProxyCIDRs("10.0.0.0/8")
	mw := TrustedProxyRealIP(cidrs)(next)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.5:12345"       // in trusted CIDR
	req.Header.Set("X-Real-IP", "198.51.100.7")
	mw.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "198.51.100.7" {
		t.Fatalf("X-Real-IP from trusted peer was ignored: %s", seen)
	}
}

func TestTrustedProxyRealIP_TrustedPeer_XFFFirstEntryUsed(t *testing.T) {
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	})
	cidrs := ParseTrustedProxyCIDRs("10.0.0.0/8")
	mw := TrustedProxyRealIP(cidrs)(next)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.5:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.5")
	mw.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "198.51.100.7" {
		t.Fatalf("XFF first entry not honored: %s", seen)
	}
}

func TestTrustedProxyRealIP_TrustedPeer_InvalidHeaderIgnored(t *testing.T) {
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	})
	cidrs := ParseTrustedProxyCIDRs("10.0.0.0/8")
	mw := TrustedProxyRealIP(cidrs)(next)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.5:12345"
	req.Header.Set("X-Real-IP", "not-an-ip")
	mw.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "10.0.0.5:12345" {
		t.Fatalf("invalid header was trusted: %s", seen)
	}
}
