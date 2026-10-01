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
	req.RemoteAddr = "10.0.0.5:12345" // in trusted CIDR
	req.Header.Set("X-Real-IP", "198.51.100.7")
	mw.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "198.51.100.7" {
		t.Fatalf("X-Real-IP from trusted peer was ignored: %s", seen)
	}
}

func TestTrustedProxyRealIP_TrustedPeer_XFFTrustedHopSkipped(t *testing.T) {
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
		t.Fatalf("XFF client behind a trusted hop not honored: %s", seen)
	}
}

func TestCapturePeerAddr_PreservesOriginalRemoteAddr(t *testing.T) {
	// CapturePeerAddr installs the raw peer into context BEFORE TrustedProxyRealIP
	// has a chance to rewrite it — so downstream handlers can always fetch the
	// real TCP peer, even on deployments with trusted proxies.
	const realPeer = "10.0.0.5:12345"
	var seenCtx, seenRemoteAddr string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenCtx = rawPeerAddr(r)
		seenRemoteAddr = r.RemoteAddr
	})
	cidrs := ParseTrustedProxyCIDRs("10.0.0.0/8")
	chain := CapturePeerAddr(TrustedProxyRealIP(cidrs)(next))

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = realPeer
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	chain.ServeHTTP(httptest.NewRecorder(), req)

	if seenRemoteAddr != "198.51.100.7" {
		t.Fatalf("expected RemoteAddr rewritten to XFF value, got %q", seenRemoteAddr)
	}
	if seenCtx != realPeer {
		t.Fatalf("expected context to preserve raw peer %q, got %q", realPeer, seenCtx)
	}
}

func TestRawPeerAddr_FallsBackToRemoteAddrWithoutMiddleware(t *testing.T) {
	// In tests (or any call path that skips CapturePeerAddr), rawPeerAddr
	// falls back to r.RemoteAddr rather than returning empty.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.0.2.1:9999"
	if got := rawPeerAddr(req); got != "192.0.2.1:9999" {
		t.Fatalf("rawPeerAddr fallback = %q, want %q", got, "192.0.2.1:9999")
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

// BUG-3323: from a trusted peer, the client is the first X-Forwarded-For hop
// outside the trusted CIDRs counted from the RIGHT, and X-Forwarded-For wins
// over X-Real-IP. Every proxy appends the address it received from, so the
// entries left of the first untrusted one, and an X-Real-IP that an
// append-only proxy passed through, are whatever the client sent. The rows
// marked "spoof" are the shapes the leftmost-entry, X-Real-IP-first parse
// resolved to the attacker's chosen address.
func TestTrustedProxyRealIP_BUG3323_ResolvesRightmostUntrustedHop(t *testing.T) {
	cases := []struct {
		name      string
		xff, xrip string
		want      string
	}{
		{"spoof: client-supplied entry left of the appended client", "6.6.6.6, 198.51.100.7", "", "198.51.100.7"},
		{"spoof: client X-Real-IP passed through by an XFF-only proxy", "198.51.100.7", "6.6.6.6", "198.51.100.7"},
		{"cloud shape: router overwrote X-Real-IP and appended the client", "6.6.6.6, 198.51.100.7, 198.51.100.7", "198.51.100.7", "198.51.100.7"},
		{"several trusted hops are skipped", "198.51.100.7, 10.0.0.9, 10.0.0.5", "", "198.51.100.7"},
		{"every hop trusted: the leftmost", "10.0.0.9, 10.0.0.8", "", "10.0.0.9"},
		{"spoof: Azure-style ip:port entry", "6.6.6.6, 198.51.100.7:51234", "", "198.51.100.7"},
		{"bracketed IPv6 with port", "[2001:db8::1]:443", "", "2001:db8::1"},
		{"an unparseable entry stops the walk at the last address read", "198.51.100.7, garbage, 10.0.0.9", "", "10.0.0.9"},
		{"only garbage: the peer is kept", "garbage", "", "10.0.0.5:12345"},
		{"no XFF: X-Real-IP", "", "198.51.100.7", "198.51.100.7"},
		{"blank XFF falls back to X-Real-IP", "  ", "198.51.100.7", "198.51.100.7"},
	}
	cidrs := ParseTrustedProxyCIDRs("10.0.0.0/8")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			mw := TrustedProxyRealIP(cidrs)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.RemoteAddr
			}))
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = "10.0.0.5:12345"
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.xrip != "" {
				req.Header.Set("X-Real-IP", tc.xrip)
			}
			mw.ServeHTTP(httptest.NewRecorder(), req)
			if seen != tc.want {
				t.Errorf("resolved %q, want %q", seen, tc.want)
			}
		})
	}
}

// A proxy that appends its hop as a SEPARATE X-Forwarded-For field leaves
// the client's own value in the first field, the only one Header.Get reads.
func TestTrustedProxyRealIP_BUG3323_ReadsEveryXFFField(t *testing.T) {
	var seen string
	mw := TrustedProxyRealIP(ParseTrustedProxyCIDRs("10.0.0.0/8"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.5:12345"
	req.Header.Add("X-Forwarded-For", "6.6.6.6")
	req.Header.Add("X-Forwarded-For", "198.51.100.7")
	mw.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "198.51.100.7" {
		t.Errorf("resolved %q from two XFF fields, want the appended client 198.51.100.7", seen)
	}
}

// The same headers from an UNTRUSTED peer change nothing, walk or no walk.
func TestTrustedProxyRealIP_BUG3323_UntrustedPeerStillIgnored(t *testing.T) {
	var seen string
	mw := TrustedProxyRealIP(ParseTrustedProxyCIDRs("10.0.0.0/8"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.9:4000"
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.7")
	req.Header.Set("X-Real-IP", "6.6.6.6")
	mw.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "203.0.113.9:4000" {
		t.Errorf("an untrusted peer's headers were honoured: %q", seen)
	}
}
