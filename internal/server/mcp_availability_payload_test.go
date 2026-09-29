package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// PLAN-2310 DR-7 (TASK-3303): the session payload and the admin stats carry
// mcp_available and oauth_available, evaluated per request, so the console can
// key Connected Apps on the OAuth path rather than on cloud mode.
func TestMCPAvailabilityInPayloads(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		on         bool
		withOAuth  bool
		mcp, oauth bool
	}{
		{"off", "https://pad.example.com", false, true, false, false},
		{"on, https, OAuth built", "https://pad.example.com", true, true, true, true},
		{"on, http", "http://10.0.0.5:7777", true, false, true, false},
		{"on, no origin", "", true, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, token := mcpServer(t, config.Config{URL: c.url}, nil)
			if c.withOAuth {
				o, err := newTestOAuthServer(t, srv)
				if err != nil {
					t.Fatalf("oauth: %v", err)
				}
				srv.oauthServer = o
			}
			if c.on {
				if err := srv.store.SetPlatformSetting(settingMCPEnabled, "true"); err != nil {
					t.Fatal(err)
				}
			}
			for _, path := range []string{"/api/v1/auth/session", "/api/v1/admin/stats"} {
				rr := doRequestWithCookie(srv, "GET", path, nil, token)
				if rr.Code != http.StatusOK {
					t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
				}
				var got struct {
					MCP   *bool `json:"mcp_available"`
					OAuth *bool `json:"oauth_available"`
				}
				if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				if got.MCP == nil || got.OAuth == nil {
					t.Fatalf("%s: fields missing: %s", path, rr.Body.String())
				}
				if *got.MCP != c.mcp || *got.OAuth != c.oauth {
					t.Errorf("%s: mcp=%v oauth=%v, want %v %v", path, *got.MCP, *got.OAuth, c.mcp, c.oauth)
				}
			}
		})
	}
}
