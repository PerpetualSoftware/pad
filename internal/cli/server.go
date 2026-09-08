package cli

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
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

// StopServer sends a stop signal to the background server process.
//
// The PID file is the handle, not the evidence (BUG-2965). It is written by the
// paths that START a server from this CLI; a server started any other way — a
// service unit, a direct `pad server start` from before this fix, a relaunch
// under a supervisor — holds the port with no file to find. Reporting "not
// running" for one of those is the failure that matters here: the caller
// believes they stopped something, and the next thing they do is act on that
// belief.
//
// So a missing file asks the port. Only an unhealthy port earns "not running";
// a healthy one earns an answer that says the server is up, that this CLI
// cannot address it, and what to do instead. Deliberately NOT "find the
// listener and kill it": resolving a pid from a port is platform-specific and,
// more to the point, the process holding it may not be ours — a stop command
// that kills by port is a stop command that can kill a stranger's process.
func StopServer(cfg *config.Config) error {
	pidData, err := os.ReadFile(cfg.PIDFile())
	if err != nil {
		if isServerHealthy(cfg.Host, cfg.Port) {
			return fmt.Errorf(
				"a server is answering on %s but this CLI did not start it (no PID file at %s) — "+
					"stop it where it was started, or signal the process listening on that address",
				cfg.Addr(), cfg.PIDFile())
		}
		return fmt.Errorf("server not running (no PID file)")
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		os.Remove(cfg.PIDFile())
		return fmt.Errorf("invalid PID file")
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		os.Remove(cfg.PIDFile())
		return fmt.Errorf("process not found")
	}

	if err := stopProcess(process); err != nil {
		os.Remove(cfg.PIDFile())
		return fmt.Errorf("failed to stop server: %w", err)
	}

	os.Remove(cfg.PIDFile())

	// Wait for it to actually stop
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		if !isServerHealthy(cfg.Host, cfg.Port) {
			return nil
		}
	}

	return nil
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

// WritePIDFile records pid at path and returns the cleanup that removes it.
//
// The PID file is how `pad server stop` addresses a running server, and before
// BUG-2965 exactly one path wrote it: EnsureServer's auto-start branch. A
// server started any other way — a service unit, a human running `pad server
// start`, a refresh recipe relaunching with the killed process's argv — held
// the port with no file to find, and `stop` answered "not running" about it.
//
// CALL IT ONLY AFTER BINDING THE PORT. That ordering is the whole ownership
// story, and it took two codex rounds to arrive at: whoever holds the address
// is the process `stop` must signal, so the claim has to follow the bind rather
// than race it. A write before the bind lets a start that LOSES the race record
// itself and then, on its way out, remove the file the winner is relying on.
//
// Given that ordering, the write is unconditional. An earlier shape refused to
// replace a PID file naming a live process, which is exactly wrong once the
// caller has bound: no live process can be serving this address, so the entry
// is stale or unrelated (pid reuse), and deferring to it would leave the real
// server unaddressable (codex round 3, P2).
//
// Cleanup, by contrast, removes the file only while it still names US — a
// process whose file was taken over must not delete its successor's entry.
//
// Failure to write is NOT fatal: the server is what the caller asked for, and a
// missing PID file degrades `stop` to its port-probe message rather than
// breaking anything. The returned cleanup is always safe to call, so callers
// can defer it unconditionally.
func WritePIDFile(path string, pid int) func() {
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		slog.Warn("could not write PID file; `pad server stop` will not be able to address this process",
			"path", path, "error", err)
		return func() {}
	}

	return func() {
		if current, ok := readPIDFile(path); !ok || current != pid {
			// Someone else owns it now (or it is already gone). Removing it
			// would unaddress a server we did not start.
			return
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("could not remove PID file on shutdown", "path", path, "error", err)
		}
	}
}

// readPIDFile returns the pid recorded at path. ok is false when the file is
// absent or does not parse — both meaning "nothing here owns this path".
func readPIDFile(path string) (pid int, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return pid, true
}
