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

// An empty file that fills while it is being re-read is a save that was in
// progress: the update applies to what it wrote, and keeps every entry.
func TestBUG3828_UnlockedUpdateRereadsAnEmptyFile(t *testing.T) {
	home := withTempHome(t)
	path := writeCredsFile(t, home, "")
	noFlock(t)
	shortRereads(t, 5, time.Millisecond)

	// The other process's save lands just before the third read: the first
	// two saw the empty file.
	reads := countReads(t, func(n int) {
		if n == 3 {
			if err := os.WriteFile(path, []byte(twoServers), 0600); err != nil {
				t.Error(err)
			}
		}
	})
	if err := UpdateStore(func(s *CredentialStore) error {
		s.Set("http://c:7777", &Credentials{Token: "tc"})
		return nil
	}); err != nil {
		t.Fatalf("UpdateStore: %v", err)
	}
	if *reads != 3 {
		t.Fatalf("UpdateStore read the file %d times, want 3 (two empty, then the save's)", *reads)
	}
	store, err := LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"http://a:7777", "http://b:7777", "http://c:7777"} {
		if store.Get(u) == nil {
			t.Errorf("%s missing after the update: %+v", u, store.Credentials)
		}
	}
}

// A read is only a read: an empty file it cannot tell from a save in progress
// is re-read, and if it stays empty answers no credentials, as before.
func TestBUG3828_UnlockedReadRereadsThenAnswersEmpty(t *testing.T) {
	home := withTempHome(t)
	path := writeCredsFile(t, home, "")
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

	*reads = 0
	countReads(t, func(n int) {
		if n == 2 {
			if err := os.WriteFile(path, []byte(twoServers), 0600); err != nil {
				t.Error(err)
			}
		}
	})
	store, err = LoadStore()
	if err != nil || store.Get("http://a:7777") == nil {
		t.Errorf("a re-read did not see the file fill: %+v, %v", store, err)
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
