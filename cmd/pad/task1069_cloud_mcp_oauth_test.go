package main

import (
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// TASK-1069: a cloud start that cannot construct the MCP OAuth server is
// refused, naming what to set, instead of serving /mcp PAT-only in silence.

func TestTASK1069_ValidateCloudMCPOAuth(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		wantErr string // substring; "" means accepted
	}{
		{"https PUBLIC_URL (pad-cloud's shape) starts", config.Config{PublicURL: "https://app.example.com"}, ""},
		{"https PAD_URL starts", config.Config{URL: "https://app.example.com"}, ""},
		{"https overrides with an https origin start", config.Config{
			URL: "https://app.example.com", MCPPublicURL: "https://mcp.example.com", AuthServerURL: "https://app.example.com",
		}, ""},
		{"no origin is refused, naming PAD_URL", config.Config{}, "no public origin is configured"},
		{"an http origin is refused (OAuth needs https)", config.Config{URL: "http://app.example.com"}, "is not https"},
		{"an unusable value is refused with the problem", config.Config{URL: "not a url"}, `PAD_URL "not a url"`},
		// Combinations (codex r1). PAD_URL wins over PUBLIC_URL, so an http
		// PAD_URL leaves no https issuer: OAuth was never constructed in this
		// shape, and the refusal says so instead of serving PAT-only.
		{"http PAD_URL beside an https PUBLIC_URL is refused", config.Config{
			URL: "http://10.0.0.5:7777", PublicURL: "https://app.example.com",
		}, "is not https"},
		// An explicit https issuer makes OAuth constructible again, so it starts.
		{"http PAD_URL with an explicit https PAD_AUTH_SERVER_URL starts", config.Config{
			URL: "http://10.0.0.5:7777", AuthServerURL: "https://app.example.com", MCPPublicURL: "https://mcp.example.com",
		}, ""},
		// An https MCP URL does not rescue an http issuer.
		{"https PAD_MCP_PUBLIC_URL with an http issuer is refused", config.Config{
			URL: "http://10.0.0.5:7777", MCPPublicURL: "https://mcp.example.com",
		}, "is not https"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCloudMCPOAuth(tc.cfg.ResolveMCPEndpoints())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted a cloud start that cannot construct MCP OAuth")
			}
			if !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "PAD_URL") {
				t.Fatalf("error %q should contain %q and name PAD_URL", err, tc.wantErr)
			}
		})
	}
}

// CONVE-19: the wiring is a claim. The refusal sits in the cloud block and
// the self-host error runs off cloud; a test of the function passes with
// either call deleted.
func TestTASK1069_ServerStartWiresBothChecks(t *testing.T) {
	src, err := os.ReadFile("cmd_server.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	cloud := strings.Index(body, "if cfg.IsCloudServer() {\n\t\t\t\tif cfg.CloudSecret == \"\"")
	if cloud < 0 {
		t.Fatal("cannot find the cloud block; update this guard")
	}
	if !strings.Contains(body[cloud:cloud+1500], "validateCloudMCPOAuth(mcpEndpoints)") {
		t.Error("the cloud block no longer calls validateCloudMCPOAuth(mcpEndpoints)")
	}
	if !strings.Contains(body, "srv.MCPBlockedReason()") {
		t.Error("server start no longer reports a blocked MCP at startup")
	}
}

// pad-cloud's PRODUCTION shape, through the real environment loading (lead
// ruling): its compose sets PAD_MCP_PUBLIC_URL=${MCP_PUBLIC_URL:-}, so the
// variable is PRESENT but EMPTY, beside PUBLIC_URL and PAD_AUTH_SERVER_URL
// set to the https domain, and no PAD_URL. A refused start on Cloud is an
// outage, so this shape must start, read the way the server reads it.
func TestTASK1069_PadCloudProductionEnvStarts(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no config.toml can contribute
	t.Setenv("PAD_URL", "")
	t.Setenv("PUBLIC_URL", "https://app.getpad.dev")
	t.Setenv("PAD_MCP_PUBLIC_URL", "")
	t.Setenv("PAD_AUTH_SERVER_URL", "https://app.getpad.dev")
	t.Setenv("PAD_MODE", "cloud")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ep := cfg.ResolveMCPEndpoints()
	if err := validateCloudMCPOAuth(ep); err != nil {
		t.Fatalf("pad-cloud's production env is refused: %v", err)
	}
	if ep.ResourceURL != "https://app.getpad.dev/mcp" || ep.AuthServerURL != "https://app.getpad.dev" {
		t.Fatalf("resolved MCP URL %q / issuer %q, want the PUBLIC_URL-derived https pair", ep.ResourceURL, ep.AuthServerURL)
	}
}
