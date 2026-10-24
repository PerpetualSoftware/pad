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
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

func TestSessionActivityRealBinaryReturnsBeforeResponse(t *testing.T) {
	const parentExitBound = time.Second
	if parentExitBound >= cli.SessionActivityHTTPTimeout {
		t.Fatalf("parent exit bound %v must be below session activity HTTP timeout %v", parentExitBound, cli.SessionActivityHTTPTimeout)
	}
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
	releaseResponse := sync.OnceFunc(func() { close(release) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		arrived <- request{body, r.Header.Clone()}
		<-release
		w.WriteHeader(http.StatusNoContent)
		close(finished)
	}))
	defer srv.Close()
	defer releaseResponse()
	cmd := exec.Command(bin, "--url", srv.URL, "session", "activity", "busy")
	cmd.Dir = t.TempDir()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start parent: %v", err)
	}
	defer cmd.Process.Kill()
	parentExited := make(chan error, 1)
	go func() { parentExited <- cmd.Wait() }()
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
	// The server has received the child's request and still holds its response.
	// Keep this bound below SetSessionActivity's HTTP timeout so waiting
	// for the child in the parent must fail this assertion.
	select {
	case err := <-parentExited:
		if err != nil {
			t.Fatalf("parent: %v, stderr=%q", err, &stderr)
		}
	case <-time.After(parentExitBound):
		t.Fatal("parent did not exit while server held the child request")
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("parent output: %q %q", &stdout, &stderr)
	}
	releaseResponse()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("child report did not complete after releasing the response")
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
