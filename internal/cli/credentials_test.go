package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempHome chroots ~/.pad to a fresh tempdir for the test. CredentialsPath
// resolves via os.UserHomeDir → $HOME, so a t.Setenv("HOME", ...) is enough
// to fully isolate the test from the real credentials.json.
func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func writeCredsFile(t *testing.T, home, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".pad")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir ~/.pad: %v", err)
	}
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	return path
}

// readCredsFile returns the on-disk JSON of credentials.json. Used to
// assert post-Save behavior — confirms the v2 format actually lands on
// disk rather than just living in memory.
func readCredsFile(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".pad", "credentials.json"))
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	return string(data)
}

// TestLoadStore_FileMissingReturnsEmptyStore — fresh machine, no creds
// file. LoadStore must return a usable empty store, not nil-or-error,
// so callers can chain Get without a nil check.
func TestLoadStore_FileMissingReturnsEmptyStore(t *testing.T) {
	withTempHome(t)

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore on missing file: %v", err)
	}
	if store == nil {
		t.Fatal("LoadStore returned nil on missing file; want empty store")
	}
	if len(store.Credentials) != 0 {
		t.Errorf("expected empty credentials map, got %d entries", len(store.Credentials))
	}
	if got := store.Get("http://anywhere"); got != nil {
		t.Errorf("Get on empty store returned %v, want nil", got)
	}
}

// TestLoadStore_EmptyFileReturnsEmptyStore — credentials.json exists but
// is empty (zero bytes / whitespace). Should be treated like the
// missing-file case rather than a parse error, since the user probably
// `> credentials.json` to recover from a corrupt state.
func TestLoadStore_EmptyFileReturnsEmptyStore(t *testing.T) {
	home := withTempHome(t)
	writeCredsFile(t, home, "")

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore on empty file: %v", err)
	}
	if got := len(store.Credentials); got != 0 {
		t.Errorf("expected 0 entries, got %d", got)
	}
}

// TestLoadStore_V1MigratesInMemory — the canonical v1 file format
// migrates to a single-entry v2 store keyed by the legacy server_url.
// Read is side-effect-free: the on-disk file stays v1 until something
// triggers Save.
func TestLoadStore_V1MigratesInMemory(t *testing.T) {
	home := withTempHome(t)
	v1Body := `{
		"server_url": "http://127.0.0.1:7777",
		"token": "padsess_v1token",
		"user_id": "u-1",
		"email": "dave@example.com",
		"name": "Dave"
	}`
	path := writeCredsFile(t, home, v1Body)

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	creds := store.Get("http://127.0.0.1:7777")
	if creds == nil {
		t.Fatalf("expected v1 entry to migrate; got nil for http://127.0.0.1:7777")
	}
	if creds.Token != "padsess_v1token" {
		t.Errorf("token after migration = %q, want padsess_v1token", creds.Token)
	}
	if creds.Email != "dave@example.com" {
		t.Errorf("email after migration = %q, want dave@example.com", creds.Email)
	}

	// On-disk file should be unchanged — read is side-effect free.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read credentials.json: %v", err)
	}
	if !strings.Contains(string(onDisk), `"server_url"`) {
		t.Errorf("expected v1 file to remain on disk after read, got: %s", onDisk)
	}
	if strings.Contains(string(onDisk), `"version"`) {
		t.Errorf("read should not have rewritten file to v2; got: %s", onDisk)
	}
}

// TestLoadStore_V1WithEmptyTokenIsEmptyStore — v1 file with an empty
// token (or empty server_url) is unusable, so we treat it as no
// credentials saved. Avoids surfacing a phantom entry keyed by "" or
// holding an empty token that callers would have to defensively check.
func TestLoadStore_V1WithEmptyTokenIsEmptyStore(t *testing.T) {
	home := withTempHome(t)
	writeCredsFile(t, home, `{"server_url": "http://x:7777", "token": ""}`)

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if got := len(store.Credentials); got != 0 {
		t.Errorf("expected 0 entries from empty-token v1, got %d", got)
	}
}

// TestLoadStore_V1MigrationDurableOnSave — the in-memory migration
// becomes durable when the caller triggers Save. The file should
// transition from v1 to v2 with the same data preserved, no entries
// lost.
func TestLoadStore_V1MigrationDurableOnSave(t *testing.T) {
	home := withTempHome(t)
	writeCredsFile(t, home, `{
		"server_url": "http://127.0.0.1:7777",
		"token": "padsess_v1token",
		"user_id": "u-1",
		"email": "dave@example.com",
		"name": "Dave"
	}`)

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Re-read raw and parse as v2 to confirm format.
	body := readCredsFile(t, home)
	var roundTrip CredentialStore
	if err := json.Unmarshal([]byte(body), &roundTrip); err != nil {
		t.Fatalf("post-save parse: %v\nbody: %s", err, body)
	}
	if roundTrip.Version != credentialsVersion {
		t.Errorf("post-save version = %d, want %d", roundTrip.Version, credentialsVersion)
	}
	if got := roundTrip.Credentials["http://127.0.0.1:7777"]; got == nil {
		t.Fatalf("post-save missing entry for original server URL; body: %s", body)
	} else if got.Token != "padsess_v1token" {
		t.Errorf("post-save token = %q, want padsess_v1token", got.Token)
	}

	// The legacy top-level "token" must NOT remain in the v2 file.
	if strings.Contains(body, `"token": "padsess_v1token"`) && !strings.Contains(body, `"credentials"`) {
		t.Errorf("v2 file unexpectedly retained legacy top-level token: %s", body)
	}
	if !strings.Contains(body, `"version": 2`) {
		t.Errorf("v2 file missing version marker: %s", body)
	}
}

// TestLoadStore_V2RoundTrip — a v2 file written by an earlier session
// reads back identical entries.
func TestLoadStore_V2RoundTrip(t *testing.T) {
	home := withTempHome(t)
	v2Body := `{
		"version": 2,
		"credentials": {
			"http://127.0.0.1:7777": {
				"server_url": "http://127.0.0.1:7777",
				"token": "padsess_local",
				"user_id": "u-1",
				"email": "dave@local",
				"name": "Local Dave"
			},
			"https://app.getpad.dev": {
				"server_url": "https://app.getpad.dev",
				"token": "padsess_cloud",
				"user_id": "u-2",
				"email": "dave@cloud",
				"name": "Cloud Dave"
			}
		}
	}`
	writeCredsFile(t, home, v2Body)

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if got := store.Get("http://127.0.0.1:7777"); got == nil || got.Token != "padsess_local" {
		t.Errorf("local entry: got %+v, want token=padsess_local", got)
	}
	if got := store.Get("https://app.getpad.dev"); got == nil || got.Token != "padsess_cloud" {
		t.Errorf("cloud entry: got %+v, want token=padsess_cloud", got)
	}
	if got := store.Get("http://nowhere"); got != nil {
		t.Errorf("Get on absent server returned %+v, want nil", got)
	}
}

// TestStore_Set_AddAndReplace — Set adds a new entry on first call,
// replaces it on a second call with the same key. Mirrors the URL into
// the value's ServerURL field for consumer consistency.
func TestStore_Set_AddAndReplace(t *testing.T) {
	store := newEmptyStore()
	store.Set("http://x:7777", &Credentials{Token: "first", Email: "a@a"})
	store.Set("http://x:7777", &Credentials{Token: "second", Email: "b@b"})

	got := store.Get("http://x:7777")
	if got == nil {
		t.Fatal("Get returned nil after Set")
	}
	if got.Token != "second" {
		t.Errorf("token = %q, want second (replace failed)", got.Token)
	}
	if got.Email != "b@b" {
		t.Errorf("email = %q, want b@b", got.Email)
	}
	// Set must mirror the URL into the value's ServerURL field.
	if got.ServerURL != "http://x:7777" {
		t.Errorf("ServerURL on stored value = %q, want http://x:7777", got.ServerURL)
	}
}

// TestStore_Delete_KeepsSiblings — Delete removes only the targeted
// entry. This is the keystone behavior of TASK-1228: `pad auth logout`
// against one server must not touch the others.
func TestStore_Delete_KeepsSiblings(t *testing.T) {
	store := newEmptyStore()
	store.Set("http://a:7777", &Credentials{Token: "a"})
	store.Set("http://b:7777", &Credentials{Token: "b"})
	store.Set("http://c:7777", &Credentials{Token: "c"})

	store.Delete("http://b:7777")

	if store.Get("http://b:7777") != nil {
		t.Error("expected b entry to be deleted")
	}
	if store.Get("http://a:7777") == nil || store.Get("http://c:7777") == nil {
		t.Errorf("Delete b removed sibling(s): a=%v c=%v",
			store.Get("http://a:7777"), store.Get("http://c:7777"))
	}
}

// TestStore_DeleteAbsent_NoOp — Delete on a missing key must not panic
// or error. Matches the UX of `pad auth logout` against a server you
// were never logged into.
func TestStore_DeleteAbsent_NoOp(t *testing.T) {
	store := newEmptyStore()
	store.Delete("http://never-existed")
	if got := len(store.Credentials); got != 0 {
		t.Errorf("expected 0 entries, got %d", got)
	}
}

// TestStore_NilReceiverSafe — Get on a nil store returns nil instead of
// panicking. NewClientFromURL relies on this so a LoadStore failure
// degrades gracefully (no auth token attached) rather than crashing
// the CLI on every command.
func TestStore_NilReceiverSafe(t *testing.T) {
	var store *CredentialStore
	if got := store.Get("http://anywhere"); got != nil {
		t.Errorf("nil-receiver Get returned %+v, want nil", got)
	}
	// Delete should also be a no-op on nil. Asserting it doesn't panic
	// is the assertion — if it panics, the test fails.
	store.Delete("http://anywhere")
}

// TestStore_URLNormalization — trailing slashes and surrounding
// whitespace must canonicalize so http://x:7777 and http://x:7777/ hit
// the same bucket. cfg.BrowserURL() trims trailing slash; user-typed
// --url flags often don't.
func TestStore_URLNormalization(t *testing.T) {
	store := newEmptyStore()
	store.Set("http://x:7777/", &Credentials{Token: "with-slash"})

	// Trailing slash variant should find the entry too.
	if got := store.Get("http://x:7777"); got == nil || got.Token != "with-slash" {
		t.Errorf("Get on slash-trimmed key: got %+v, want token=with-slash", got)
	}
	// As should a leading-whitespace variant.
	if got := store.Get("  http://x:7777  "); got == nil {
		t.Errorf("Get on whitespace variant returned nil")
	}

	// And only one entry should exist (Set with a different formatting
	// of the same canonical URL must replace, not duplicate).
	store.Set("http://x:7777", &Credentials{Token: "no-slash"})
	if got := len(store.Credentials); got != 1 {
		t.Errorf("expected 1 entry after canonical-equivalent Set, got %d", got)
	}
}

// TestStore_Save_PreservesAllEntries — Save round-trips multiple
// servers without losing any entries. Direct test of the multi-server
// invariant: switching between servers via login/logout must not
// clobber each other's credentials.
func TestStore_Save_PreservesAllEntries(t *testing.T) {
	withTempHome(t)

	store := newEmptyStore()
	store.Set("http://a:7777", &Credentials{Token: "a", Email: "a@a"})
	store.Set("http://b:7777", &Credentials{Token: "b", Email: "b@b"})
	store.Set("http://c:7777", &Credentials{Token: "c", Email: "c@c"})

	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	for _, key := range []string{"http://a:7777", "http://b:7777", "http://c:7777"} {
		if got := reloaded.Get(key); got == nil {
			t.Errorf("entry for %q lost across Save/Load", key)
		}
	}
}

// TestStore_Save_FilePermissionsAre0600 — credentials.json must be 0600
// so other local users can't read tokens. Defends against a regression
// where Save loosens the permissions during the write (e.g. someone
// accidentally writes via a path that isn't atomic + 0600).
func TestStore_Save_FilePermissionsAre0600(t *testing.T) {
	home := withTempHome(t)
	store := newEmptyStore()
	store.Set("http://x:7777", &Credentials{Token: "t"})
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(filepath.Join(home, ".pad", "credentials.json"))
	if err != nil {
		t.Fatalf("stat credentials.json: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("credentials.json mode = %o, want 0600", got)
	}
}

// TestWipeCredentialsFile — removes the file entirely. Distinct from
// CredentialStore.Delete (which removes one entry). Idempotent.
func TestWipeCredentialsFile(t *testing.T) {
	home := withTempHome(t)
	writeCredsFile(t, home, `{"version": 2, "credentials": {}}`)

	if err := WipeCredentialsFile(); err != nil {
		t.Fatalf("WipeCredentialsFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".pad", "credentials.json")); !os.IsNotExist(err) {
		t.Errorf("credentials.json still exists after Wipe; stat err=%v", err)
	}
	// Idempotent — second wipe on absent file must succeed.
	if err := WipeCredentialsFile(); err != nil {
		t.Errorf("WipeCredentialsFile (second call): %v", err)
	}
}

// TestLoadStore_GarbageFileErrors — a malformed credentials.json is
// reported as an error, not silently treated as empty. We don't want
// `pad auth login` writing to a file we can't safely interpret —
// that'd lose user data.
func TestLoadStore_GarbageFileErrors(t *testing.T) {
	home := withTempHome(t)
	writeCredsFile(t, home, "not json at all {[}")

	if _, err := LoadStore(); err == nil {
		t.Error("LoadStore on garbage file: expected error, got nil")
	}
}
