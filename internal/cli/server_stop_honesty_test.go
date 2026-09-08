package cli

import (
	"net"
	"net/http"
	"net/http/httptest"
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
