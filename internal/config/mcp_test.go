package config

import (
	"strings"
	"testing"
)

// PLAN-2310 DR-3: MCP addressing comes from configuration only, never from
// a request, and an unusable value is reported, never fatal.
func TestResolveMCPEndpoints(t *testing.T) {
	cases := []struct {
		name                         string
		cfg                          Config
		origin, resource, authServer string
		usable, https                bool
		problem                      string // substring of the only expected problem, "" for none
	}{
		{name: "nothing configured", cfg: Config{Host: "0.0.0.0", Port: 7777}},
		{
			name: "PAD_URL https", cfg: Config{URL: "https://pad.example.com/"},
			origin: "https://pad.example.com", resource: "https://pad.example.com/mcp", authServer: "https://pad.example.com",
			usable: true, https: true,
		},
		{
			name: "PUBLIC_URL http when PAD_URL is unset", cfg: Config{PublicURL: "http://pad.lan:7777"},
			origin: "http://pad.lan:7777", resource: "http://pad.lan:7777/mcp", authServer: "http://pad.lan:7777",
			usable: true,
		},
		{
			name: "PAD_URL wins over PUBLIC_URL", cfg: Config{URL: "https://a.example", PublicURL: "https://b.example"},
			origin: "https://a.example", resource: "https://a.example/mcp", authServer: "https://a.example",
			usable: true, https: true,
		},
		{
			name: "cloud's three variables", cfg: Config{PublicURL: "https://app.getpad.dev", MCPPublicURL: "https://mcp.getpad.dev", AuthServerURL: "https://app.getpad.dev"},
			origin: "https://app.getpad.dev", resource: "https://mcp.getpad.dev", authServer: "https://app.getpad.dev",
			usable: true, https: true,
		},
		{
			name: "https origin but an http auth server is PAT-only", cfg: Config{URL: "https://pad.example.com", AuthServerURL: "http://auth.internal"},
			origin: "https://pad.example.com", resource: "https://pad.example.com/mcp", authServer: "http://auth.internal",
			usable: true,
		},
		{
			name: "an origin with a path is unusable", cfg: Config{URL: "https://pad.example.com/pad"},
			problem: "PAD_URL",
		},
		{
			name: "a non-http scheme is unusable", cfg: Config{PublicURL: "ftp://pad.example.com"},
			problem: "PUBLIC_URL",
		},
		{
			name: "a query is unusable", cfg: Config{URL: "https://pad.example.com?x=1"},
			problem: "query",
		},
		{
			name: "an unusable override does not fall back to the origin", cfg: Config{URL: "https://pad.example.com", MCPPublicURL: "mcp.example.com"},
			origin: "https://pad.example.com", authServer: "https://pad.example.com",
			problem: "PAD_MCP_PUBLIC_URL",
		},
		{
			name: "an override without an origin is still unavailable", cfg: Config{MCPPublicURL: "https://mcp.example.com"},
			resource: "https://mcp.example.com",
		},
		{
			name: "scheme and host are canonicalised, default port dropped", cfg: Config{URL: " HTTPS://Pad.Example.COM:443/ "},
			origin: "https://pad.example.com", resource: "https://pad.example.com/mcp", authServer: "https://pad.example.com",
			usable: true, https: true,
		},
		{
			name: "a non-default port is kept", cfg: Config{URL: "http://pad.lan:8080"},
			origin: "http://pad.lan:8080", resource: "http://pad.lan:8080/mcp", authServer: "http://pad.lan:8080",
			usable: true,
		},
		{
			name: "IPv6 host", cfg: Config{URL: "http://[FD00::1]:7777"},
			origin: "http://[fd00::1]:7777", resource: "http://[fd00::1]:7777/mcp", authServer: "http://[fd00::1]:7777",
			usable: true,
		},
		{
			name: "an encoded slash is a path, so the origin is unusable", cfg: Config{URL: "https://pad.example.com/%2F"},
			problem: "path",
		},
		{
			name: "an empty port is unusable", cfg: Config{URL: "https://pad.example.com:"},
			problem: "empty port",
		},
		{
			name: "a zoned IPv6 address is unusable", cfg: Config{URL: "http://[fe80::1%25eth0]:7777"},
			problem: "zoned",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.cfg.ResolveMCPEndpoints()
			if e.Origin != tc.origin || e.ResourceURL != tc.resource || e.AuthServerURL != tc.authServer {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)", e.Origin, e.ResourceURL, e.AuthServerURL, tc.origin, tc.resource, tc.authServer)
			}
			if e.Usable() != tc.usable || e.HTTPS() != tc.https {
				t.Errorf("usable/https = %v/%v, want %v/%v", e.Usable(), e.HTTPS(), tc.usable, tc.https)
			}
			probs := e.Problems()
			switch {
			case tc.problem == "" && len(probs) != 0:
				t.Errorf("problems = %v, want none", probs)
			case tc.problem != "" && (len(probs) != 1 || !strings.Contains(probs[0], tc.problem)):
				t.Errorf("problems = %v, want one naming %q", probs, tc.problem)
			}
		})
	}
}

func TestParseMCPEnabledEnv(t *testing.T) {
	for v, want := range map[string]string{
		"true": "true", "1": "true", "YES": "true", " on ": "true",
		"false": "false", "0": "false", "no": "false", "Off": "false",
		"maybe": "nil", "": "nil",
	} {
		got := "nil"
		if b := parseMCPEnabledEnv(v); b != nil {
			got = map[bool]string{true: "true", false: "false"}[*b]
		}
		if got != want {
			t.Errorf("parseMCPEnabledEnv(%q) = %s, want %s", v, got, want)
		}
	}
}

// Load reads PAD_MCP_ENABLED, keeping an unparseable value for the warning.
func TestLoadReadsPadMCPEnabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_MCP_ENABLED", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCPEnabledEnv == nil || !*cfg.MCPEnabledEnv {
		t.Fatalf("MCPEnabledEnv = %v, want true", cfg.MCPEnabledEnv)
	}

	t.Setenv("PAD_MCP_ENABLED", "sometimes")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCPEnabledEnv != nil || cfg.MCPEnabledEnvRaw != "sometimes" {
		t.Fatalf("unparseable value: env=%v raw=%q, want nil and the raw value", cfg.MCPEnabledEnv, cfg.MCPEnabledEnvRaw)
	}
}

// Cloud's OAuth tokens are bound to the audience cmd_server.go has always
// built: strings.TrimRight(PAD_MCP_PUBLIC_URL, "/"). The canonical form must
// be byte-identical for every value cloud has used, or PLAN-2310 U2's switch
// to the resolved URL would invalidate every issued token.
func TestResolveMCPEndpoints_CloudAudienceUnchanged(t *testing.T) {
	for _, v := range []string{"https://mcp.getpad.dev", "https://mcp.getpad.dev/"} {
		c := Config{PublicURL: "https://app.getpad.dev", MCPPublicURL: v, AuthServerURL: "https://app.getpad.dev"}
		e := c.ResolveMCPEndpoints()
		if want := strings.TrimRight(v, "/"); e.ResourceURL != want {
			t.Errorf("PAD_MCP_PUBLIC_URL=%q resolves to %q, want the historical audience %q", v, e.ResourceURL, want)
		}
	}
}
