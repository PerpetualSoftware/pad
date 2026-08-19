package cli

// Per-server credentials store (TASK-1228 / IDEA-1226, extended by #879).
//
// `~/.pad/credentials.json` previously held a single login blob:
//
//   {"server_url": "...", "token": "padsess_...", "user_id": "...", ...}
//
// That works for one Pad instance per developer machine, but breaks the
// real workflow of one developer on multiple instances (apm/ → cloud,
// target/ → local, testing/ → staging). Switching among them with
// `pad init --url <other>` would clobber the single entry, so going back
// to a previously-authed server meant logging in again.
//
// v2 keys credentials by canonical server URL:
//
//   {
//     "version": 2,
//     "credentials": {
//       "https://app.getpad.dev":      {"token": "...", "user_id": "...", ...},
//       "http://127.0.0.1:7777":       {"token": "...", "user_id": "...", ...}
//     }
//   }
//
// v3 (#879 layer 2) nests named profiles under each server so multiple
// identities can share one CLI install without logout/login cycling:
//
//   {
//     "version": 3,
//     "credentials": {
//       "https://app.getpad.dev": {
//         "profiles": {
//           "default": { "token": "...", "user_id": "..." },
//           "cursor":  { "token": "...", "user_id": "..." }
//         }
//       }
//     }
//   }
//
// Reads transparently migrate v1 → v3 and v2 → v3 in memory; the next
// Save writes v3. We deliberately do NOT save-on-read so reads stay
// side-effect free — the migration becomes durable on the first
// login/logout/setup.
//
// Single-profile users see no behavior change: one entry under
// profiles.default, Get(url) still returns it, identical UX.
//
// No persisted "current profile" and no top-level default pointer.
// Selection is stateless per invocation: --profile > PAD_PROFILE >
// "default". The configured server URL (cfg.BaseURL() from
// ~/.pad/config.toml or --url) remains the authoritative answer for
// "which server am I targeting."

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// credentialsVersion is what new saves write. Bump and add a migration
// branch in LoadStore when the on-disk shape changes.
const credentialsVersion = 3

// defaultProfileName is the implicit profile. v2 single-user entries
// migrate here so existing Get(url) call sites stay valid.
const defaultProfileName = "default"

// Credentials is the per-server (and, in v3, per-profile) login blob.
// The `server_url` JSON tag is kept (with omitempty) so v1 files still
// parse during the migration path; in v2/v3 the URL is the map key and
// the field is redundant — Set mirrors the key into the field so
// consumers reading the value alone still see a consistent ServerURL.
type Credentials struct {
	ServerURL string `json:"server_url,omitempty"`
	Token     string `json:"token"`
	UserID    string `json:"user_id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
}

// ServerCredentials is the v3 per-server bucket: a map of named profiles.
type ServerCredentials struct {
	Profiles map[string]*Credentials `json:"profiles"`
}

// CredentialStore is the on-disk shape of `~/.pad/credentials.json` (v3).
// Keyed by canonical server URL (trailing slash + surrounding whitespace
// stripped — see normalizeServerURL). Each server holds named profiles;
// "default" is the implicit one.
type CredentialStore struct {
	Version     int                           `json:"version"`
	Credentials map[string]*ServerCredentials `json:"credentials"`
}

// v2CredentialStore is the on-disk shape of a v2 file, used only as the
// source of the in-memory v2 → v3 migration.
type v2CredentialStore struct {
	Version     int                     `json:"version"`
	Credentials map[string]*Credentials `json:"credentials"`
}

// CredentialsPath returns ~/.pad/credentials.json.
func CredentialsPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".pad", "credentials.json"), nil
}

// credentialsPath is retained for internal call sites.
func credentialsPath() (string, error) {
	return CredentialsPath()
}

// LoadStore reads the credentials file and returns a usable
// CredentialStore. Returns an empty (but non-nil) store if the file
// doesn't exist — that's not an error, just "nothing logged in
// anywhere." Migrates v1 single-blob and v2 per-URL formats to v3 in
// memory; older files stay as-is on disk until the first Save (which
// always writes v3).
//
// LoadStore is the only intended way to read credentials. Internal
// callers that need a single server's default entry should call
// LoadStore().Get(url). Callers that need a named profile should call
// LoadStore().GetProfile(url, name).
func LoadStore() (*CredentialStore, error) {
	path, err := credentialsPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return newEmptyStore(), nil
		}
		return nil, fmt.Errorf("read credentials: %w", err)
	}

	if len(strings.TrimSpace(string(data))) == 0 {
		return newEmptyStore(), nil
	}

	// Detect format. v1 has a top-level `token` string; v2 has a
	// top-level `credentials` object whose values are login blobs; v3
	// values are `{profiles: {...}}`. Probe with a generic map so a
	// garbage file fails loudly here rather than silently parsing as
	// "empty v3."
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	if _, hasToken := probe["token"]; hasToken {
		return migrateV1(data)
	}
	if isV3Store(probe) {
		return parseV3(data)
	}
	return migrateV2(data)
}

// isV3Store reports whether the probed file is already v3: either the
// version field is >= 3, or a credentials entry has a `profiles` object.
// An empty credentials map with version 2 is treated as v2 (migrate);
// the same shape with version 3 is v3.
func isV3Store(probe map[string]json.RawMessage) bool {
	if versionFromProbe(probe) >= 3 {
		return true
	}
	raw, ok := probe["credentials"]
	if !ok || len(raw) == 0 {
		return false
	}
	var creds map[string]json.RawMessage
	if err := json.Unmarshal(raw, &creds); err != nil {
		return false
	}
	for _, entry := range creds {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(entry, &keys); err != nil {
			continue
		}
		if _, hasProfiles := keys["profiles"]; hasProfiles {
			return true
		}
	}
	return false
}

func versionFromProbe(probe map[string]json.RawMessage) int {
	raw, ok := probe["version"]
	if !ok {
		return 0
	}
	var version int
	if err := json.Unmarshal(raw, &version); err != nil {
		return 0
	}
	return version
}

// migrateV1 reads a v1 single-blob file and returns an in-memory v3 store
// keyed by the legacy ServerURL under the default profile. A v1 file with
// an empty server_url is treated as "no usable credentials" (returns an
// empty store) — that's the only safe interpretation, since we have no
// key to file the entry under. The on-disk file is unchanged; callers
// triggering Save (login, logout, setup) will write v3.
func migrateV1(data []byte) (*CredentialStore, error) {
	var legacy Credentials
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, fmt.Errorf("parse v1 credentials: %w", err)
	}
	store := newEmptyStore()
	if normalizeServerURL(legacy.ServerURL) != "" && legacy.Token != "" {
		store.Set(legacy.ServerURL, &legacy)
	}
	return store, nil
}

// migrateV2 reads a v2 per-URL file and wraps each login blob as
// profiles.default. In-memory only; disk stays v2 until Save.
func migrateV2(data []byte) (*CredentialStore, error) {
	var legacy v2CredentialStore
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, fmt.Errorf("parse v2 credentials: %w", err)
	}
	store := newEmptyStore()
	for url, creds := range legacy.Credentials {
		if creds == nil {
			continue
		}
		store.Set(url, creds)
	}
	return store, nil
}

// parseV3 unmarshals a v3 file. Tolerates a missing or zero version
// field and a nil credentials/profiles map by normalizing both —
// anything else (malformed JSON, wrong types) surfaces as an error.
func parseV3(data []byte) (*CredentialStore, error) {
	var store CredentialStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("parse v3 credentials: %w", err)
	}
	if store.Credentials == nil {
		store.Credentials = map[string]*ServerCredentials{}
	}
	for _, sc := range store.Credentials {
		if sc != nil && sc.Profiles == nil {
			sc.Profiles = map[string]*Credentials{}
		}
	}
	if store.Version == 0 {
		store.Version = credentialsVersion
	}
	return &store, nil
}

func newEmptyStore() *CredentialStore {
	return &CredentialStore{
		Version:     credentialsVersion,
		Credentials: map[string]*ServerCredentials{},
	}
}

// Get returns the default-profile credential for the given server URL,
// or nil if none exists. Delegates to GetProfile(url, "default") so
// existing call sites stay valid. URL is canonicalized (trailing slash
// + whitespace stripped) before lookup. Nil-receiver safe.
func (s *CredentialStore) Get(serverURL string) *Credentials {
	return s.GetProfile(serverURL, defaultProfileName)
}

// GetProfile returns the named profile for the given server URL, or nil
// if none exists. Empty / whitespace names resolve to "default".
// Nil-receiver safe.
func (s *CredentialStore) GetProfile(serverURL, name string) *Credentials {
	if s == nil || s.Credentials == nil {
		return nil
	}
	sc := s.Credentials[normalizeServerURL(serverURL)]
	if sc == nil || sc.Profiles == nil {
		return nil
	}
	return sc.Profiles[normalizeProfileName(name)]
}

// Set adds or replaces the default-profile credential for the given
// server URL. Delegates to SetProfile(url, "default", c).
func (s *CredentialStore) Set(serverURL string, c *Credentials) {
	s.SetProfile(serverURL, defaultProfileName, c)
}

// SetProfile adds or replaces the named profile for the given server
// URL. The canonical URL is mirrored into the value's ServerURL field
// so a caller reading the credential standalone still sees a consistent
// URL. Passing a nil credential is a no-op (use DeleteProfile to remove).
func (s *CredentialStore) SetProfile(serverURL, name string, c *Credentials) {
	if c == nil {
		return
	}
	if s.Credentials == nil {
		s.Credentials = map[string]*ServerCredentials{}
	}
	key := normalizeServerURL(serverURL)
	name = normalizeProfileName(name)
	sc := s.Credentials[key]
	if sc == nil {
		sc = &ServerCredentials{Profiles: map[string]*Credentials{}}
		s.Credentials[key] = sc
	}
	if sc.Profiles == nil {
		sc.Profiles = map[string]*Credentials{}
	}
	c.ServerURL = key
	sc.Profiles[name] = c
}

// Delete removes the default-profile credential for the given server
// URL. Other named profiles on the same server are left intact.
// Delegates to DeleteProfile(url, "default").
func (s *CredentialStore) Delete(serverURL string) {
	s.DeleteProfile(serverURL, defaultProfileName)
}

// DeleteProfile removes the named profile for the given server URL.
// No-op if the entry isn't present, or if the receiver is nil. When
// the last profile for a server is removed, the server key itself is
// dropped so we don't persist an empty profiles object.
func (s *CredentialStore) DeleteProfile(serverURL, name string) {
	if s == nil || s.Credentials == nil {
		return
	}
	key := normalizeServerURL(serverURL)
	sc := s.Credentials[key]
	if sc == nil || sc.Profiles == nil {
		return
	}
	delete(sc.Profiles, normalizeProfileName(name))
	if len(sc.Profiles) == 0 {
		delete(s.Credentials, key)
	}
}

// Save writes the store to disk in v3 format with mode 0600. Always
// writes the current version constant — even if the store was loaded
// from v1 or v2, this is the migration moment.
func (s *CredentialStore) Save() error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create credentials directory: %w", err)
	}

	s.Version = credentialsVersion
	if s.Credentials == nil {
		s.Credentials = map[string]*ServerCredentials{}
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	return nil
}

// WipeCredentialsFile removes ~/.pad/credentials.json entirely. Distinct
// from CredentialStore.Delete (which removes a single server's default
// profile) — used by tests and any future "pad auth wipe" that wants a
// clean slate. Silently succeeds if the file is already absent.
func WipeCredentialsFile() error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete credentials: %w", err)
	}
	return nil
}

// normalizeServerURL canonicalizes a server URL for use as a credential
// key. Trailing slash and surrounding whitespace are stripped; nothing
// else is touched (no scheme normalization, no port-canonicalization
// — those are deliberate, since differently-spelled URLs may legitimately
// reach different servers). Mirrors normalizeURL in cmd/pad/server_info.go
// so the two stay consistent.
func normalizeServerURL(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}

// normalizeProfileName trims whitespace and treats an empty name as the
// implicit default profile.
func normalizeProfileName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return defaultProfileName
	}
	return name
}

// profileOverride is the --profile flag value, set once per invocation
// from cmd/pad's PersistentPreRunE. Empty means "not set" so PAD_PROFILE
// and the implicit default can still win. Guarded by a mutex because
// tests in this package construct clients from multiple goroutines in
// other suites; CLI use is single-threaded.
var (
	profileOverrideMu sync.Mutex
	profileOverride   string
)

// SetProfileOverride records the --profile flag for this process. Pass
// "" to clear (so subsequent ResolveProfile calls fall through to
// PAD_PROFILE / default). Callers should invoke this from PersistentPreRunE
// on every command so a leftover value from a previous Execute in the
// same test process cannot leak.
func SetProfileOverride(name string) {
	profileOverrideMu.Lock()
	defer profileOverrideMu.Unlock()
	profileOverride = strings.TrimSpace(name)
}

// ResolveProfile returns the active credential profile for this
// invocation: --profile (SetProfileOverride) > PAD_PROFILE > "default".
// Selection is stateless; nothing is persisted.
func ResolveProfile() string {
	profileOverrideMu.Lock()
	override := profileOverride
	profileOverrideMu.Unlock()
	if override != "" {
		return override
	}
	if env := strings.TrimSpace(os.Getenv("PAD_PROFILE")); env != "" {
		return env
	}
	return defaultProfileName
}
