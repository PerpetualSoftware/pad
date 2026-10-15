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
	"sync"
	"syscall"
	"time"
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
	err := withCredentialsLock(false, func(path string, locked bool) error {
		var err error
		if locked {
			store, err = loadStoreLocked(path)
		} else {
			store, err = loadStoreUnlocked(path)
		}
		return err
	})
	// A read is only a read: an empty file it could not tell from a save in
	// progress is answered as no credentials, as before (BUG-3828). The worst
	// case is a spurious "not logged in"; only UpdateStore must not act on it.
	if errors.Is(err, errNoCredentialsDir) || errors.Is(err, errCredentialsEmptyUnlocked) {
		return newEmptyStore(), nil
	}
	return store, err
}

// loadStoreLocked is LoadStore for a caller that holds the credentials lock.
//
// Holding the lock does not rule out a save in progress: a writer that waited
// out credentialsLockWait saves without it (BUG-3829), truncating in place
// before it writes. So an empty file is re-read with loadStoreUnlocked's
// bound before it is believed. One that stays empty is a genuinely empty
// file (a crash between a save's truncate and its write leaves one) and
// loads as an empty store, so login keeps working over it. Residual: an
// unlocked writer that stalls longer than the re-read bound (about 200ms)
// between its truncate and its write is still read as empty, and an
// UpdateStore landing there saves over it.
//
// A genuinely empty file pays the bound once per process, not once per load:
// emptyConfirmed remembers the file state it was confirmed for.
func loadStoreLocked(path string) (*CredentialStore, error) {
	for i := 0; ; i++ {
		data, err := readCredentialsFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return newEmptyStore(), nil
			}
			return nil, fmt.Errorf("read credentials: %w", err)
		}
		if len(strings.TrimSpace(string(data))) != 0 {
			return parseStore(data)
		}
		fi, serr := os.Stat(path)
		if serr == nil && emptyConfirmed.matches(path, fi) {
			return newEmptyStore(), nil
		}
		if i >= credentialsEmptyRereads {
			if serr == nil {
				emptyConfirmed.set(path, fi)
			}
			return newEmptyStore(), nil
		}
		time.Sleep(credentialsEmptyRereadPause)
	}
}

// emptyConfirmed is the empty credentials file a locked load already waited
// out (BUG-3829). A save that truncates a non-empty file, or writes into an
// empty one, moves its mtime, so a later empty read of a file a save is
// rewriting does not match and is re-read again.
//
// Only a file whose mtime is emptyConfirmedMinAge older than the confirmation
// is remembered. A later save is stamped no earlier than the confirmation, so
// on a filesystem with coarse timestamps it could otherwise land in the same
// tick and leave the mtime unchanged (Codex r1). A file emptied within the
// last two seconds just pays the re-read bound on each load until it ages.
// So does a file whose mtime is in the future (clock skew, Codex r2): the age
// check reads it as recent and never remembers it. That errs toward the
// re-read, which is the safe side, at the cost of the bound on every load.
var emptyConfirmed confirmedEmptyFile

const emptyConfirmedMinAge = 2 * time.Second

type confirmedEmptyFile struct {
	mu   sync.Mutex
	path string
	fi   fs.FileInfo
}

func (c *confirmedEmptyFile) matches(path string, fi fs.FileInfo) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fi != nil && c.path == path && fi.Size() == 0 &&
		os.SameFile(c.fi, fi) && c.fi.ModTime().Equal(fi.ModTime())
}

func (c *confirmedEmptyFile) set(path string, fi fs.FileInfo) {
	if fi != nil && time.Since(fi.ModTime()) < emptyConfirmedMinAge {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path, c.fi = path, fi
}

// errCredentialsEmptyUnlocked is loadStoreUnlocked's answer for a file that
// stayed empty on every re-read: unknown, not empty (BUG-3828).
var errCredentialsEmptyUnlocked = errors.New("credentials file is empty and could not be read under the lock; another pad process may be writing it")

// credentialsEmptyRereads and credentialsEmptyRereadPause bound how long a
// read waits for an empty credentials file to fill, with the lock or without
// it (BUG-3828, BUG-3829).
// Variables so a test can shorten them.
var (
	credentialsEmptyRereads     = 10
	credentialsEmptyRereadPause = 20 * time.Millisecond
)

// loadStoreUnlocked is loadStoreLocked for a caller that could not take the
// lock (BUG-3828). Save truncates in place before it writes, so an empty file
// read without the lock may be another process's save caught between the two,
// not an empty store. It is re-read a bounded number of times; one that stays
// empty is errCredentialsEmptyUnlocked, never an empty store, so UpdateStore
// cannot save a store built from it over every other server's entry.
func loadStoreUnlocked(path string) (*CredentialStore, error) {
	for i := 0; ; i++ {
		data, err := readCredentialsFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return newEmptyStore(), nil
			}
			return nil, fmt.Errorf("read credentials: %w", err)
		}
		if len(strings.TrimSpace(string(data))) != 0 {
			return parseStore(data)
		}
		if i >= credentialsEmptyRereads {
			return nil, errCredentialsEmptyUnlocked
		}
		time.Sleep(credentialsEmptyRereadPause)
	}
}

// readCredentialsFile is os.ReadFile, a variable so a test can count the
// re-reads and change the file between them (BUG-3828).
var readCredentialsFile = os.ReadFile

// parseStore parses a non-empty credentials file, v1 or v2.
func parseStore(data []byte) (*CredentialStore, error) {

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
// which reads under the shared side, never sees the truncated file of a save
// that took the lock. A save that waited the lock out writes without it, and
// its truncate can be read; loadStoreLocked re-reads an empty file for that
// (BUG-3829).
//
// Save alone does not stop two processes losing each other's entries; a
// load-modify-save goes through UpdateStore. Never call Save, LoadStore,
// UpdateStore or WipeCredentialsFile from an UpdateStore callback: they take
// the same lock and would wait on it forever (flock is per open file, so
// this process's own hold blocks them too).
func (s *CredentialStore) Save() error {
	return withCredentialsLock(true, func(path string, _ bool) error {
		return s.saveLocked(path)
	})
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
// as os.WriteFile did. A regular file is made 0600 before it is truncated,
// so the new tokens never land under a looser mode on a file we own; on one
// we cannot chmod (a foreign-owned file we may write) the write goes ahead,
// as os.WriteFile's did. Anything else a link may point at (/dev/null, a
// FIFO) is neither chmodded nor truncated, as O_TRUNC leaves it, and the
// sync is best effort since os.WriteFile never synced: a target os.WriteFile
// could write stays writable (final review round, both hunters).
//
// A failed stat fails the write before anything is written (BUG-3810): it
// cannot say whether the target is a regular file to truncate, and writing
// a shorter body without the truncate would leave the old tail behind.
func writeFileInPlace0600(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	fi, err := statOpenFile(f)
	if err != nil {
		_ = f.Close()
		return err
	}
	if fi.Mode().IsRegular() {
		_ = f.Chmod(0600)
		if err := f.Truncate(0); err != nil {
			_ = f.Close()
			return err
		}
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	_ = f.Sync()
	return f.Close()
}

// statOpenFile is f.Stat, a variable so a test can fail it (BUG-3810).
var statOpenFile = (*os.File).Stat

// credentialsLockWarning is where a write that could not take the lock says
// so; a variable so a test can read it.
var credentialsLockWarning io.Writer = os.Stderr

// withCredentialsLock runs fn with the path of credentials.json while
// holding credentials.json.lock: exclusive for a write, shared for a read.
// fn is told whether it holds the lock: a read without it cannot tell an
// empty file from a save in progress (BUG-3828). A read with it can tell only
// up to a bound, since a writer that waited out the lock saves without it
// (BUG-3829, loadStoreLocked).
//
// The lock sits on top of what os.WriteFile and os.ReadFile did and is never
// the reason a working command fails (BUG-3649 ruling): when it cannot be
// taken for any reason (a read-only or foreign-owned ~/.pad, a lock file a
// sudo run left behind, a filesystem without flock such as NFS without
// lockd), fn runs unlocked, as before. A write says so on stderr; a read
// stays quiet, because every pad command reads. EINTR is retried first. A
// read whose directory does not exist has nothing to read and returns
// errNoCredentialsDir rather than creating it.
func withCredentialsLock(exclusive bool, fn func(path string, locked bool) error) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if exclusive {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return fmt.Errorf("create credentials directory: %w", err)
		}
	}
	unlock, err := lockCredentialsRetrying(path+".lock", exclusive)
	if !exclusive && errors.Is(err, fs.ErrNotExist) {
		// No ~/.pad when the lock was opened. If it is still absent there is
		// nothing to read, and the read is decided here, without opening a
		// file a save may since have created. If a save created it in between,
		// its lock exists now (Save creates the lock before the file), so take
		// it rather than read a file that save may be truncating (Codex, 3a
		// round on option B).
		if _, serr := os.Stat(filepath.Dir(path)); errors.Is(serr, fs.ErrNotExist) {
			return errNoCredentialsDir
		}
		unlock, err = lockCredentialsRetrying(path+".lock", exclusive)
	}
	if err != nil {
		if exclusive {
			fmt.Fprintf(credentialsLockWarning, "warning: could not lock %s (%v); saving credentials without it\n", path+".lock", err)
		}
		return fn(path, false)
	}
	defer unlock()
	return fn(path, true)
}

// lockCredentials is lockCredentialsFile, swappable so a test can make the
// lock fail the way a filesystem without flock does.
var lockCredentials = lockCredentialsFile

// errCredentialsLockBusy is the platform lock's answer when another process
// holds it; the lock is only ever tried without blocking.
var errCredentialsLockBusy = errors.New("credentials lock is held by another process")

// credentialsLockWait bounds how long a busy lock is waited for. A holder
// that never lets go (a stopped process, a hung NFS lock) must not hang every
// pad command, so after this the caller proceeds unlocked (final review
// round). A variable so a test can shorten it.
var credentialsLockWait = 5 * time.Second

// lockCredentialsRetrying takes the lock, retrying a busy lock until
// credentialsLockWait passes and an interrupted call (EINTR) a few times.
func lockCredentialsRetrying(lockPath string, exclusive bool) (func(), error) {
	deadline := time.Now().Add(credentialsLockWait)
	interrupts := 0
	for {
		unlock, err := lockCredentials(lockPath, exclusive)
		switch {
		case errors.Is(err, syscall.EINTR) && interrupts < 4:
			interrupts++
		case errors.Is(err, errCredentialsLockBusy) && time.Now().Before(deadline):
			time.Sleep(20 * time.Millisecond)
		default:
			return unlock, err
		}
	}
}

// errNoCredentialsDir is a read's answer when ~/.pad does not exist: no
// credentials, decided without touching the file.
var errNoCredentialsDir = errors.New("no credentials directory")

// UpdateStore is the load-modify-save for credentials.json, under the
// exclusive credentials lock (BUG-3649): two `pad auth login` runs for
// different servers each used to load the same snapshot and save it with
// their own entry, so the second dropped the first's. fn mutates the
// freshly loaded store; when it returns an error nothing is saved. fn must
// not call Save, LoadStore, UpdateStore or WipeCredentialsFile (see Save);
// nothing slow (a network call) belongs inside it either, since the lock is
// held for the whole call.
//
// Without the lock, an empty file is not loaded as an empty store
// (BUG-3828): it may be another process's save between its truncate and its
// write, and saving fn's change over it would wipe every other server's
// credentials. UpdateStore re-reads it, and if it stays empty refuses with
// errCredentialsEmptyUnlocked and saves nothing. With the lock it re-reads
// too, since a writer that waited out the lock may be mid-save, and then
// treats one that stays empty as empty (BUG-3829, loadStoreLocked).
func UpdateStore(fn func(*CredentialStore) error) error {
	return withCredentialsLock(true, func(path string, locked bool) error {
		var store *CredentialStore
		var err error
		if locked {
			store, err = loadStoreLocked(path)
		} else {
			store, err = loadStoreUnlocked(path)
		}
		if errors.Is(err, errCredentialsEmptyUnlocked) {
			return fmt.Errorf("%w; nothing was saved, retry (if it persists with no other pad running, remove the empty %s)", err, path)
		}
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
	// No ~/.pad, nothing to wipe: answered without creating it, which on a
	// read-only home would fail where the wipe used to succeed (final review
	// round).
	if path, err := credentialsPath(); err == nil {
		if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
	}
	return withCredentialsLock(true, func(path string, _ bool) error {
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
