//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// BUG-2965 and BUG-2969 together: the PID file must let `pad server stop`
// address the running server, and must not let it signal anything else.
//
// These are the Unix half — ownership here is an advisory lock, so the tests
// exercise flock behaviour directly. The Windows half (creation-time
// comparison) is covered by the CI smoke on windows-latest, which runs
// `pad server stop` against a server it started.

func TestClaimPIDFile_WritesARecordAndReleasesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.pid")

	release := ClaimPIDFile(path)

	rec, ok := readPIDRecord(path)
	if !ok {
		t.Fatal("no PID record written")
	}
	if rec.PID != os.Getpid() {
		t.Errorf("record names pid %d, want this process (%d)", rec.PID, os.Getpid())
	}
	if rec.StartedAt.IsZero() {
		t.Error("record has no start time — the human-readable half is what a person reads to identify the process")
	}
	if rec.Exe == "" {
		t.Error("record has no executable path")
	}

	release()
	if _, ok := readPIDRecord(path); ok {
		t.Error("PID file survived release")
	}
}

// TestClaimPIDFile_HoldsTheLockWhileRunning is the ownership claim itself: while
// a server is up, the file reports as OURS, which is the only state in which
// stop is allowed to signal.
func TestClaimPIDFile_HoldsTheLockWhileRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.pid")
	release := ClaimPIDFile(path)
	defer release()

	if _, ok := readPIDRecord(path); !ok {
		t.Fatal("no PID record written")
	}
	if _, got := pidFileOwner(path); got != pidFileOurs {
		t.Errorf("pidFileOwner = %v while the claim is held, want ours", got)
	}
}

// TestPIDFileOwner_UnheldFileIsStale is the defect's core, expressed as a
// property: a record nobody holds is stale NO MATTER WHAT PID IT NAMES. The pid
// here is this live test process — the strongest form of the case, because a
// liveness check would call it ours and signal it.
func TestPIDFileOwner_UnheldFileIsStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.pid")
	rec := pidRecord{PID: os.Getpid()}
	if err := writePIDRecord(path, rec); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, got := pidFileOwner(path); got != pidFileStale {
		t.Errorf("pidFileOwner = %v for an unheld file naming a LIVE pid, want stale — "+
			"liveness is not ownership, and this is the pid a stranger would hold after reuse", got)
	}
}

func TestPIDFileOwner_MissingFileIsStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.pid")
	if _, got := pidFileOwner(path); got != pidFileStale {
		t.Errorf("pidFileOwner = %v for a missing file, want stale", got)
	}
}

func TestReadPIDRecord_AcceptsTheLegacyBarePID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(4242)+"\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec, ok := readPIDRecord(path)
	if !ok {
		t.Fatal("legacy bare-pid file did not parse — a file written by an older binary is exactly the case this family of bugs is about")
	}
	if rec.PID != 4242 {
		t.Errorf("pid = %d, want 4242", rec.PID)
	}
	if !rec.StartedAt.IsZero() {
		t.Error("a legacy record must carry no fingerprint, so ownership reads as unprovable rather than proven")
	}
}

func TestReadPIDRecord_RejectsGarbage(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         "",
		"whitespace":    "   \n",
		"not a number":  "not-a-pid",
		"zero":          "0",
		"negative":      "-1",
		"broken json":   `{"pid": `,
		"json zero pid": `{"pid": 0}`,
	} {
		path := filepath.Join(t.TempDir(), "pad.pid")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("%s: seed: %v", name, err)
		}
		if _, ok := readPIDRecord(path); ok {
			t.Errorf("%s: parsed as a valid record", name)
		}
	}
}

// TestClaimPIDFile_ReleaseIsIdempotent covers the shutdown path, which calls
// release explicitly before the listener closes AND keeps it deferred as a
// backstop.
func TestClaimPIDFile_ReleaseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.pid")
	release := ClaimPIDFile(path)
	release()
	release()
	if _, ok := readPIDRecord(path); ok {
		t.Error("PID file present after two releases")
	}
}

// TestProcessIsGone_LiveAndDead pins the confirmation predicate. The old wait
// loop asked whether the PORT was quiet, which a port nothing ever served
// answers instantly — that is how a kill that never touched a pad server
// reported "Server stopped".
func TestProcessIsGone_LiveAndDead(t *testing.T) {
	if processIsGone(os.Getpid()) {
		t.Error("processIsGone(self) = true")
	}
	if !processIsGone(-1) {
		t.Error("processIsGone(-1) = false")
	}
}

// TestPIDFileOwner_ReportsTheRecordItVerified is codex round 1's first race,
// as a property rather than a timing test: the record the ownership check
// hands back must be the one it read from the descriptor it probed, so a
// caller cannot signal a pid whose ownership was never established.
//
// A successor claim rewrites the file; the check must then report the
// SUCCESSOR's pid, never a pid a caller read earlier.
func TestPIDFileOwner_ReportsTheRecordItVerified(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.pid")

	// Predecessor's record, unheld.
	if err := writePIDRecord(path, pidRecord{PID: 111}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The successor claims it — same path, its own pid, lock held.
	release := ClaimPIDFile(path)
	defer release()

	rec, owner := pidFileOwner(path)
	if owner != pidFileOurs {
		t.Fatalf("owner = %v, want ours", owner)
	}
	if rec.PID != os.Getpid() {
		t.Errorf("verified record names pid %d, want the successor (%d) — signalling the earlier pid would "+
			"send SIGTERM to whatever now holds it", rec.PID, os.Getpid())
	}
}

// TestPIDFileOwner_HeldButUnreadableIsUnprovable covers the claim caught
// mid-write: the lock says held, the bytes do not parse, and there is no pid we
// can justify signalling.
func TestPIDFileOwner_HeldButUnreadableIsUnprovable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.pid")
	release := ClaimPIDFile(path)
	defer release()

	// Truncate the contents while the lock is still held.
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	if _, owner := pidFileOwner(path); owner != pidFileUnprovable {
		t.Errorf("owner = %v for a held but unparseable file, want unprovable", owner)
	}
}

// TestPIDFileOwner_RemovesTheStaleFileItself pins where cleanup happens. It has
// to be inside the ownership check, while the lock is held: a caller that
// removed the file afterwards would race a replacement server's claim and
// delete ITS record (codex round 2). Asserting the file is gone when the check
// returns is how that placement stays put.
func TestPIDFileOwner_RemovesTheStaleFileItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.pid")
	if err := writePIDRecord(path, pidRecord{PID: os.Getpid()}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, owner := pidFileOwner(path); owner != pidFileStale {
		t.Fatalf("owner = %v, want stale", owner)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale PID file survived the ownership check (stat err = %v) — cleanup must happen while the "+
			"check holds the lock, not afterwards", err)
	}
}

// TestClaimPIDFile_RetriesPastABriefProbe covers the interaction the round-2
// fix created: `stop`'s ownership probe holds the lock for an instant, and a
// server claiming in that instant must not lose its claim for the rest of its
// life. The probe here is a real held lock released while the claim is trying.
func TestClaimPIDFile_RetriesPastABriefProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.pid")

	probe, err := holdPIDFile(path)
	if err != nil {
		t.Fatalf("probe hold: %v", err)
	}
	done := make(chan struct{})
	go func() {
		time.Sleep(60 * time.Millisecond)
		probe()
		close(done)
	}()

	release := ClaimPIDFile(path)
	defer release()
	<-done

	rec, ok := readPIDRecord(path)
	if !ok || rec.PID != os.Getpid() {
		t.Errorf("claim did not survive a brief foreign hold (record %+v, ok=%v)", rec, ok)
	}
}
