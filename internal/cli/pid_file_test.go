package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// BUG-2965: `pad server stop` answered "server not running (no PID file)" about
// a live server, because the file was written by exactly one path —
// EnsureServer's auto-start branch — and never by `pad server start` itself.
// EnsureServer spawns THIS command and records the child's pid, so writing our
// own pid here produces the same value by a second route; what it adds is the
// case where a human, a service unit, or a refresh recipe ran the command
// directly.
func TestWritePIDFile_WritesAndCleansUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.pid")

	cleanup := WritePIDFile(path, 4242)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("PID file not written: %v", err)
	}
	if got := string(data); got != strconv.Itoa(4242) {
		t.Errorf("PID file contains %q, want %q — StopServer parses this with strconv.Atoi", got, "4242")
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("PID file survived cleanup (stat err = %v) — a stale file names a dead process to the next stop", err)
	}
}

// TestWritePIDFile_UnwritablePathIsNotFatal pins the degradation. The server is
// what the user asked for; a missing PID file costs them the addressable-stop
// path and nothing else, so this must not panic and its cleanup must stay safe
// to call.
func TestWritePIDFile_UnwritablePathIsNotFatal(t *testing.T) {
	// A path whose parent is a FILE, not a directory — unwritable without
	// needing permissions games that behave differently under root.
	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	path := filepath.Join(parent, "pad.pid")

	cleanup := WritePIDFile(path, 4242)
	if cleanup == nil {
		t.Fatal("cleanup must never be nil — the caller defers it unconditionally")
	}
	cleanup() // must not panic, and must not care that there is nothing to remove

	// Read rather than IsNotExist: a path UNDER a non-directory fails with
	// ENOTDIR, not ENOENT, so os.IsNotExist is false even though nothing is
	// there. The claim is "no PID file can be read here", so read.
	if _, err := os.ReadFile(path); err == nil {
		t.Errorf("a PID file is readable at %s after a failed write", path)
	}
}

// TestWritePIDFile_CleanupToleratesAnAlreadyRemovedFile covers the ordinary
// double-stop shape: something else (an operator, a stop that raced) removed
// the file first. Cleanup must be quiet about that rather than logging a
// failure on every clean shutdown.
func TestWritePIDFile_CleanupToleratesAnAlreadyRemovedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.pid")
	cleanup := WritePIDFile(path, 99)
	if err := os.Remove(path); err != nil {
		t.Fatalf("pre-remove: %v", err)
	}
	cleanup()
}

// TestWritePIDFile_CleanupDoesNotRemoveSomeoneElsesFile is the P1's second
// half, and the sharper one. A duplicate start writes the file (the original's
// pid was stale, or the write raced), fails to bind, and runs its cleanup — if
// that cleanup is unconditional it deletes whatever is there, including a file
// another instance has since written, leaving a HEALTHY server unaddressable.
// That is this fix's own defect, reintroduced by its own cleanup.
func TestWritePIDFile_CleanupDoesNotRemoveSomeoneElsesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.pid")

	cleanup := WritePIDFile(path, 4242)

	// Another instance takes ownership while we are still running.
	if err := os.WriteFile(path, []byte("777"), 0o644); err != nil {
		t.Fatalf("takeover: %v", err)
	}

	cleanup()

	if got, ok := readPIDFile(path); !ok || got != 777 {
		t.Errorf("PID file is %v (ok=%v) after our cleanup, want 777 — cleanup must only remove a file it still owns", got, ok)
	}
}

// Removed with codex round 3's P2: TestWritePIDFile_LeavesALivePeersFileAlone
// and TestWritePIDFile_ReplacesAStaleFile pinned a live-pid refusal that is
// wrong once the caller binds first — no live process can be serving an address
// this process just bound, so deferring to one strands the real server. The
// TestProcessIsAlive_* pair went with the predicate they covered, which had no
// production caller left. Their replacement is the ordering itself, pinned in
// internal/server (Listen refuses a held port) and exercised end to end against
// the real binary on BUG-2965's trail.

// TestWritePIDFile_CleanupIsIdempotent covers the shape the shutdown path now
// relies on: the release runs explicitly before the listener closes AND stays
// deferred as a backstop for the paths that never reach the shutdown sequence.
// Calling it twice must be quiet and must not touch a successor's file.
func TestWritePIDFile_CleanupIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.pid")
	cleanup := WritePIDFile(path, 4242)

	cleanup()
	if _, ok := readPIDFile(path); ok {
		t.Fatal("first cleanup left the file behind")
	}

	// A successor claims the path between the two calls — the ordinary fast
	// restart. The second call must leave it alone.
	if err := os.WriteFile(path, []byte("777"), 0o644); err != nil {
		t.Fatalf("successor write: %v", err)
	}
	cleanup()
	if got, ok := readPIDFile(path); !ok || got != 777 {
		t.Errorf("second cleanup removed the successor's file (got %v, ok=%v)", got, ok)
	}
}
