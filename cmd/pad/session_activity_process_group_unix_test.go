//go:build unix

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSessionActivitySurvivesParentProcessGroupKill(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pad")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// Build before isolating HOME so the build uses the run's Go cache.
	sessionActivityRegistry(t)
	t.Setenv("PAD_WORKER_ID", "")
	arrived := make(chan context.Context, 1)
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
		arrived <- r.Context()
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
	var requestContext context.Context
	select {
	case requestContext = <-arrived:
	case err := <-completed:
		t.Fatalf("receive child request: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("background request never arrived")
	}
	select {
	case err := <-parentExited:
		if err != nil {
			t.Fatalf("parent: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("parent did not exit while server held the child request")
	}
	// Setpgid with Pgid zero makes the parent's pid its process group id.
	// ESRCH is expected when Setsid has already moved the child out of it.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		t.Fatalf("kill parent process group: %v", err)
	}
	select {
	case <-requestContext.Done():
		t.Fatal("child request canceled by parent process-group kill")
	case <-time.After(100 * time.Millisecond):
		// Let signal delivery and a disconnected socket reach the server before
		// releasing the response; otherwise a handler return could hide a death.
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
