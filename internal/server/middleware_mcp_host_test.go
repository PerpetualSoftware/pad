package server

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// warnOncePerKey allows the first line per key, never a repeat, and no
// more than the overall burst for distinct keys, so a client spraying
// Host values (or addresses) cannot flood the log.
func TestWarnOncePerKey_FirstPerKeyRateLimited(t *testing.T) {
	var l warnOncePerKey
	if !l.allow("a.example") || l.allow("a.example") {
		t.Fatal("same key twice: want allowed once")
	}
	n := 0
	for i := 0; i < 20; i++ {
		if l.allow(fmt.Sprintf("h%d.example", i)) {
			n++
		}
	}
	if n != 4 {
		t.Fatalf("20 more distinct keys: %d allowed, want 4 (the burst of 5, one already spent)", n)
	}
}

// The 421 logs its WARN through warnOncePerKey: one line for a host
// refused twice.
func TestMisdirectedHostWarnIsLoggedOncePerHost(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	srv := testServer(t)
	srv.SetMCPConfig(testHTTPSEndpoints(), boolPtrForTest(true))
	h := srv.requireConfiguredHost(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/mcp", nil)
		req.Host = "evil.example"
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if n := strings.Count(buf.String(), "received_host=evil.example"); n != 1 {
		t.Fatalf("a host refused twice: %d WARN lines, want 1\n%s", n, buf.String())
	}
}

func boolPtrForTest(b bool) *bool { return &b }

// PLAN-2310 DR-6 matching rules for the Host allowlist.
func TestHostMatchesURL(t *testing.T) {
	cases := []struct {
		host, configured string
		want             bool
	}{
		// Exact, case-insensitive, port defaulted from the configured scheme.
		{"pad.example.com", "https://pad.example.com", true},
		{"PAD.Example.COM", "https://pad.example.com", true},
		{"pad.example.com:443", "https://pad.example.com", true},
		{"pad.example.com", "https://pad.example.com:443", true},
		{"pad.example.com:80", "https://pad.example.com", false},
		{"pad.example.com:8443", "https://pad.example.com:8443/mcp", true},
		{"pad.example.com", "https://pad.example.com:8443", false},
		{"pad.lan:7777", "http://pad.lan:7777", true},
		{"pad.lan", "http://pad.lan:7777", false},
		{"pad.lan", "http://pad.lan", true},
		// A different host, a suffix, a trailing dot: all refused.
		{"evil.example", "https://pad.example.com", false},
		{"pad.example.com.evil.example", "https://pad.example.com", false},
		{"pad.example.com.", "https://pad.example.com", false},
		{"", "https://pad.example.com", false},
		// Loopback: any loopback spelling with the configured port.
		{"localhost:7777", "http://127.0.0.1:7777", true},
		{"127.0.0.1:7777", "http://localhost:7777", true},
		{"[::1]:7777", "http://localhost:7777", true},
		{"127.0.0.2:7777", "http://localhost:7777", true},
		{"LOCALHOST:7777", "http://localhost:7777", true},
		{"localhost:7778", "http://localhost:7777", false},
		{"localhost", "http://localhost:7777", false},
		// A non-loopback origin does not admit loopback Hosts.
		{"localhost:443", "https://pad.example.com", false},
		{"127.0.0.1", "http://pad.lan", false},
		// IPv6 literals.
		{"[fd00::1]:7777", "http://[FD00::1]:7777", true},
		{"[fd00::1]", "http://[fd00::1]", true},
		{"[fd00::2]:7777", "http://[fd00::1]:7777", false},
	}
	for _, tc := range cases {
		if got := hostMatchesURL(tc.host, tc.configured); got != tc.want {
			t.Errorf("hostMatchesURL(%q, %q) = %v, want %v", tc.host, tc.configured, got, tc.want)
		}
	}
}
