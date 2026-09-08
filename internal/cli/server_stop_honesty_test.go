package cli

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// BUG-2965. StopServer read the PID file and, on any read error, answered
// "server not running (no PID file)" — without asking whether anything was
// listening. The file is written by the paths that START a server from this
// CLI, so a server started any other way (a service unit, a direct `pad server
// start` from before this fix, a supervisor relaunch) held the port with no
// file to find, and `stop` told the caller it was not running.
//
// That is the failure that matters: the caller believes they stopped something
// and acts on the belief. The refresh recipe in CONVE-2687 works around it by
// killing the pid found from the port; a human following the docs does not.

// healthyServerConfig starts a stub answering the health probe and returns a
// config pointed at it, with a PID-file path inside a temp dir (absent unless a
// test writes one).
func healthyServerConfig(t *testing.T) *config.Config {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)

	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split %q: %v", srv.URL, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return &config.Config{Host: host, Port: port, DataDir: t.TempDir()}
}

func TestStopServer_NoPIDFileButServerIsUp_SaysSoAndDoesNotClaimItIsStopped(t *testing.T) {
	cfg := healthyServerConfig(t)

	err := StopServer(cfg)
	if err == nil {
		t.Fatal("StopServer returned nil for a server it cannot address — the caller would believe it stopped")
	}
	msg := err.Error()
	if strings.Contains(msg, "not running") {
		t.Errorf("message says the server is not running while it is answering on %s: %q", cfg.Addr(), msg)
	}
	// The message has to be actionable: name the address, and name the file
	// whose absence is the reason this CLI cannot address the process.
	if !strings.Contains(msg, cfg.Addr()) {
		t.Errorf("message does not name the address the server is answering on: %q", msg)
	}
	if !strings.Contains(msg, cfg.PIDFile()) {
		t.Errorf("message does not name the missing PID file: %q", msg)
	}
}

// TestStopServer_NoPIDFileAndNothingListening keeps the original answer for the
// case it was always right about. Without this, "never say not running" would
// be satisfiable by deleting the phrase, which would leave `stop` unable to say
// the one true thing it says today.
func TestStopServer_NoPIDFileAndNothingListening(t *testing.T) {
	// A port nothing is listening on: bind one, read it, release it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	if cerr := ln.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}

	cfg := &config.Config{Host: "127.0.0.1", Port: port, DataDir: t.TempDir()}

	err = StopServer(cfg)
	if err == nil {
		t.Fatal("expected an error when there is no PID file and nothing is listening")
	}
	if !strings.Contains(err.Error(), "not running") {
		t.Errorf("message = %q, want it to say the server is not running", err.Error())
	}
}

// TestStopServer_PIDFilePathIsUnchanged guards the join the two halves of this
// fix share: `pad server start` writes cfg.PIDFile() and StopServer reads it.
// They are the same call, so this is cheap insurance against a future change
// moving one and not the other.
func TestStopServer_PIDFilePathIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir}
	if got, want := cfg.PIDFile(), filepath.Join(dir, "pad.pid"); got != want {
		t.Errorf("PIDFile() = %q, want %q", got, want)
	}
}

// BUG-2969: stop must signal only a pid it can prove is our server.
//
// Measured before the fix, on the merged binary: a `sleep 600` whose pid had
// been written into the PID file was SIGTERMed and stop printed "Server
// stopped." These pin both halves of the refusal — the stale record naming a
// LIVE process, and the message that must not claim success.

func TestStopServer_StalePIDFileNamingALiveStrangerIsNotSignalled(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Host: "127.0.0.1", Port: unusedPort(t), DataDir: dir}

	// The record names THIS process — alive, and emphatically not a pad server.
	// Nothing holds the file, so ownership is stale and the pid must not be
	// signalled. If it were, this test process would receive SIGTERM.
	if err := writePIDRecord(cfg.PIDFile(), pidRecord{PID: os.Getpid()}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := StopServer(cfg)
	if err == nil {
		t.Fatal("StopServer reported success for a stale record — that is the measured defect: a stranger killed, success printed")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("message = %q, want it to name the record as stale", err.Error())
	}
	if _, statErr := os.Stat(cfg.PIDFile()); !os.IsNotExist(statErr) {
		t.Errorf("stale PID file survived (stat err = %v) — it should be removed once it is known to name nothing", statErr)
	}
}

// TestStopServer_HeldPIDFileIsAddressable is the counterfactual: without it,
// "never signal" would be satisfiable by never signalling anything, which would
// break stop entirely. A held claim must read as ours.
func TestStopServer_HeldPIDFileIsAddressable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pad.pid")

	release := ClaimPIDFile(path)
	defer release()

	if _, ok := readPIDRecord(path); !ok {
		t.Fatal("claim wrote no record")
	}
	if _, got := pidFileOwner(path); got != pidFileOurs {
		t.Fatalf("a held claim reads as %v, want ours — stop would refuse to address a live server", got)
	}
}

// TestStopServer_LegacyRecordIsNotSignalled covers the file an older binary
// left behind: it carries a pid and no proof. Unprovable is not a licence to
// send SIGTERM, so stop refuses and says how to proceed.
func TestStopServer_LegacyRecordIsNotSignalled(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Host: "127.0.0.1", Port: unusedPort(t), DataDir: dir}

	// A legacy bare-pid file naming this live process. On Unix the lock is the
	// discriminator, so an unheld file is stale regardless of the pid; the
	// refusal is what matters, and its wording tells the reader which case
	// they are in.
	if err := os.WriteFile(cfg.PIDFile(), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := StopServer(cfg)
	if err == nil {
		t.Fatal("StopServer reported success for a legacy record naming a non-pad process")
	}
	for _, want := range []string{strconv.Itoa(os.Getpid())} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message = %q, want it to name the pid %s it refused to signal", err.Error(), want)
		}
	}
}

// unusedPort returns a port nothing is listening on, so the health probe in
// StopServer resolves to "nothing there" rather than finding an unrelated
// server — the mistake my own first live check made, where the probe answered
// about this box's dev server on 7777.
func unusedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return port
}
