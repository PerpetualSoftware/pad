package server

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// The first 421 per host is logged at WARN; repeats of a host are not,
// and distinct hosts beyond the overall burst are not either, so a client
// spraying Host values cannot flood the log.
func TestMisdirectedHostLog_FirstPerHostRateLimited(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	var l misdirectedHostLog
	l.warnOnce("a.example", "Host", "https://pad.example.com", "192.0.2.1:1")
	l.warnOnce("a.example", "Host", "https://pad.example.com", "192.0.2.1:1")
	if n := strings.Count(buf.String(), "received_host=a.example"); n != 1 {
		t.Fatalf("same host twice: %d lines, want 1", n)
	}
	for i := 0; i < 20; i++ {
		l.warnOnce(fmt.Sprintf("h%d.example", i), "Host", "https://pad.example.com", "192.0.2.1:1")
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 5 {
		t.Fatalf("21 distinct hosts: %d WARN lines, want 5 (the overall burst)", n)
	}
}

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
