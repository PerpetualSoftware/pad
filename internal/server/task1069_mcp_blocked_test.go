package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// TASK-1069: MCP that is turned on but cannot be served says so: one reason
// for the admin panel, the startup error and the health flag, so the three
// cannot disagree. Cloud never reports blocked here; it refuses to start
// instead (cmd/pad validateCloudMCPOAuth).

func healthBody(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/health", nil)
	if rr.Code != 200 {
		t.Fatalf("health status %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	return out
}

func TestTASK1069_MCPBlockedReason(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name       string
		cfg        config.Config
		env        *bool
		cloud      bool
		wantReason string // substring; "" means not blocked
	}{
		{"off: never blocked", config.Config{}, &off, false, ""},
		{"on, no origin: blocked, naming PAD_URL", config.Config{}, &on, false, "PAD_URL"},
		{"on, unusable PAD_URL: blocked with that problem", config.Config{URL: "not a url"}, &on, false, `PAD_URL "not a url"`},
		{"on, usable http origin: served (PAT-only is valid)", config.Config{URL: "http://pad.local:7777"}, &on, false, ""},
		{"on, usable https origin: served", config.Config{URL: "https://pad.example.com"}, &on, false, ""},
		{"cloud, no origin: not reported here (cloud refuses to start)", config.Config{}, nil, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, token := mcpServer(t, tc.cfg, tc.env)
			if tc.cloud {
				srv.SetCloudMode("test-cloud-secret")
			}
			reason := srv.MCPBlockedReason()
			flag, hasFlag := healthBody(t, srv)["mcp_blocked"]
			if tc.wantReason == "" {
				if reason != "" {
					t.Fatalf("reason = %q, want not blocked", reason)
				}
				if hasFlag {
					t.Fatalf("health carries mcp_blocked=%v, want the key absent", flag)
				}
				return
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("reason = %q, want it to contain %q", reason, tc.wantReason)
			}
			if flag != true {
				t.Fatalf("health mcp_blocked = %v, want true", flag)
			}
			// One rule: the admin panel's blocked text is the same reason.
			got := decodeMCPSettings(t, doRequestWithCookie(srv, "GET", "/api/v1/admin/mcp", nil, token))
			if got.Readiness.State != "blocked" || got.Readiness.Blocked != reason {
				t.Fatalf("panel = %q / %q, want blocked / %q", got.Readiness.State, got.Readiness.Blocked, reason)
			}
		})
	}
}

// The health flag says only THAT MCP is blocked. The reason names
// configuration and stays on the admin endpoint.
func TestTASK1069_HealthDoesNotLeakTheReason(t *testing.T) {
	on := true
	srv, _ := mcpServer(t, config.Config{URL: "not a url"}, &on)
	body := healthBody(t, srv)
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "not a url") || strings.Contains(string(raw), "PAD_URL") {
		t.Fatalf("health leaks configuration: %s", raw)
	}
}
