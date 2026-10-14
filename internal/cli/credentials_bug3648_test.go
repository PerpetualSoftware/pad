package cli

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
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

func TestBUG3648_SaveWritesThroughASymlinkedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	home := withTempHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".pad"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "dotfiles-credentials.json")
	if err := os.WriteFile(target, []byte(`{"version": 2, "credentials": {}}`), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".pad", "credentials.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	store := newEmptyStore()
	store.Set("http://x:7777", &Credentials{Token: "fresh"})
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("credentials.json is no longer a symlink after Save (err %v)", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"fresh"`) {
		t.Errorf("the symlink target did not receive the save: %s", data)
	}
}

// Rows 3-7 of the population table on BUG-3649: a symlinked credentials.json
// is written in place through the link, so a dangling link keeps being a
// link, a target in a read-only directory is still writable, and a loose
// target is tightened.
// longSeed is longer than the save that replaces it, so an in-place write
// that skipped the truncate would leave trailing bytes and invalid JSON.
var longSeed = []byte(`{"version": 2, "credentials": {"https://an-old-server.example.com:7777": {"token": "` + strings.Repeat("x", 400) + `"}}}`)

func symlinkedCredentials(t *testing.T, targetDir string, seed bool, mode os.FileMode) (link, target string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	home := withTempHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".pad"), 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, targetDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	target = filepath.Join(dir, "credentials.json")
	if seed {
		if err := os.WriteFile(target, longSeed, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, mode); err != nil {
			t.Fatal(err)
		}
	}
	link = filepath.Join(home, ".pad", "credentials.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link, target
}

func saveFresh(t *testing.T) {
	t.Helper()
	store := newEmptyStore()
	store.Set("http://x:7777", &Credentials{Token: "fresh"})
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func assertLinkedSave(t *testing.T, link, target string) {
	t.Helper()
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("credentials.json is no longer a symlink after Save (err %v)", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"fresh"`) || !json.Valid(data) {
		t.Errorf("the symlink target did not receive the save whole: %s", data)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0600 {
		t.Errorf("target mode = %o, want 600", got)
	}
}

func TestBUG3648_SaveKeepsADanglingSymlink(t *testing.T) {
	link, target := symlinkedCredentials(t, "dotfiles", false, 0)
	saveFresh(t)
	assertLinkedSave(t, link, target)
}

func TestBUG3648_SaveThroughASymlinkIntoAReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	link, target := symlinkedCredentials(t, "secrets", true, 0600)
	dir := filepath.Dir(target)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	saveFresh(t)
	assertLinkedSave(t, link, target)
}

func TestBUG3648_SaveTightensALooseSymlinkTarget(t *testing.T) {
	link, target := symlinkedCredentials(t, "dotfiles", true, 0644)
	saveFresh(t)
	assertLinkedSave(t, link, target)
}

// Row 6: a rename over a bind-mounted file fails EBUSY (measured in a
// container, BUG-3649), where os.WriteFile's in-place write worked, so Save
// falls back to writing in place.
func TestBUG3648_SaveWritesInPlaceWhenTheRenameIsBusy(t *testing.T) {
	home := withTempHome(t)
	path := filepath.Join(home, ".pad", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, longSeed, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	orig := osRename
	osRename = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EBUSY}
	}
	t.Cleanup(func() { osRename = orig })
	saveFresh(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"fresh"`) || !json.Valid(data) {
		t.Errorf("the busy target did not receive the save whole: %s", data)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0600 {
			t.Errorf("mode = %o, want 600", got)
		}
	}
}
