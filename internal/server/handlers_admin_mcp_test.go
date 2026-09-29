package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-2310 U1: GET/PUT /api/v1/admin/mcp, through the router.

func decodeMCPSettings(t *testing.T, rr *httptest.ResponseRecorder) mcpSettingsResponse {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var out mcpSettingsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, rr.Body.String())
	}
	return out
}

func mcpServer(t *testing.T, cfg config.Config, env *bool) (*Server, string) {
	t.Helper()
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetMCPConfig(cfg.ResolveMCPEndpoints(), env)
	return srv, token
}

// An install that never touched the setting reads as off, from the stored
// setting, unlocked. This is the upgrade case: nothing turns on by itself.
func TestAdminMCP_DefaultIsOff(t *testing.T) {
	srv, token := mcpServer(t, config.Config{URL: "https://pad.example.com"}, nil)
	got := decodeMCPSettings(t, doRequestWithCookie(srv, "GET", "/api/v1/admin/mcp", nil, token))
	if got.Enabled || got.Source != mcpSourceSetting || got.Locked || got.Readiness.State != "off" {
		t.Fatalf("default = %+v, want off from the setting, unlocked", got)
	}
	if srv.mcpAvailable() {
		t.Fatal("mcpAvailable() is true on an install that never enabled MCP")
	}
}

// Turning it on with a usable https origin: on, OAuth and PATs, and the
// DR-1 predicate agrees with the panel.
func TestAdminMCP_EnableHTTPS(t *testing.T) {
	srv, token := mcpServer(t, config.Config{URL: "https://pad.example.com"}, nil)
	got := decodeMCPSettings(t, doRequestWithCookie(srv, "PUT", "/api/v1/admin/mcp", map[string]any{"enabled": true}, token))
	rd := got.Readiness
	if !got.Enabled || rd.State != "on" || !rd.HTTPS || rd.MCPURL != "https://pad.example.com/mcp" {
		t.Fatalf("after enable = %+v", got)
	}
	if len(rd.AuthMethods) != 2 || rd.AuthMethods[0] != "oauth" || rd.AuthMethods[1] != "pat" {
		t.Fatalf("auth methods = %v, want [oauth pat]", rd.AuthMethods)
	}
	if !srv.mcpAvailable() {
		t.Fatal("mcpAvailable() is false with the setting on and an https origin")
	}
	// Persisted, not just echoed.
	if v, _ := srv.store.GetPlatformSetting(settingMCPEnabled); v != "true" {
		t.Fatalf("stored mcp_enabled = %q, want true", v)
	}
}

func TestAdminMCP_HTTPIsPATOnly(t *testing.T) {
	srv, token := mcpServer(t, config.Config{PublicURL: "http://pad.lan:7777"}, nil)
	got := decodeMCPSettings(t, doRequestWithCookie(srv, "PUT", "/api/v1/admin/mcp", map[string]any{"enabled": true}, token))
	if got.Readiness.State != "on" || got.Readiness.HTTPS || len(got.Readiness.AuthMethods) != 1 || got.Readiness.AuthMethods[0] != "pat" {
		t.Fatalf("http deployment = %+v, want on with [pat]", got.Readiness)
	}
}

// On without a usable origin is blocked, with the reason, and not available.
func TestAdminMCP_BlockedWithoutOrigin(t *testing.T) {
	for name, cfg := range map[string]config.Config{
		"no origin":       {Host: "0.0.0.0", Port: 7777},
		"unusable origin": {URL: "https://pad.example.com/sub"},
	} {
		t.Run(name, func(t *testing.T) {
			srv, token := mcpServer(t, cfg, nil)
			got := decodeMCPSettings(t, doRequestWithCookie(srv, "PUT", "/api/v1/admin/mcp", map[string]any{"enabled": true}, token))
			if got.Readiness.State != "blocked" || got.Readiness.Blocked == "" {
				t.Fatalf("readiness = %+v, want blocked with a reason", got.Readiness)
			}
			if srv.mcpAvailable() {
				t.Fatal("mcpAvailable() is true without a usable origin")
			}
		})
	}
}

// PAD_MCP_ENABLED forces the value and locks the toggle: GET reports it,
// PUT is refused 409 set_by_environment and stores nothing.
func TestAdminMCP_EnvironmentLock(t *testing.T) {
	off := false
	srv, token := mcpServer(t, config.Config{URL: "https://pad.example.com"}, &off)
	if err := srv.store.SetPlatformSetting(settingMCPEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	got := decodeMCPSettings(t, doRequestWithCookie(srv, "GET", "/api/v1/admin/mcp", nil, token))
	if got.Enabled || got.Source != mcpSourceEnvironment || !got.Locked {
		t.Fatalf("env-forced off over a stored on = %+v, want off, environment, locked", got)
	}
	if srv.mcpAvailable() {
		t.Fatal("mcpAvailable() ignores PAD_MCP_ENABLED=false")
	}

	rr := doRequestWithCookie(srv, "PUT", "/api/v1/admin/mcp", map[string]any{"enabled": true}, token)
	if rr.Code != http.StatusConflict {
		t.Fatalf("PUT under the env lock: %d, want 409: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.Error.Code != "set_by_environment" {
		t.Fatalf("code = %q, want set_by_environment", body.Error.Code)
	}
}

// Cloud is always on, and the toggle is the operator's.
func TestAdminMCP_Cloud(t *testing.T) {
	srv, token := mcpServer(t, config.Config{PublicURL: "https://app.getpad.dev", MCPPublicURL: "https://mcp.getpad.dev"}, nil)
	srv.SetCloudMode("cloud-secret")
	got := decodeMCPSettings(t, doRequestWithCookie(srv, "GET", "/api/v1/admin/mcp", nil, token))
	if !got.Enabled || got.Source != mcpSourceCloud || !got.Locked || got.Readiness.State != "on" {
		t.Fatalf("cloud = %+v, want on, cloud, locked", got)
	}
	rr := doRequestWithCookie(srv, "PUT", "/api/v1/admin/mcp", map[string]any{"enabled": false}, token)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("PUT on cloud: %d, want 403: %s", rr.Code, rr.Body.String())
	}
}

// A setting that cannot be read counts as off (DR-2): the capability fails
// closed. The control leg shows the stored value reads as on first, so the
// off after the failure is caused by the failure.
func TestAdminMCP_SettingReadErrorIsOff(t *testing.T) {
	srv, _ := mcpServer(t, config.Config{URL: "https://pad.example.com"}, nil)
	if err := srv.store.SetPlatformSetting(settingMCPEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	if on, _ := srv.mcpSetting(); !on || !srv.mcpAvailable() {
		t.Fatal("control: the stored setting does not read as on")
	}
	if _, err := srv.store.DB().Exec(`ALTER TABLE platform_settings RENAME TO platform_settings_unreadable`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = srv.store.DB().Exec(`ALTER TABLE platform_settings_unreadable RENAME TO platform_settings`)
	})
	if on, _ := srv.mcpSetting(); on || srv.mcpAvailable() {
		t.Fatal("a setting read error counted as on")
	}
}

func TestAdminMCP_RequiresAdminAndABody(t *testing.T) {
	srv, token := mcpServer(t, config.Config{URL: "https://pad.example.com"}, nil)
	if _, err := srv.store.CreateUser(models.UserCreate{Email: "member@example.com", Name: "Member", Password: "correct-horse-battery-staple", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	member := loginUser(t, srv, "member@example.com", "correct-horse-battery-staple")
	for _, method := range []string{"GET", "PUT"} {
		rr := doRequestWithCookie(srv, method, "/api/v1/admin/mcp", map[string]any{"enabled": true}, member)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s as a member: %d, want 403", method, rr.Code)
		}
	}
	rr := doRequestWithCookie(srv, "PUT", "/api/v1/admin/mcp", map[string]any{}, token)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("PUT without enabled: %d, want 400", rr.Code)
	}
	if v, _ := srv.store.GetPlatformSetting(settingMCPEnabled); v != "" {
		t.Fatalf("a refused PUT stored %q", v)
	}
}
