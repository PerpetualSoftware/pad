package cli

import (
	"os"
	"sync"
	"testing"
	"time"
)

// BUG-3838: the emptyConfirmed memo relied on a save's truncate moving the
// file's mtime even when the file was already empty. That was measured on
// Linux only; POSIX moves it only when the size changes. The save now sets
// the mtime itself before it truncates.

// posixTruncate gives the save's truncate the semantics of a platform where
// ftruncate leaves an empty file's mtime alone, and stalls the save after it
// until release is called. truncated is closed once the truncate has run.
// Cleanup releases a save the test left stalled, so a failing test leaves no
// goroutine blocked in the seam (codex r1 on BUG-3838).
func posixTruncate(t *testing.T, path string) (truncated chan struct{}, release func()) {
	truncated = make(chan struct{})
	stall := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(stall) }) }
	orig := truncateCredentialsFile
	truncateCredentialsFile = func(f *os.File, size int64) error {
		before, err := os.Stat(path)
		if err != nil {
			return err
		}
		if err := orig(f, size); err != nil {
			return err
		}
		if before.Size() == size {
			if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
				return err
			}
		}
		close(truncated)
		<-stall
		return nil
	}
	t.Cleanup(func() { truncateCredentialsFile = orig })
	t.Cleanup(release)
	return truncated, release
}

// A process has remembered a genuinely empty file. Another process saves over
// it without the lock, on a platform whose truncate leaves the mtime alone. A
// locked load between that save's truncate and its write must not take the
// memo: it re-reads and loads what the save wrote.
func TestBUG3838_MemoDoesNotSkipASaveWhoseTruncateKeepsTheMtime(t *testing.T) {
	home := withTempHome(t)
	path := writeCredsFile(t, home, "")
	resetEmptyConfirmed(t)
	shortRereads(t, 3, time.Millisecond)
	aged := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, aged, aged); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStore(); err != nil {
		t.Fatal(err)
	}
	emptyConfirmed.mu.Lock()
	remembered := emptyConfirmed.fi != nil
	emptyConfirmed.mu.Unlock()
	if !remembered {
		t.Fatal("the aged empty file was not remembered; the test needs the memo set")
	}

	truncated, release := posixTruncate(t, path)
	saved := make(chan error, 1)
	go func() { saved <- writeFileInPlace0600(path, []byte(twoServers)) }()
	// Registered after posixTruncate's, so it runs first: release, then wait.
	t.Cleanup(func() {
		release()
		if err := <-saved; err != nil {
			t.Error(err)
		}
	})
	select {
	case <-truncated:
	case err := <-saved:
		saved <- err // for the cleanup's receive
		t.Fatalf("the save returned before its truncate stalled: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the save never reached its truncate")
	}
	reads := countReads(t, func(n int) {
		if n == 2 {
			release()
			// Wait for the write without consuming the save's result, bounded
			// so a save that failed after the release fails the test instead
			// of hanging it (hunter r2 on BUG-3838); cleanup reports its error.
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
				if fi, err := os.Stat(path); err == nil && fi.Size() != 0 {
					return
				}
				time.Sleep(time.Millisecond)
			}
			t.Error("the released save never wrote the file")
		}
	})

	store, err := loadStoreLocked(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := *reads; n < 2 {
		t.Fatalf("reads = %d: the load answered from the memo during the save", n)
	}
	if store.Get("http://a:7777") == nil || store.Get("http://b:7777") == nil {
		t.Errorf("store = %+v, want the save's two entries", store.Credentials)
	}
}

// The last re-read is empty, and a save writes before the stat that follows
// it: the stat's non-zero size earns one more read instead of an empty store.
func TestBUG3838_FinalStatShowingABodyRereadsOnce(t *testing.T) {
	home := withTempHome(t)
	path := writeCredsFile(t, home, "")
	resetEmptyConfirmed(t)
	shortRereads(t, 3, time.Millisecond)
	orig := readCredentialsFile
	n := 0
	readCredentialsFile = func(p string) ([]byte, error) {
		n++
		data, err := orig(p)
		if n == 4 {
			if werr := os.WriteFile(path, []byte(twoServers), 0600); werr != nil {
				t.Error(werr)
			}
		}
		return data, err
	}
	t.Cleanup(func() { readCredentialsFile = orig })

	store, err := loadStoreLocked(path)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("reads = %d, want 5 (1+3 re-reads, then the one the stat earned)", n)
	}
	if store.Get("http://a:7777") == nil || store.Get("http://b:7777") == nil {
		t.Errorf("store = %+v, want the save's two entries", store.Credentials)
	}
}

// The touch lands on the file the save opened, not on whatever the path names
// by the time it runs: a symlink retargeted after the open must not take the
// stamp while the opened file keeps its old mtime (codex r3 on BUG-3838).
func TestBUG3838_TouchStampsTheOpenedFile(t *testing.T) {
	dir := t.TempDir()
	opened, other, link := dir+"/opened", dir+"/other", dir+"/credentials.json"
	aged := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, p := range []string{opened, other} {
		if err := os.WriteFile(p, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, aged, aged); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(opened, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	f, err := os.OpenFile(link, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}

	if err := touchCredentialsFile(f, link); err != nil {
		t.Fatalf("touch: %v", err)
	}
	for _, c := range []struct {
		path  string
		moved bool
	}{{opened, true}, {other, false}} {
		fi, err := os.Stat(c.path)
		if err != nil {
			t.Fatal(err)
		}
		if moved := !fi.ModTime().Equal(aged); moved != c.moved {
			t.Errorf("%s mtime moved = %v, want %v", c.path, moved, c.moved)
		}
	}
}
