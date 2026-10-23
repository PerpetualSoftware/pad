//go:build unix

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

// requirePSEnv makes missing ps fail rather than skip in the Linux Go CI job.
const requirePSEnv = "PAD_TEST_REQUIRE_PS"

func skipProcessGroupTest(t *testing.T, why string) {
	t.Helper()
	if os.Getenv(requirePSEnv) != "" {
		t.Fatalf("%s is set, but this test would skip: %s", requirePSEnv, why)
	}
	t.Skip(why)
}

func TestSessionActivitySurvivesParentProcessGroupKill(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		skipProcessGroupTest(t, "ps unavailable on PATH (for example, in the Nix build sandbox); the Linux Go CI job has ps and runs this test: "+err.Error())
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
	arrived := make(chan time.Time, 1)
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
		arrived <- time.Now()
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
	var arrivedAt time.Time
	select {
	case arrivedAt = <-arrived:
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
	parentExitElapsed := time.Since(arrivedAt)
	// The unique server URL identifies this test's child. The lookup uses
	// ps -A -o pid=,args= (procps on Linux). Without ps, this test skips in
	// the Nix build sandbox but fails when Linux Go CI sets PAD_TEST_REQUIRE_PS.
	// Capture its pid before signaling so a dead child cannot disappear from
	// the process list and accidentally satisfy the survival assertion.
	// Leave time to release the response before the child's HTTP timeout.
	latestPollEnd := arrivedAt.Add(cli.SessionActivityHTTPTimeout - 500*time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), latestPollEnd)
	defer cancel()
	lookupStart := time.Now()
	childPID, err := sessionActivityChildPID(ctx, srv.URL, cmd.Process.Pid)
	lookupElapsed := time.Since(lookupStart)
	const minimumLiveness = 200 * time.Millisecond
	failLoaded := func(observed time.Duration) {
		t.Helper()
		t.Fatalf("runner too loaded to measure child liveness: parent exit after arrival=%v, child lookup=%v, elapsed since arrival=%v, remaining budget=%v, observed liveness=%v (need at least %v)",
			parentExitElapsed, lookupElapsed, time.Since(arrivedAt), time.Until(latestPollEnd), observed, minimumLiveness)
	}
	if time.Until(latestPollEnd) < minimumLiveness {
		failLoaded(0)
	}
	if err != nil {
		t.Fatalf("find child: %v", err)
	}
	// Setpgid with Pgid zero makes the parent's pid its process group id.
	// cmd.Wait has already reaped the parent. A detached child leaves this
	// group empty, so ESRCH is expected. The signal catches a child left in
	// the parent's group when Setsid is removed.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		t.Fatalf("kill parent process group: %v", err)
	}
	// SIGKILL delivery is asynchronous, and zombies can still answer
	// kill(pid, 0). Require a non-zombie child throughout a sustained window,
	// capped by the HTTP deadline even if parent exit or the lookup was slow.
	deadline := time.Now().Add(min(500*time.Millisecond, time.Until(latestPollEnd)))
	var firstAlive, lastAlive time.Time
	for time.Now().Before(deadline) {
		out, err := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", strconv.Itoa(childPID)).Output()
		now := time.Now()
		if !now.Before(latestPollEnd) {
			failLoaded(lastAlive.Sub(firstAlive))
		}
		state := strings.TrimSpace(string(out))
		if err != nil || state == "" || strings.HasPrefix(state, "Z") {
			t.Fatalf("child pid %d did not stay alive after parent process-group kill: state=%q, ps error=%v", childPID, state, err)
		}
		if firstAlive.IsZero() {
			firstAlive = now
		}
		lastAlive = now
		time.Sleep(min(20*time.Millisecond, time.Until(deadline)))
	}
	if observed := lastAlive.Sub(firstAlive); observed < minimumLiveness {
		failLoaded(observed)
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

func sessionActivityChildPID(ctx context.Context, url string, parentPID int) (int, error) {
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,args=").Output()
	if err != nil {
		return 0, err
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
				return pid, nil
			}
		}
	}
	return 0, fmt.Errorf("no child with --url %s (parent pid %d)", url, parentPID)
}
