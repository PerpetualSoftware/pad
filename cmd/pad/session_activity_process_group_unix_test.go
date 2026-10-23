//go:build unix

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

func TestSessionActivitySurvivesParentProcessGroupKill(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps unavailable on PATH (for example, in the Nix build sandbox); the Linux Go CI job has ps and runs this test: " + err.Error())
	}
	const parentExitBound = time.Second
	if parentExitBound >= cli.SessionActivityHTTPTimeout {
		t.Fatalf("parent exit bound %v must be below session activity HTTP timeout %v", parentExitBound, cli.SessionActivityHTTPTimeout)
	}
	bin := filepath.Join(t.TempDir(), "pad")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// Build before isolating HOME so the build uses the run's Go cache.
	sessionActivityRegistry(t)
	t.Setenv("PAD_WORKER_ID", "")
	arrived := make(chan struct{}, 1)
	completed := make(chan error, 1)
	release := make(chan struct{})
	releaseResponse := sync.OnceFunc(func() { close(release) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading to EOF lets net/http monitor a peer disconnect while the
		// handler holds its response. A successful buffered write alone would
		// not prove that the child survived the signal.
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			completed <- fmt.Errorf("read child request: %w", err)
			return
		}
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			completed <- fmt.Errorf("child disconnected before response: %w", r.Context().Err())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		completed <- http.NewResponseController(w).Flush()
	}))
	defer srv.Close()
	defer releaseResponse()
	cmd := exec.Command(bin, "--url", srv.URL, "session", "activity", "busy")
	cmd.Dir = t.TempDir()
	// Model a hook runner that kills the hook's entire process group.
	// This group contains only the parent we start and any undetached child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start parent: %v", err)
	}
	defer cmd.Process.Kill()
	parentExited := make(chan error, 1)
	go func() { parentExited <- cmd.Wait() }()
	select {
	case <-arrived:
	case err := <-completed:
		t.Fatalf("receive child request: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("background request never arrived")
	}
	// Keep this bound below SetSessionActivity's HTTP timeout so a parent
	// waiting for the child cannot pass by waiting for that timeout instead.
	select {
	case err := <-parentExited:
		if err != nil {
			t.Fatalf("parent: %v", err)
		}
	case <-time.After(parentExitBound):
		t.Fatal("parent did not exit while server held the child request")
	}
	// The unique server URL identifies this test's child. The lookup uses
	// ps -A -o pid=,args= (procps on Linux). This test skips without ps,
	// including in the Nix build sandbox; the Linux Go CI job has ps and runs it.
	// Capture its pid before signaling so a dead child cannot disappear from
	// the process list and accidentally satisfy the survival assertion.
	childPID := sessionActivityChildPID(t, srv.URL, cmd.Process.Pid)
	// Setpgid with Pgid zero makes the parent's pid its process group id.
	// ESRCH is expected when Setsid has already moved the child out of it.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		t.Fatalf("kill parent process group: %v", err)
	}
	// SIGKILL delivery is asynchronous, and zombies can still answer
	// kill(pid, 0). Require a non-zombie child throughout a sustained window.
	for deadline := time.Now().Add(500 * time.Millisecond); ; time.Sleep(20 * time.Millisecond) {
		out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(childPID)).Output()
		state := strings.TrimSpace(string(out))
		if err != nil || state == "" || strings.HasPrefix(state, "Z") {
			t.Fatalf("child pid %d did not stay alive after parent process-group kill: state=%q, ps error=%v", childPID, state, err)
		}
		if !time.Now().Before(deadline) {
			break
		}
	}
	releaseResponse()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("child request did not complete: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("child request did not complete after releasing the response")
	}
}

func sessionActivityChildPID(t *testing.T, url string, parentPID int) int {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "pid=,args=").Output()
	if err != nil {
		t.Fatalf("find child: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == parentPID {
			continue
		}
		for i := 1; i+1 < len(fields); i++ {
			if fields[i] == "--url" && fields[i+1] == url {
				return pid
			}
		}
	}
	t.Fatalf("no child with --url %s (parent pid %d)", url, parentPID)
	return 0
}
