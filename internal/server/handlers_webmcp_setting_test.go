package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestWebMCPSetting_AdminPatchPersistsAndSessionReflects pins TASK-1889 /
// PLAN-1888 Phase 1: the webmcp_enabled platform setting is admin-writable
// through the settings whitelist, defaults to false on a fresh instance, and
// is surfaced in the /api/v1/auth/session payload reflecting the stored value.
func TestWebMCPSetting_AdminPatchPersistsAndSessionReflects(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// Fresh instance: session must report webmcp_enabled=false.
	rr := doRequestWithCookie(srv, "GET", "/api/v1/auth/session", nil, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("session: status=%d body=%s", rr.Code, rr.Body.String())
	}
	enabled, ok := sessionWebMCPEnabled(t, rr.Body.Bytes())
	if !ok {
		t.Fatalf("session payload missing webmcp_enabled; body=%s", rr.Body.String())
	}
	if enabled {
		t.Errorf("fresh instance: webmcp_enabled=%v, want false", enabled)
	}

	// Admin GET /admin/settings should surface the default explicitly.
	rr = doRequestWithCookie(srv, "GET", "/api/v1/admin/settings", nil, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("get settings: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var settings map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &settings); err != nil {
		t.Fatalf("decode settings: %v body=%s", err, rr.Body.String())
	}
	if settings["webmcp_enabled"] != "false" {
		t.Errorf("get settings: webmcp_enabled=%q, want \"false\"", settings["webmcp_enabled"])
	}

	// Admin PATCH enables it.
	rr = doRequestWithCookie(srv, "PATCH", "/api/v1/admin/settings",
		map[string]any{"webmcp_enabled": "true"}, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch settings: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Stored value should now be "true".
	got, err := srv.store.GetPlatformSetting("webmcp_enabled")
	if err != nil {
		t.Fatalf("GetPlatformSetting: %v", err)
	}
	if got != "true" {
		t.Errorf("stored webmcp_enabled=%q, want \"true\"", got)
	}

	// Session now reflects the enabled value.
	rr = doRequestWithCookie(srv, "GET", "/api/v1/auth/session", nil, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("session after enable: status=%d body=%s", rr.Code, rr.Body.String())
	}
	enabled, ok = sessionWebMCPEnabled(t, rr.Body.Bytes())
	if !ok {
		t.Fatalf("session payload missing webmcp_enabled after enable; body=%s", rr.Body.String())
	}
	if !enabled {
		t.Errorf("after enable: webmcp_enabled=%v, want true", enabled)
	}
}

// TestWebMCPSetting_NonAdminForbidden pins that a non-admin can't toggle the
// webmcp_enabled setting through the admin PATCH endpoint.
func TestWebMCPSetting_NonAdminForbidden(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	memberToken := registerNonAdmin(t, srv, "member@test.com", "Member")

	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/admin/settings",
		map[string]any{"webmcp_enabled": "true"}, memberToken)
	if rr.Code != http.StatusForbidden {
		t.Errorf("non-admin patch: status=%d, want 403", rr.Code)
	}

	// The setting must remain unset (fail-closed default false).
	got, err := srv.store.GetPlatformSetting("webmcp_enabled")
	if err != nil {
		t.Fatalf("GetPlatformSetting: %v", err)
	}
	if got == "true" {
		t.Errorf("webmcp_enabled persisted=%q after forbidden patch, want unset/false", got)
	}
}

// sessionWebMCPEnabled extracts the webmcp_enabled bool from a session payload.
// The second return value reports whether the key was present.
func sessionWebMCPEnabled(t *testing.T, body []byte) (bool, bool) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode session payload: %v body=%s", err, body)
	}
	raw, ok := payload["webmcp_enabled"]
	if !ok {
		return false, false
	}
	enabled, isBool := raw.(bool)
	if !isBool {
		t.Fatalf("webmcp_enabled is %T, want bool", raw)
	}
	return enabled, true
}
