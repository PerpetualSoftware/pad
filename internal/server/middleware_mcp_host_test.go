package server

import "testing"

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
