package cli

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// BUG-3828: Save truncates credentials.json in place before it writes, under
// the exclusive lock. A caller that could not take the lock (a holder past
// credentialsLockWait, a filesystem without flock) used to read an empty file
// as an empty store, and UpdateStore then saved its one change over it,
// wiping every other server's credentials once the other save finished.

// noFlock makes every lock attempt fail the way NFS without lockd does, so
// reads and writes take the unlocked path.
func noFlock(t *testing.T) *strings.Builder {
	return swapLock(t, func(string, bool) (func(), error) {
		return nil, &fs.PathError{Op: "flock", Path: "x", Err: syscall.ENOLCK}
	})
}

// countReads counts credentials-file reads, and calls onRead (when set) with
// the read's 1-based number before it happens.
func countReads(t *testing.T, onRead func(n int)) *int {
	orig := readCredentialsFile
	n := 0
	readCredentialsFile = func(path string) ([]byte, error) {
		n++
		if onRead != nil {
			onRead(n)
		}
		return orig(path)
	}
	t.Cleanup(func() { readCredentialsFile = orig })
	return &n
}

func shortRereads(t *testing.T, n int, pause time.Duration) {
	origN, origPause := credentialsEmptyRereads, credentialsEmptyRereadPause
	credentialsEmptyRereads, credentialsEmptyRereadPause = n, pause
	t.Cleanup(func() { credentialsEmptyRereads, credentialsEmptyRereadPause = origN, origPause })
}

const twoServers = `{"version": 2, "credentials": {"http://a:7777": {"token": "ta"}, "http://b:7777": {"token": "tb"}}}`

// An empty file read without the lock that stays empty is refused: nothing
// is saved, so when the other process's save lands its entries are intact.
func TestBUG3828_UnlockedUpdateRefusesAnEmptyFile(t *testing.T) {
	home := withTempHome(t)
	path := writeCredsFile(t, home, "")
	noFlock(t)
	shortRereads(t, 3, time.Millisecond)

	ran := false
	err := UpdateStore(func(s *CredentialStore) error {
		ran = true
		s.Set("http://c:7777", &Credentials{Token: "tc"})
		return nil
	})
	if !errors.Is(err, errCredentialsEmptyUnlocked) {
		t.Fatalf("UpdateStore over an empty unlocked file = %v, want errCredentialsEmptyUnlocked", err)
	}
	if ran {
		t.Error("UpdateStore ran its callback on a store built from an empty unlocked read")
	}
	if data, _ := os.ReadFile(path); len(data) != 0 {
		t.Fatalf("the refusal wrote the file: %q", data)
	}
	// The other process's save finishes.
	if err := os.WriteFile(path, []byte(twoServers), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	if store.Get("http://a:7777") == nil || store.Get("http://b:7777") == nil {
		t.Errorf("the other servers' entries were lost: %+v", store.Credentials)
	}
}

// midSave makes the read numbered truncateAt land just after another
// process's save truncated the file, and that save's write land just before
// the read numbered writeAt: the in-place truncate-then-write an unlocked
// read can overlap.
func midSave(t *testing.T, path string, truncateAt, writeAt int) *int {
	return countReads(t, func(n int) {
		var err error
		switch n {
		case truncateAt:
			err = os.Truncate(path, 0)
		case writeAt:
			err = os.WriteFile(path, []byte(twoServers), 0600)
		}
		if err != nil {
			t.Error(err)
		}
	})
}

// An update whose unlocked read overlaps another process's save re-reads
// until the save has written, then applies to what it wrote: every entry
// is kept.
func TestBUG3828_UnlockedUpdateRereadsAcrossASave(t *testing.T) {
	home := withTempHome(t)
	path := writeCredsFile(t, home, twoServers)
	noFlock(t)
	shortRereads(t, 5, time.Millisecond)
	reads := midSave(t, path, 1, 3)

	if err := UpdateStore(func(s *CredentialStore) error {
		s.Set("http://c:7777", &Credentials{Token: "tc"})
		return nil
	}); err != nil {
		t.Fatalf("UpdateStore: %v", err)
	}
	if *reads != 3 {
		t.Fatalf("UpdateStore read the file %d times, want 3 (two inside the save, then its result)", *reads)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"http://a:7777", "http://b:7777", "http://c:7777"} {
		if !strings.Contains(string(data), u) {
			t.Errorf("%s missing from the saved file after the update:\n%s", u, data)
		}
	}
}

// The same overlap, with a save that has not written by the last re-read:
// the update refuses and saves nothing, so the save's entries land intact.
func TestBUG3828_UnlockedUpdateRefusesASaveStillInProgress(t *testing.T) {
	home := withTempHome(t)
	path := writeCredsFile(t, home, twoServers)
	noFlock(t)
	shortRereads(t, 3, time.Millisecond)
	reads := midSave(t, path, 1, 0)

	err := UpdateStore(func(s *CredentialStore) error {
		s.Set("http://c:7777", &Credentials{Token: "tc"})
		return nil
	})
	if !errors.Is(err, errCredentialsEmptyUnlocked) {
		t.Fatalf("UpdateStore inside a save = %v, want errCredentialsEmptyUnlocked", err)
	}
	if *reads != 4 {
		t.Errorf("UpdateStore read the file %d times, want 4 (the read and its 3 re-reads)", *reads)
	}
	if data, _ := os.ReadFile(path); len(data) != 0 {
		t.Fatalf("the refusal wrote over the save in progress: %q", data)
	}
}

// A read is only a read: an empty file it cannot tell from a save in progress
// is re-read, and if it stays empty answers no credentials, as before.
func TestBUG3828_UnlockedReadRereadsThenAnswersEmpty(t *testing.T) {
	home := withTempHome(t)
	writeCredsFile(t, home, "")
	noFlock(t)
	shortRereads(t, 5, time.Millisecond)

	reads := countReads(t, nil)
	store, err := LoadStore()
	if err != nil || len(store.Credentials) != 0 {
		t.Fatalf("LoadStore over an empty unlocked file = %+v, %v; want an empty store", store, err)
	}
	if *reads != 6 {
		t.Errorf("LoadStore read the empty file %d times, want 6 (the read and its 5 re-reads)", *reads)
	}

}

// A read that overlaps a save re-reads until the save has written.
func TestBUG3828_UnlockedReadRereadsAcrossASave(t *testing.T) {
	home := withTempHome(t)
	path := writeCredsFile(t, home, twoServers)
	noFlock(t)
	shortRereads(t, 5, time.Millisecond)
	reads := midSave(t, path, 1, 2)

	store, err := LoadStore()
	if err != nil || store.Get("http://a:7777") == nil || store.Get("http://b:7777") == nil {
		t.Errorf("a read across a save = %+v, %v; want both entries", store, err)
	}
	if *reads != 2 {
		t.Errorf("LoadStore read the file %d times, want 2", *reads)
	}
}

// Under the lock nothing can be mid-save, so empty still means empty: no
// re-reads, and an update saves.
func TestBUG3828_LockedEmptyFileIsEmpty(t *testing.T) {
	home := withTempHome(t)
	writeCredsFile(t, home, "")
	shortRereads(t, 5, time.Millisecond)

	reads := countReads(t, nil)
	if err := UpdateStore(func(s *CredentialStore) error {
		s.Set("http://c:7777", &Credentials{Token: "tc"})
		return nil
	}); err != nil {
		t.Fatalf("UpdateStore over an empty locked file: %v", err)
	}
	if *reads != 1 {
		t.Errorf("a locked update read the empty file %d times, want 1: it re-read", *reads)
	}
	store, _ := LoadStore()
	if store.Get("http://c:7777") == nil {
		t.Errorf("the update did not land: %+v", store.Credentials)
	}
}
