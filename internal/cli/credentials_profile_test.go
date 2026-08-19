package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const profileServerURL = "https://app.getpad.dev"

func v2SingleUserBody() string {
	return `{
		"version": 2,
		"credentials": {
			"https://app.getpad.dev": {
				"server_url": "https://app.getpad.dev",
				"token": "padsess_default",
				"user_id": "u-default",
				"email": "dave@example.com",
				"name": "Dave"
			}
		}
	}`
}

func v3TwoProfileBody() string {
	return `{
		"version": 3,
		"credentials": {
			"https://app.getpad.dev": {
				"profiles": {
					"default": {
						"token": "padsess_default",
						"user_id": "u-default",
						"email": "dave@example.com",
						"name": "Dave"
					},
					"cursor": {
						"token": "padsess_cursor",
						"user_id": "u-cursor",
						"email": "cursor@example.com",
						"name": "Cursor"
					}
				}
			}
		}
	}`
}

// TestLoadStore_V2MigratesToDefaultProfileInMemory — a v2 single-blob-per-URL
// file becomes an in-memory v3 store with the legacy entry under "default".
// Read is side-effect-free: the on-disk file stays v2 until Save.
func TestLoadStore_V2MigratesToDefaultProfileInMemory(t *testing.T) {
	home := withTempHome(t)
	path := writeCredsFile(t, home, v2SingleUserBody())

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	got := store.GetProfile(profileServerURL, "default")
	if got == nil {
		t.Fatal("expected v2 entry to migrate into profiles.default; got nil")
	}
	if got.Token != "padsess_default" {
		t.Errorf("default profile token = %q, want padsess_default", got.Token)
	}
	// Get still delegates to the default profile so existing call sites
	// keep working through the migration.
	if viaGet := store.Get(profileServerURL); viaGet == nil || viaGet.Token != "padsess_default" {
		t.Errorf("Get after v2 migrate = %+v, want default token", viaGet)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read credentials.json: %v", err)
	}
	if strings.Contains(string(onDisk), `"profiles"`) {
		t.Errorf("read should not rewrite v2 file to v3; got: %s", onDisk)
	}
	if !strings.Contains(string(onDisk), `"version": 2`) {
		t.Errorf("expected v2 file to remain on disk after read, got: %s", onDisk)
	}
}

// TestLoadStore_V2MigrationDurableOnSave — the in-memory v2→v3 migration
// becomes durable on Save. Single-user files land as profiles.default
// with no extra profiles invented.
func TestLoadStore_V2MigrationDurableOnSave(t *testing.T) {
	home := withTempHome(t)
	writeCredsFile(t, home, v2SingleUserBody())

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	body := readCredsFile(t, home)
	if !strings.Contains(body, `"version": 3`) {
		t.Errorf("post-save missing version 3 marker: %s", body)
	}
	if !strings.Contains(body, `"profiles"`) {
		t.Errorf("post-save missing profiles map: %s", body)
	}
	if !strings.Contains(body, `"default"`) {
		t.Errorf("post-save missing default profile: %s", body)
	}

	var roundTrip struct {
		Version     int `json:"version"`
		Credentials map[string]struct {
			Profiles map[string]*Credentials `json:"profiles"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(body), &roundTrip); err != nil {
		t.Fatalf("post-save parse: %v\nbody: %s", err, body)
	}
	if roundTrip.Version != 3 {
		t.Errorf("post-save version = %d, want 3", roundTrip.Version)
	}
	entry := roundTrip.Credentials[profileServerURL]
	if entry.Profiles == nil || entry.Profiles["default"] == nil {
		t.Fatalf("post-save missing profiles.default; body: %s", body)
	}
	if entry.Profiles["default"].Token != "padsess_default" {
		t.Errorf("default token = %q, want padsess_default", entry.Profiles["default"].Token)
	}
	if len(entry.Profiles) != 1 {
		t.Errorf("expected only the default profile after v2 migrate, got %d", len(entry.Profiles))
	}
}

// TestLoadStore_V3RoundTrip — a v3 file written by an earlier session
// reads back both the default and a named profile.
func TestLoadStore_V3RoundTrip(t *testing.T) {
	home := withTempHome(t)
	writeCredsFile(t, home, v3TwoProfileBody())

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	def := store.GetProfile(profileServerURL, "default")
	if def == nil || def.Token != "padsess_default" {
		t.Errorf("default profile: got %+v, want token=padsess_default", def)
	}
	cur := store.GetProfile(profileServerURL, "cursor")
	if cur == nil || cur.Token != "padsess_cursor" {
		t.Errorf("cursor profile: got %+v, want token=padsess_cursor", cur)
	}
	if got := store.GetProfile(profileServerURL, "missing"); got != nil {
		t.Errorf("GetProfile missing name returned %+v, want nil", got)
	}
}

// TestGet_DelegatesToDefaultProfile — Get(url) is GetProfile(url, "default")
// even when PAD_PROFILE names another profile. Existing call sites stay on
// the implicit profile; selection is the caller's job.
func TestGet_DelegatesToDefaultProfile(t *testing.T) {
	t.Setenv("PAD_PROFILE", "cursor")
	store := newEmptyStore()
	store.SetProfile(profileServerURL, "default", &Credentials{Token: "def"})
	store.SetProfile(profileServerURL, "cursor", &Credentials{Token: "cur"})

	got := store.Get(profileServerURL)
	if got == nil || got.Token != "def" {
		t.Errorf("Get = %+v, want default token (must ignore PAD_PROFILE)", got)
	}
}

// TestStore_SetProfile_KeepsSiblings — writing one named profile must not
// clobber another on the same server.
func TestStore_SetProfile_KeepsSiblings(t *testing.T) {
	store := newEmptyStore()
	store.SetProfile(profileServerURL, "default", &Credentials{Token: "def"})
	store.SetProfile(profileServerURL, "cursor", &Credentials{Token: "cur"})

	if got := store.GetProfile(profileServerURL, "default"); got == nil || got.Token != "def" {
		t.Errorf("default after sibling Set: %+v", got)
	}
	if got := store.GetProfile(profileServerURL, "cursor"); got == nil || got.Token != "cur" {
		t.Errorf("cursor after sibling Set: %+v", got)
	}
}

// TestStore_DeleteProfile_KeepsSiblings — logout --profile cursor must not
// drop the default (or any other) profile on the same server.
func TestStore_DeleteProfile_KeepsSiblings(t *testing.T) {
	store := newEmptyStore()
	store.SetProfile(profileServerURL, "default", &Credentials{Token: "def"})
	store.SetProfile(profileServerURL, "cursor", &Credentials{Token: "cur"})

	store.DeleteProfile(profileServerURL, "cursor")

	if store.GetProfile(profileServerURL, "cursor") != nil {
		t.Error("expected cursor profile to be deleted")
	}
	if got := store.GetProfile(profileServerURL, "default"); got == nil || got.Token != "def" {
		t.Errorf("DeleteProfile cursor removed default: %+v", got)
	}
}

// TestStore_DeleteProfile_DropsEmptyServer — when the last profile for a
// server is removed, the server key itself goes away so we don't leave an
// empty profiles object in the file.
func TestStore_DeleteProfile_DropsEmptyServer(t *testing.T) {
	store := newEmptyStore()
	store.SetProfile(profileServerURL, "cursor", &Credentials{Token: "cur"})
	store.DeleteProfile(profileServerURL, "cursor")

	if got := len(store.Credentials); got != 0 {
		t.Errorf("expected empty server map after last profile delete, got %d", got)
	}
}

// TestStore_Delete_OnlyRemovesDefault — Delete(url) stays the default-profile
// operation so existing logout call sites don't wipe every named profile.
func TestStore_Delete_OnlyRemovesDefault(t *testing.T) {
	store := newEmptyStore()
	store.SetProfile(profileServerURL, "default", &Credentials{Token: "def"})
	store.SetProfile(profileServerURL, "cursor", &Credentials{Token: "cur"})

	store.Delete(profileServerURL)

	if store.Get(profileServerURL) != nil {
		t.Error("expected default profile to be deleted")
	}
	if got := store.GetProfile(profileServerURL, "cursor"); got == nil || got.Token != "cur" {
		t.Errorf("Delete(url) must not drop cursor: %+v", got)
	}
}

// TestStore_GetProfile_NilReceiverSafe — same contract as Get.
func TestStore_GetProfile_NilReceiverSafe(t *testing.T) {
	var store *CredentialStore
	if got := store.GetProfile(profileServerURL, "cursor"); got != nil {
		t.Errorf("nil-receiver GetProfile returned %+v, want nil", got)
	}
	store.DeleteProfile(profileServerURL, "cursor")
}

// TestStore_EmptyProfileNameIsDefault — blank / whitespace profile names
// collapse to the implicit default so `--profile ""` and an empty
// PAD_PROFILE don't invent a nameless bucket.
func TestStore_EmptyProfileNameIsDefault(t *testing.T) {
	store := newEmptyStore()
	store.SetProfile(profileServerURL, "  ", &Credentials{Token: "def"})
	if got := store.GetProfile(profileServerURL, ""); got == nil || got.Token != "def" {
		t.Errorf("empty name should resolve to default, got %+v", got)
	}
}

// TestResolveProfile_Precedence — --profile (SetProfileOverride) beats
// PAD_PROFILE beats the implicit default. Stateless: no persisted current
// profile.
func TestResolveProfile_Precedence(t *testing.T) {
	t.Cleanup(func() { SetProfileOverride("") })

	t.Setenv("PAD_PROFILE", "")
	SetProfileOverride("")
	if got := ResolveProfile(); got != "default" {
		t.Errorf("no flag/env: got %q, want default", got)
	}

	t.Setenv("PAD_PROFILE", "from-env")
	if got := ResolveProfile(); got != "from-env" {
		t.Errorf("env only: got %q, want from-env", got)
	}

	SetProfileOverride("from-flag")
	if got := ResolveProfile(); got != "from-flag" {
		t.Errorf("flag+env: got %q, want from-flag (flag wins)", got)
	}

	SetProfileOverride("")
	if got := ResolveProfile(); got != "from-env" {
		t.Errorf("cleared flag: got %q, want from-env", got)
	}
}

// TestNewClientFromURL_UsesResolvedProfile — the constructor chokepoint
// attaches the active profile's token, not always default. Pins the
// selection contract for every command that goes through NewClientFromURL.
func TestNewClientFromURL_UsesResolvedProfile(t *testing.T) {
	home := withTempHome(t)
	writeCredsFile(t, home, v3TwoProfileBody())
	t.Cleanup(func() { SetProfileOverride("") })

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/me" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"u-cursor","email":"cursor@example.com","name":"Cursor","role":"member"}`))
	}))
	defer srv.Close()

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	store.SetProfile(srv.URL, "default", &Credentials{Token: "padsess_default"})
	store.SetProfile(srv.URL, "cursor", &Credentials{Token: "padsess_cursor"})
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Setenv("PAD_PROFILE", "cursor")
	SetProfileOverride("")
	client := NewClientFromURL(srv.URL)
	if _, err := client.GetCurrentUser(); err != nil {
		t.Fatalf("GetCurrentUser: %v", err)
	}
	if gotAuth != "Bearer padsess_cursor" {
		t.Errorf("PAD_PROFILE=cursor attached %q, want Bearer padsess_cursor", gotAuth)
	}

	SetProfileOverride("default")
	client = NewClientFromURL(srv.URL)
	if _, err := client.GetCurrentUser(); err != nil {
		t.Fatalf("GetCurrentUser (flag default): %v", err)
	}
	if gotAuth != "Bearer padsess_default" {
		t.Errorf("--profile default attached %q, want Bearer padsess_default", gotAuth)
	}
}

// TestNewClientFromURL_DefaultWhenUnset — single-profile users see zero
// behavior change: no flag, no env, constructor still picks default.
func TestNewClientFromURL_DefaultWhenUnset(t *testing.T) {
	home := withTempHome(t)
	writeCredsFile(t, home, v3TwoProfileBody())
	t.Cleanup(func() { SetProfileOverride("") })
	t.Setenv("PAD_PROFILE", "")
	SetProfileOverride("")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/me" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"u-default","email":"dave@example.com","name":"Dave","role":"owner"}`))
	}))
	defer srv.Close()

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	store.SetProfile(srv.URL, "default", &Credentials{Token: "padsess_default"})
	store.SetProfile(srv.URL, "cursor", &Credentials{Token: "padsess_cursor"})
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	client := NewClientFromURL(srv.URL)
	if _, err := client.GetCurrentUser(); err != nil {
		t.Fatalf("GetCurrentUser: %v", err)
	}
	if gotAuth != "Bearer padsess_default" {
		t.Errorf("unset profile attached %q, want Bearer padsess_default", gotAuth)
	}
}
