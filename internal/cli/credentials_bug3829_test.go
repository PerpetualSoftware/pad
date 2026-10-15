package cli

import (
	"os"
	"testing"
	"time"
)

// BUG-3829: a writer that waits out credentialsLockWait saves without the
// lock, truncating in place before it writes. A loader that DOES hold the
// lock used to read that truncate as an empty store on the first read, and
// an UpdateStore landing there saved its one change over every other
// server's entry.

func resetEmptyConfirmed(t *testing.T) {
	emptyConfirmed.set("", nil)
	t.Cleanup(func() { emptyConfirmed.set("", nil) })
}

// A locked update whose read lands inside an unlocked writer's save re-reads
// until that save has written, then applies to what it wrote.
func TestBUG3829_LockedUpdateRereadsAcrossAnUnlockedSave(t *testing.T) {
	home := withTempHome(t)
	path := writeCredsFile(t, home, twoServers)
	resetEmptyConfirmed(t)
	shortRereads(t, 5, time.Millisecond)
	reads := midSave(t, path, 1, 3)

	if err := UpdateStore(func(s *CredentialStore) error {
		s.Set("http://c:7777", &Credentials{Token: "tc"})
		return nil
	}); err != nil {
		t.Fatalf("UpdateStore: %v", err)
	}
	if *reads != 3 {
		t.Errorf("reads = %d, want 3 (empty, empty, then the save's body)", *reads)
	}
	store, err := LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"http://a:7777", "http://b:7777", "http://c:7777"} {
		if store.Get(url) == nil {
			t.Errorf("%s missing after the update: %+v", url, store.Credentials)
		}
	}
}

// A file that stays empty under the lock is genuinely empty (a crash between
// a save's truncate and its write): it loads as an empty store, and login
// still saves over it.
func TestBUG3829_LockedGenuinelyEmptyFileStillLogsIn(t *testing.T) {
	home := withTempHome(t)
	path := writeCredsFile(t, home, "")
	resetEmptyConfirmed(t)
	shortRereads(t, 3, time.Millisecond)

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore over an empty locked file: %v", err)
	}
	if len(store.Credentials) != 0 {
		t.Fatalf("store = %+v, want empty", store.Credentials)
	}
	if err := UpdateStore(func(s *CredentialStore) error {
		s.Set("http://c:7777", &Credentials{Token: "tc"})
		return nil
	}); err != nil {
		t.Fatalf("UpdateStore over an empty locked file: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := parseStore(data)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Get("http://c:7777") == nil {
		t.Errorf("the login was not saved: %s", data)
	}
}

// A genuinely empty file pays the re-read bound once per process, not once
// per load: a command that loads credentials several times waits once. A
// save that touches the file (its mtime moves) is re-read again.
func TestBUG3829_GenuinelyEmptyFileWaitsOncePerProcess(t *testing.T) {
	home := withTempHome(t)
	path := writeCredsFile(t, home, "")
	resetEmptyConfirmed(t)
	shortRereads(t, 3, time.Millisecond)
	reads := countReads(t, nil)

	for i := 0; i < 3; i++ {
		if _, err := LoadStore(); err != nil {
			t.Fatal(err)
		}
	}
	if *reads != 4+1+1 {
		t.Errorf("reads over three loads = %d, want 6 (1+3 re-reads, then 1 each)", *reads)
	}

	// Another save truncates the file again: a new state, waited out again.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	*reads = 0
	if _, err := LoadStore(); err != nil {
		t.Fatal(err)
	}
	if *reads != 4 {
		t.Errorf("reads after the file changed = %d, want 4 (1+3 re-reads)", *reads)
	}
}
