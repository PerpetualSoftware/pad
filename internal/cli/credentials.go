package cli

// Per-server credentials store (TASK-1228 / IDEA-1226).
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
//       "http://127.0.0.1:7777":       {"token": "...", "user_id": "...", ...},
//       "https://pad-staging:7777":    {"token": "...", "user_id": "...", ...}
//     }
//   }
//
// Reads transparently migrate v1 → v2 in memory; the next Save writes v2.
// We deliberately do NOT save-on-read so reads stay side-effect free —
// the migration becomes durable on the first login/logout/setup.
//
// Single-server users see no behavior change: one entry, same shape per
// entry, identical UX.
//
// No top-level `default` field. The configured server URL
// (cfg.BaseURL() from ~/.pad/config.toml or --url) is always the
// authoritative answer for "which server am I targeting" — a separate
// `default` would create a second source of truth and the split-brain
// bugs that follow. The v2 schema can be extended later if a use case
// shows up; the current map shape is forward compatible.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// credentialsVersion is what new saves write. Bump and add a migration
// branch in LoadStore when the on-disk shape changes.
const credentialsVersion = 2

// Credentials is the per-server login blob. The `server_url` JSON tag is
// kept (with omitempty) so v1 files still parse during the migration
// path; in v2 the URL is the map key and the field is redundant — Set
// mirrors the key into the field so consumers reading the value alone
// still see a consistent ServerURL.
type Credentials struct {
	ServerURL string `json:"server_url,omitempty"`
	Token     string `json:"token"`
	UserID    string `json:"user_id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
}

// CredentialStore is the on-disk shape of `~/.pad/credentials.json` (v2).
// Keyed by canonical server URL (trailing slash + surrounding whitespace
// stripped — see normalizeServerURL).
type CredentialStore struct {
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
// anywhere." Migrates v1 single-blob format to v2 in memory; v1 files
// stay v1 on disk until the first Save (which always writes v2).
//
// LoadStore is the only intended way to read credentials. Internal
// callers that need a single server's entry should call
// LoadStore().Get(url).
//
// It reads under a shared lock on credentials.json.lock (BUG-3649), so it
// never sees a save half-written: Save writes in place, as os.WriteFile
// did, under the exclusive side of the same lock.
func LoadStore() (*CredentialStore, error) {
	var store *CredentialStore
	err := withCredentialsLock(false, func(path string) error {
		var err error
		store, err = loadStoreLocked(path)
		return err
	})
	return store, err
}

// loadStoreLocked is LoadStore for a caller that holds the credentials lock.
func loadStoreLocked(path string) (*CredentialStore, error) {
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
	// top-level `credentials` object. Probe with a generic map so a
	// garbage file fails loudly here rather than silently parsing as
	// "empty v2."
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	if _, hasToken := probe["token"]; hasToken {
		return migrateV1(data)
	}
	return parseV2(data)
}

// migrateV1 reads a v1 single-blob file and returns an in-memory v2 store
// keyed by the legacy ServerURL. A v1 file with an empty server_url is
// treated as "no usable credentials" (returns an empty store) — that's
// the only safe interpretation, since we have no key to file the entry
// under. The on-disk file is unchanged; callers triggering Save (login,
// logout, setup) will write v2.
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

// parseV2 unmarshals a v2 file. Tolerates a missing or zero version
// field and a nil credentials map by normalizing both — anything else
// (malformed JSON, wrong types) surfaces as an error.
func parseV2(data []byte) (*CredentialStore, error) {
	var store CredentialStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("parse v2 credentials: %w", err)
	}
	if store.Credentials == nil {
		store.Credentials = map[string]*Credentials{}
	}
	if store.Version == 0 {
		store.Version = credentialsVersion
	}
	return &store, nil
}

func newEmptyStore() *CredentialStore {
	return &CredentialStore{
		Version:     credentialsVersion,
		Credentials: map[string]*Credentials{},
	}
}

// Get returns the credential for the given server URL, or nil if none
// exists. URL is canonicalized (trailing slash + whitespace stripped)
// before lookup. Nil-receiver safe — callers can write
// `store.Get(url)` without a prior nil check on store.
func (s *CredentialStore) Get(serverURL string) *Credentials {
	if s == nil || s.Credentials == nil {
		return nil
	}
	return s.Credentials[normalizeServerURL(serverURL)]
}

// Set adds or replaces the credential for the given server URL. The
// canonical URL is mirrored into the value's ServerURL field so a
// caller reading the credential standalone still sees a consistent
// URL. Passing a nil credential is a no-op (use Delete to remove).
func (s *CredentialStore) Set(serverURL string, c *Credentials) {
	if c == nil {
		return
	}
	if s.Credentials == nil {
		s.Credentials = map[string]*Credentials{}
	}
	key := normalizeServerURL(serverURL)
	c.ServerURL = key
	s.Credentials[key] = c
}

// Delete removes the credential for the given server URL. No-op if the
// entry isn't present, or if the receiver is nil.
func (s *CredentialStore) Delete(serverURL string) {
	if s == nil || s.Credentials == nil {
		return
	}
	delete(s.Credentials, normalizeServerURL(serverURL))
}

// Save writes the store to disk in v2 format with mode 0600. Always
// writes the current version constant — even if the store was loaded
// from v1, this is the migration moment.
//
// The write is in place, the shape os.WriteFile had, so a symlinked,
// bind-mounted, foreign-owned or ACL-carrying credentials.json, or one in a
// read-only directory, keeps working as it did (option B on BUG-3649: an
// atomic temp-plus-rename was worse than os.WriteFile for each of those).
// What changes is privacy and readers. os.WriteFile applied 0600 only when
// it CREATED the file, so an existing loose-mode file kept its mode with
// fresh tokens in it (BUG-3648); Save chmods before it truncates. And it
// writes under the exclusive side of the credentials lock, so LoadStore,
// which reads under the shared side, never sees the truncated file.
//
// Save alone does not stop two processes losing each other's entries; a
// load-modify-save goes through UpdateStore. Never call Save, LoadStore,
// UpdateStore or WipeCredentialsFile from an UpdateStore callback: they take
// the same lock and would wait on it forever (flock is per open file, so
// this process's own hold blocks them too).
func (s *CredentialStore) Save() error {
	return withCredentialsLock(true, s.saveLocked)
}

// saveLocked is Save for a caller that holds the credentials lock.
func (s *CredentialStore) saveLocked(path string) error {
	s.Version = credentialsVersion
	if s.Credentials == nil {
		s.Credentials = map[string]*Credentials{}
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	if err := writeFileInPlace0600(path, data); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	return nil
}

// writeFileInPlace0600 writes data over path in place, following a symlink,
// as os.WriteFile did. The file is made 0600 before it is truncated, so the
// new tokens never land under a looser mode on a file we own; on one we
// cannot chmod (a foreign-owned file we may write) the write goes ahead, as
// os.WriteFile's did.
func writeFileInPlace0600(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	_ = f.Chmod(0600)
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// credentialsLockWarning is where a write that could not take the lock says
// so; a variable so a test can read it.
var credentialsLockWarning io.Writer = os.Stderr

// withCredentialsLock runs fn with the path of credentials.json while
// holding credentials.json.lock: exclusive for a write, shared for a read.
//
// A lock file this user may not create or open (a read-only or foreign-owned
// ~/.pad, or a lock file a sudo run left behind) is EACCES, EPERM or EROFS,
// and then fn runs unlocked, as every read and write did before BUG-3649;
// a write says so on stderr, a read stays quiet because every pad command
// reads. Any other lock error is returned. A read whose directory does not
// exist has nothing to read and runs unlocked rather than creating it.
func withCredentialsLock(exclusive bool, fn func(path string) error) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if exclusive {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return fmt.Errorf("create credentials directory: %w", err)
		}
	}
	unlock, err := lockCredentialsFile(path+".lock", exclusive)
	switch {
	case err == nil:
		defer unlock()
	case !exclusive && errors.Is(err, fs.ErrNotExist):
	case lockRefused(err):
		if exclusive {
			fmt.Fprintf(credentialsLockWarning, "warning: could not lock %s (%v); saving credentials without it\n", path+".lock", err)
		}
	default:
		return fmt.Errorf("lock credentials: %w", err)
	}
	return fn(path)
}

// lockRefused reports whether a lock error means this user may not create or
// open the lock file, rather than that locking itself failed.
func lockRefused(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS)
}

// UpdateStore is the load-modify-save for credentials.json, under the
// exclusive credentials lock (BUG-3649): two `pad auth login` runs for
// different servers each used to load the same snapshot and save it with
// their own entry, so the second dropped the first's. fn mutates the
// freshly loaded store; when it returns an error nothing is saved. fn must
// not call Save, LoadStore, UpdateStore or WipeCredentialsFile (see Save);
// nothing slow (a network call) belongs inside it either, since the lock is
// held for the whole call.
func UpdateStore(fn func(*CredentialStore) error) error {
	return withCredentialsLock(true, func(path string) error {
		store, err := loadStoreLocked(path)
		if err != nil {
			return err
		}
		if err := fn(store); err != nil {
			return err
		}
		return store.saveLocked(path)
	})
}

// WipeCredentialsFile removes ~/.pad/credentials.json entirely. Distinct
// from CredentialStore.Delete (which removes a single server's entry)
// — used by tests and any future "pad auth wipe" that wants a clean
// slate. Silently succeeds if the file is already absent.
//
// Under the exclusive lock, so it cannot land in the middle of a save
// (Codex round 1, BUG-3649). Never call it from an UpdateStore callback.
func WipeCredentialsFile() error {
	return withCredentialsLock(true, func(path string) error {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("delete credentials: %w", err)
		}
		return nil
	})
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
