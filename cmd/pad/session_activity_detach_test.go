package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

func TestSessionActivityRealBinaryReturnsBeforeResponse(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pad")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// Build before isolating HOME: the nested build uses the run's Go cache.
	record := sessionActivityRegistry(t)
	t.Setenv("PAD_WORKER_ID", "") // The worker must come from the registry.
	t.Setenv("PAD_AGENT", "")
	if err := os.Unsetenv("PAD_AGENT"); err != nil {
		t.Fatal(err)
	}
	type request struct {
		body   map[string]string
		header http.Header
	}
	arrived := make(chan request, 1)
	finished := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		arrived <- request{body, r.Header.Clone()}
		<-release
		w.WriteHeader(http.StatusNoContent)
		close(finished)
	}))
	defer srv.Close()
	defer close(release)
	cmd := exec.Command(bin, "--url", srv.URL, "session", "activity", "busy")
	cmd.Dir = t.TempDir()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("parent: %v, stderr=%q", err, &stderr)
	}
	if elapsed := time.Since(start); elapsed >= 300*time.Millisecond {
		t.Errorf("parent waited %s for blocked child; want <300ms", elapsed)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("parent output: %q %q", &stdout, &stderr)
	}
	select {
	case req := <-arrived:
		if got := req.header.Get("X-Pad-Worker"); got != record.WorkerID {
			t.Errorf("X-Pad-Worker = %q, want registered worker %q", got, record.WorkerID)
		}
		if got := req.header.Get("X-Pad-Agent"); got != record.Agent {
			t.Errorf("X-Pad-Agent = %q, want registered name %q (PAD_AGENT unset)", got, record.Agent)
		}
		body := req.body
		at, err := time.Parse(time.RFC3339Nano, body["at"])
		if body["state"] != "busy" || err != nil || at.Before(start) || at.After(time.Now()) {
			t.Fatalf("request: %v, timestamp error: %v", body, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background request never arrived")
	}
	select {
	case <-finished:
		t.Fatal("child finished before parent returned")
	default:
	}
}

func sessionActivityRegistry(t *testing.T) cli.SessionRegistration {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PAD_DATA_DIR", t.TempDir())
	t.Setenv("PAD_DB_PATH", "")
	t.Setenv("PAD_TOKEN", "activity-local-test")
	t.Setenv("PAD_SESSION_PID", "")
	t.Setenv("CLAUDE_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	record, err := cli.RegisterSession(t.TempDir(), "registered-activity-agent")
	if err != nil {
		t.Fatal(err)
	}
	if record.Liveness != cli.LivenessAlive {
		t.Skip("this platform cannot verify the registered session is alive")
	}
	data, err := os.ReadFile(record.Path)
	if err != nil {
		t.Fatal(err)
	}
	var registration cli.SessionRegistration
	if err := json.Unmarshal(data, &registration); err != nil {
		t.Fatal(err)
	}
	return registration
}
