package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

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
