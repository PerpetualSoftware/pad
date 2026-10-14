package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// BUG-3648: Save used os.WriteFile, which applies 0600 only when it creates
// the file, so an existing loose-mode credentials.json kept its mode with
// fresh tokens written into it.
func TestBUG3648_SaveTightensALooseExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	home := withTempHome(t)
	path := writeCredsFile(t, home, `{"version": 2, "credentials": {}}`)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	store := newEmptyStore()
	store.Set("http://x:7777", &Credentials{Token: "fresh"})
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("credentials.json mode after Save over a 0644 file = %o, want 0600", got)
	}
}

func TestBUG3648_SaveCreatesThePadDirPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	home := withTempHome(t)
	store := newEmptyStore()
	store.Set("http://x:7777", &Credentials{Token: "t"})
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(filepath.Join(home, ".pad"))
	if err != nil {
		t.Fatalf("stat .pad: %v", err)
	}
	if got := info.Mode().Perm(); got != 0700 {
		t.Errorf(".pad mode = %o, want 0700", got)
	}
	entries, _ := os.ReadDir(filepath.Join(home, ".pad"))
	for _, e := range entries {
		if e.Name() != "credentials.json" {
			t.Errorf("Save left %q behind in .pad", e.Name())
		}
	}
}

// BUG-3649: two load-modify-save runs (e.g. `pad auth login` against two
// servers) each loaded the same snapshot and saved it with only their own
// entry, so the later save dropped the earlier one's. The sleep inside fn
// holds every writer between its load and its save, which is the window.
func TestBUG3649_ConcurrentUpdatesKeepEveryEntry(t *testing.T) {
	withTempHome(t)
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- UpdateStore(func(s *CredentialStore) error {
				time.Sleep(20 * time.Millisecond)
				s.Set(fmt.Sprintf("http://server-%d:7777", i), &Credentials{Token: fmt.Sprintf("t%d", i)})
				return nil
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("UpdateStore: %v", err)
		}
	}
	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if got := len(store.Credentials); got != n {
		t.Errorf("after %d concurrent updates the store holds %d entries, want %d: %v", n, got, n, store.Credentials)
	}
}

func TestBUG3649_UpdateStoreSavesNothingWhenFnFails(t *testing.T) {
	home := withTempHome(t)
	writeCredsFile(t, home, `{"version": 2, "credentials": {"http://keep:7777": {"token": "k"}}}`)
	want := readCredsFile(t, home)
	err := UpdateStore(func(s *CredentialStore) error {
		s.Delete("http://keep:7777")
		return fmt.Errorf("refused")
	})
	if err == nil {
		t.Fatalf("UpdateStore returned nil for a failing fn")
	}
	if got := readCredsFile(t, home); got != want {
		t.Errorf("a failing fn still saved: %s", got)
	}
}

// BUG-3649: Save truncated the live file before writing it, and LoadStore
// reads an empty file as an empty store, so a reader racing a save could
// see no credentials, or half a JSON document. The store is large so each
// write takes long enough for the reader to land inside it.
func TestBUG3649_ReaderNeverSeesAPartialFile(t *testing.T) {
	withTempHome(t)
	store := newEmptyStore()
	for i := range 3000 {
		store.Set(fmt.Sprintf("http://server-%04d:7777", i), &Credentials{Token: fmt.Sprintf("token-%04d-padding-padding-padding", i)})
	}
	if err := store.Save(); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := store.Save(); err != nil {
				t.Errorf("Save: %v", err)
				return
			}
		}
	}()
	deadline := time.Now().Add(1500 * time.Millisecond)
	reads := 0
	for time.Now().Before(deadline) {
		got, err := LoadStore()
		reads++
		if err != nil {
			t.Errorf("read %d during a save: %v", reads, err)
			break
		}
		if len(got.Credentials) != 3000 {
			t.Errorf("read %d during a save saw %d entries, want 3000", reads, len(got.Credentials))
			break
		}
	}
	close(stop)
	wg.Wait()
}

// TestBUG3649_GuardNoUnlockedCredentialWrites is the wiring half of the fix:
// UpdateStore only helps if the writers use it. Any non-test file that loads
// the store into a variable and calls Save on that variable is a
// load-modify-save outside the lock.
func TestBUG3649_GuardNoUnlockedCredentialWrites(t *testing.T) {
	root := filepath.Join("..", "..")
	load := regexp.MustCompile(`(\w+), \w+ :?= (?:cli\.)?LoadStore\(\)`)
	scanned := 0
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			if filepath.Base(path) == "credentials.go" && filepath.Base(filepath.Dir(path)) == "cli" {
				return nil // UpdateStore itself
			}
			for _, m := range load.FindAllStringSubmatch(string(src), -1) {
				if regexp.MustCompile(`\b` + regexp.QuoteMeta(m[1]) + `\.Save\(\)`).Match(src) {
					t.Errorf("%s loads the credentials store into %q and saves it outside UpdateStore", path, m[1])
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if scanned < 100 {
		t.Fatalf("guard scanned only %d files; the walk root is wrong", scanned)
	}
}

func TestBUG3648_LockFileIsTightenedWhenItExists(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	home := withTempHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".pad"), 0700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(home, ".pad", "credentials.json.lock")
	if err := os.WriteFile(lock, nil, 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lock, 0666); err != nil {
		t.Fatal(err)
	}
	if err := UpdateStore(func(*CredentialStore) error { return nil }); err != nil {
		t.Fatalf("UpdateStore: %v", err)
	}
	info, err := os.Stat(lock)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("lock file mode = %o, want 0600", got)
	}
}

// TestBUG3649_WipeWaitsForTheLock: a wipe racing UpdateStore could be
// undone by the update's rename, so Wipe takes the same lock (Codex round
// 1). Holding the lock must hold Wipe; releasing it must let Wipe finish.
func TestBUG3649_WipeWaitsForTheLock(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("no cross-process credentials lock on this platform")
	}
	home := withTempHome(t)
	path := writeCredsFile(t, home, `{"version": 2, "credentials": {"http://x:7777": {"token": "t"}}}`)
	unlock, err := lockCredentialsFile(path + ".lock")
	if err != nil {
		t.Fatalf("lockCredentialsFile: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- WipeCredentialsFile() }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("WipeCredentialsFile returned (%v) while the credentials lock was held", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("credentials.json removed while the lock was held: %v", err)
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WipeCredentialsFile: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("WipeCredentialsFile did not finish after the lock was released")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("credentials.json still present after the wipe: %v", err)
	}
}
