package cli

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// EnsureServer checks if the pad server is running; if not, starts it in the background.
func EnsureServer(cfg *config.Config) error {
	// Only an explicitly configured local client should auto-manage a local
	// background process. Unconfigured or external clients should connect only
	// to their configured target.
	if !cfg.ManagesLocalServer() {
		return nil
	}

	if isServerHealthy(cfg.Host, cfg.Port) {
		return nil
	}

	// Start server as background process
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}

	cmd := exec.Command(exePath, "server", "start")
	setSysProcAttr(cmd)

	// Redirect stdout/stderr to log file
	logFile, err := os.OpenFile(cfg.LogFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start server: %w", err)
	}

	// The PID file is deliberately NOT written here (BUG-2965, codex round 3).
	// The child claims it AFTER it binds the port, so the file always names the
	// process that actually owns the address. Writing it from the parent, at
	// spawn time, records a process that may never bind — and when the port is
	// held by an existing server that is briefly unhealthy, that write
	// overwrites the running server's own entry with a pid that is about to
	// exit, leaving a healthy server unaddressable by `pad server stop`. The
	// health wait below is what tells us the child got there.

	// Release the process so it doesn't become a zombie
	cmd.Process.Release()
	logFile.Close()

	// Wait for server to become healthy
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if isServerHealthy(cfg.Host, cfg.Port) {
			return nil
		}
	}

	return fmt.Errorf("server failed to start within 3 seconds. Check %s for errors", cfg.LogFile())
}

// StopServer stops the server this CLI's config points at — or explains why it
// will not, which is the harder half.
//
// The PID file is a HANDLE, not evidence (BUG-2965), and the pid inside it is
// not proof of identity (BUG-2969). Measured before this changed: a `sleep 600`
// whose pid had been written into the file was SIGTERMed, and stop printed
// "Server stopped." — os.FindProcess succeeds for any pid on Unix, nothing
// asked whether the pid was ours, and the confirmation loop polled the PORT,
// which is unhealthy from the first poll when nothing was ever serving. The
// success check was satisfied by the failure case.
//
// So the order is: read the record, ask the PLATFORM whether a live pad server
// owns it (an advisory lock on Unix, the process creation time on Windows),
// and signal only on a yes. Stale and unprovable both mean signal nothing.
func StopServer(cfg *config.Config) error {
	path := cfg.PIDFile()

	// One read, under the ownership probe: pidFileOwner returns the record it
	// verified, so the pid signalled below is the pid whose ownership was
	// established. An earlier separate read left a window for a successor to
	// claim the file between the two (codex round 1).
	rec, owner := pidFileOwner(path)
	if owner == pidFileStale && rec.PID == 0 {
		// No file at all. A server started outside this CLI still holds the
		// port, and saying "not running" about it is the BUG-2965 failure.
		if isServerHealthy(cfg.Host, cfg.Port) {
			return fmt.Errorf(
				"a server is answering on %s but this CLI did not start it (no PID file at %s) — "+
					"stop it where it was started, or signal the process listening on that address",
				cfg.Addr(), path)
		}
		return fmt.Errorf("server not running (no PID file)")
	}

	switch owner {
	case pidFileStale:
		// The writer is gone. The pid may since have been reused by something
		// unrelated, so it does not get signalled on the strength of a file
		// the dead process left behind.
		// The file is already gone: pidFileOwner removed it while holding the
		// lock, which is the only point at which no replacement can have
		// claimed the path (codex round 2).
		if isServerHealthy(cfg.Host, cfg.Port) {
			// Deliberately NOT "the process is gone": the recorded pid may
			// well be alive — that is the dangerous case, since it is a
			// stranger that inherited the number — and a reader who checks
			// will find it running. What is true is that it does not own this
			// file, so it is not the server answering on that address.
			//
			// The message does not claim the file was removed: Unix drops it
			// under the ownership lock, Windows leaves it for the next start
			// to overwrite (see pidfile_windows.go), and a message that
			// asserted a cleanup the platform did not do would be the same
			// class of lie this whole item is about.
			return fmt.Errorf(
				"a server is answering on %s, but the PID file does not belong to it (%s) — "+
					"nothing was signalled; stop that server where it was started",
				cfg.Addr(), describePIDRecord(rec))
		}
		return fmt.Errorf("server not running (stale PID file: it named %s, which does not own it)", describePIDRecord(rec))

	case pidFileUnprovable:
		return fmt.Errorf(
			"cannot confirm that %s belongs to this pad server, so it was not signalled — "+
				"stop it where it was started, or remove %s if you know the process is gone",
			describePIDRecord(rec), path)

	case pidFileOurs:
		// Fall through to the stop.
	default:
		return fmt.Errorf("unrecognised PID file ownership state %q", owner)
	}

	process, err := os.FindProcess(rec.PID)
	if err != nil {
		return fmt.Errorf("process not found")
	}
	if err := stopProcess(process); err != nil {
		return fmt.Errorf("failed to stop server: %w", err)
	}

	// Wait for OUR PROCESS to exit — not for the port to go quiet. A port that
	// nothing was serving is quiet immediately, which is how the old loop
	// confirmed a kill that never touched a pad server.
	for i := 0; i < 60; i++ {
		if processIsGone(rec.PID) {
			// Deliberately NOT removing the file here (codex round 1): the
			// server removes its own on the way down, and by the time we
			// observe its exit a successor may already have bound the port and
			// claimed the path. Deleting then would unaddress the live server.
			// A file left behind by a crash is handled where it belongs — the
			// next stop reads it as stale and removes it there.
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Still there after six seconds. The file stays: the process it names is
	// alive, so the record is still true and still the handle for a retry.
	return fmt.Errorf("signalled %s but it has not exited after 6s — it may be draining connections", describePIDRecord(rec))
}

// IsServerRunning checks if the server is currently running.
func IsServerRunning(cfg *config.Config) bool {
	return isServerHealthy(cfg.Host, cfg.Port)
}

func isServerHealthy(host string, port int) bool {
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d/api/v1/health", host, port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ClaimPIDFile records this process in the PID file and holds the platform's
// ownership marker for as long as it runs. The returned release drops that
// marker and removes the file.
//
// CALL IT ONLY AFTER BINDING THE PORT. That ordering is the whole ownership
// story and it took two codex rounds to arrive at (BUG-2965): whoever holds the
// address is the process `stop` must signal, so the claim follows the bind
// rather than racing it. A write before the bind lets a start that LOSES the
// race record itself and then, on its way out, remove the file the winner
// relies on.
//
// On Unix the marker is an advisory lock held on the file itself, which is what
// lets `stop` distinguish this server from a stranger that inherited the pid
// (BUG-2969). On Windows there is no such lock, and the recorded creation time
// is the discriminator instead.
//
// Failure is NOT fatal: the server is what the caller asked for, and a missing
// or unlocked PID file degrades `stop` to its port-probe message rather than
// breaking anything. The returned release is always safe to call, and safe to
// call twice.
func ClaimPIDFile(path string) func() {
	release, err := holdPIDFile(path)
	if err != nil {
		slog.Warn("could not claim the PID file; `pad server stop` will not be able to address this process",
			"path", path, "error", err)
		return func() {}
	}

	rec := currentPIDRecord()
	if err := writePIDRecord(path, rec); err != nil {
		slog.Warn("could not write PID file; `pad server stop` will not be able to address this process",
			"path", path, "error", err)
		release()
		return func() {}
	}

	return func() {
		// Remove BEFORE dropping the lock: while we still hold it, no other
		// server can have claimed this path, so there is nothing of anyone
		// else's to delete. Dropping first would reopen the read-then-remove
		// race that codex round 4 found in the previous shape.
		if current, ok := readPIDRecord(path); ok && current.PID == rec.PID {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				slog.Warn("could not remove PID file on shutdown", "path", path, "error", err)
			}
		}
		release()
	}
}
