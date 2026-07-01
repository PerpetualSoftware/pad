package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/email"
)

// TestAdminSettings_SecretsNotExposed is the BUG-1909 regression: GET
// /admin/settings must return ONLY admin-managed keys. The platform_settings
// table also holds the 2FA challenge signing secret and plan-limit rows; neither
// may leak to the admin client.
func TestAdminSettings_SecretsNotExposed(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// Seed the kinds of rows that live in platform_settings alongside the
	// admin-managed keys: a server secret and a plan-limit row.
	const secret = "5FTmIeLVxG3wC+NwjOdSch79IQASkY0ybmRHU65ufAc="
	if err := srv.store.SetPlatformSetting("2fa_challenge_secret", secret); err != nil {
		t.Fatalf("seed 2fa secret: %v", err)
	}
	if err := srv.store.SetPlatformSetting("plan_limits_free_workspaces", "3"); err != nil {
		t.Fatalf("seed limit row: %v", err)
	}
	// And a legitimate admin-managed key to prove the allowlist still passes those.
	if err := srv.store.SetPlatformSetting(settingPlatformName, "Acme Pad"); err != nil {
		t.Fatalf("seed platform_name: %v", err)
	}

	rr := doRequestWithCookie(srv, "GET", "/api/v1/admin/settings", nil, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Raw-body check: the secret must not appear anywhere in the response, even
	// as a substring (guards against future serialization changes).
	if body := rr.Body.String(); strings.Contains(body, secret) {
		t.Fatalf("2fa_challenge_secret leaked in GET /admin/settings body: %s", body)
	}

	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got["2fa_challenge_secret"]; ok {
		t.Error("2fa_challenge_secret present as a key in GET /admin/settings")
	}
	if _, ok := got["plan_limits_free_workspaces"]; ok {
		t.Error("plan_limits_* row leaked into GET /admin/settings")
	}
	if got[settingPlatformName] != "Acme Pad" {
		t.Errorf("admin-managed platform_name missing/wrong: %q", got[settingPlatformName])
	}
	// Every returned key must be in the allowlist.
	for k := range got {
		if !adminManagedSettings[k] {
			t.Errorf("GET returned non-allowlisted key %q", k)
		}
	}
}

// TestAdminSettings_SecretNotWritable pins the write side of deny-by-default: a
// PATCH cannot overwrite the 2FA secret (or any non-allowlisted key) through the
// settings endpoint.
func TestAdminSettings_SecretNotWritable(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	const secret = "original-2fa-secret-value"
	if err := srv.store.SetPlatformSetting("2fa_challenge_secret", secret); err != nil {
		t.Fatalf("seed 2fa secret: %v", err)
	}

	body := map[string]any{
		"2fa_challenge_secret":        "attacker-controlled",
		"plan_limits_free_workspaces": "9999",
	}
	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/admin/settings", body, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: status=%d body=%s", rr.Code, rr.Body.String())
	}

	if stored, _ := srv.store.GetPlatformSetting("2fa_challenge_secret"); stored != secret {
		t.Errorf("2fa_challenge_secret was overwritten via settings PATCH: got %q, want %q", stored, secret)
	}
	if stored, _ := srv.store.GetPlatformSetting("plan_limits_free_workspaces"); stored != "" {
		t.Errorf("plan_limits row was written via settings PATCH: got %q", stored)
	}
}

// TestMaskAPIKey pins the single source of truth for the sensitive-value mask
// (BUG-1890). GET /admin/settings emits this form and the update handler compares
// against it to detect an echoed-back (unchanged) key.
func TestMaskAPIKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"short", "abc", "****"},
		{"exactly-8", "abcd1234", "****"},
		{"long", "mlr-1234567890abcdef", "mlr-...cdef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maskAPIKey(tc.in); got != tc.want {
				t.Errorf("maskAPIKey(%q)=%q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestAdminSettings_MaskedKeyNotPersisted is the core BUG-1890 regression: the
// admin GETs settings (key comes back masked), then PATCHes the whole object back
// without re-typing the key. The masked placeholder must NOT overwrite the real
// stored key.
func TestAdminSettings_MaskedKeyNotPersisted(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	const realKey = "mlr-liveSecret-9f8e7d6c5b4a"
	if err := srv.store.SetPlatformSetting(settingMailerooAPIKey, realKey); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	// GET returns the masked form.
	rr := doRequestWithCookie(srv, "GET", "/api/v1/admin/settings", nil, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v body=%s", err, rr.Body.String())
	}
	masked := got[settingMailerooAPIKey]
	if masked == realKey {
		t.Fatalf("GET returned the raw key %q — should be masked", masked)
	}
	if masked != maskAPIKey(realKey) {
		t.Fatalf("GET mask=%q, want %q", masked, maskAPIKey(realKey))
	}

	// PATCH the masked value straight back (the pre-fix client behaviour).
	body := map[string]any{
		settingEmailProvider:  "maileroo",
		settingMailerooAPIKey: masked,
		settingEmailFrom:      "noreply@example.com",
		settingEmailFromName:  "Pad",
	}
	rr = doRequestWithCookie(srv, "PATCH", "/api/v1/admin/settings", body, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// The real key must survive — the guard treated the mask as "unchanged".
	stored, err := srv.store.GetPlatformSetting(settingMailerooAPIKey)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != realKey {
		t.Errorf("stored key corrupted: got %q, want %q", stored, realKey)
	}

	// The other email fields the admin *did* set should still round-trip.
	if from, _ := srv.store.GetPlatformSetting(settingEmailFrom); from != "noreply@example.com" {
		t.Errorf("email_from=%q, want noreply@example.com", from)
	}
}

// TestAdminSettings_ShortKeyMaskNotPersisted covers the other mask branch: a key
// of <=8 chars masks to the literal "****". Echoing "****" back must not overwrite
// the real short key. (The guard is format-agnostic string equality, but this pins
// the "****" HTTP round-trip explicitly.)
func TestAdminSettings_ShortKeyMaskNotPersisted(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	const realKey = "short123" // 8 chars -> masks to "****"
	if err := srv.store.SetPlatformSetting(settingMailerooAPIKey, realKey); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	if maskAPIKey(realKey) != "****" {
		t.Fatalf("precondition: maskAPIKey(%q)=%q, want ****", realKey, maskAPIKey(realKey))
	}

	body := map[string]any{settingMailerooAPIKey: "****"}
	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/admin/settings", body, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: status=%d body=%s", rr.Code, rr.Body.String())
	}

	stored, err := srv.store.GetPlatformSetting(settingMailerooAPIKey)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != realKey {
		t.Errorf("short key corrupted: got %q, want %q", stored, realKey)
	}
}

// TestAdminSettings_RealKeyUpdateWins confirms the guard only skips the mask, not
// a genuine new key: a real value entered by the admin replaces the stored key.
func TestAdminSettings_RealKeyUpdateWins(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	if err := srv.store.SetPlatformSetting(settingMailerooAPIKey, "mlr-old-0000111122223333"); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	const newKey = "mlr-new-4444555566667777"
	body := map[string]any{settingMailerooAPIKey: newKey}
	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/admin/settings", body, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: status=%d body=%s", rr.Code, rr.Body.String())
	}

	stored, err := srv.store.GetPlatformSetting(settingMailerooAPIKey)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != newKey {
		t.Errorf("new key not persisted: got %q, want %q", stored, newKey)
	}
}

// TestAdminSettings_DisableTearsDownLiveSender pins the BUG-1890 follow-up: when a
// UI-configured (no env) instance disables email by clearing the key, the live
// in-memory sender must be torn down — not just the DB row — so mail actually
// stops without a restart.
func TestAdminSettings_DisableTearsDownLiveSender(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// Configure email through the platform-settings path (no env sender).
	body := map[string]any{
		settingEmailProvider:  "maileroo",
		settingMailerooAPIKey: "mlr-live-configured-123456",
		settingEmailFrom:      "noreply@example.com",
	}
	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/admin/settings", body, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("configure patch: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if srv.email == nil {
		t.Fatal("expected live email sender after configuring a key")
	}

	// Disable: provider None + cleared key (what the fixed client sends).
	body = map[string]any{settingEmailProvider: "", settingMailerooAPIKey: ""}
	rr = doRequestWithCookie(srv, "PATCH", "/api/v1/admin/settings", body, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("disable patch: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if srv.email != nil {
		t.Error("live sender not torn down after clearing key — email still active until restart")
	}
	if srv.emailAPIKey != "" {
		t.Errorf("emailAPIKey=%q after disable, want empty", srv.emailAPIKey)
	}
}

// TestReconfigureEmail_EnvSenderPreserved is the counterpart guard: an env-wired
// sender (the deployment baseline) must survive a reconfigure when platform
// settings carry no key — the admin UI doesn't disable env-configured email.
func TestReconfigureEmail_EnvSenderPreserved(t *testing.T) {
	srv := testServer(t)

	srv.SetEmailSender(email.NewSender("env-key-abcdef", "env@example.com", "Pad", "http://localhost"), "env-key-abcdef")
	if srv.email == nil {
		t.Fatal("precondition: env sender should be set")
	}

	// No platform-settings key stored; reconfigure must not wipe the env sender.
	srv.reconfigureEmail()
	if srv.email == nil {
		t.Error("env-configured sender was torn down by reconfigureEmail with empty platform key")
	}
}

// TestAdminSettings_EmptyKeyClears confirms an explicit empty value still clears
// the key (the "disable email" path) — the guard only skips a non-empty mask.
func TestAdminSettings_EmptyKeyClears(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	if err := srv.store.SetPlatformSetting(settingMailerooAPIKey, "mlr-to-be-cleared-8888"); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	body := map[string]any{settingMailerooAPIKey: ""}
	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/admin/settings", body, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: status=%d body=%s", rr.Code, rr.Body.String())
	}

	stored, err := srv.store.GetPlatformSetting(settingMailerooAPIKey)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != "" {
		t.Errorf("key not cleared: got %q, want empty", stored)
	}
}
