package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestSessionActivityTimeoutDefaultsToReportBound(t *testing.T) {
	if sessionActivityTimeout != models.SessionActivityReportBound {
		t.Fatalf("session activity timeout = %v, want report bound %v", sessionActivityTimeout, models.SessionActivityReportBound)
	}
}

func TestSessionActivityResolutionTimeoutDefault(t *testing.T) {
	if sessionActivityResolutionTimeout != 500*time.Millisecond {
		t.Fatalf("identity resolution default = %v, want 500ms", sessionActivityResolutionTimeout)
	}
}

func TestSessionActivityHookProcess(t *testing.T) {
	if os.Getenv("PAD_TEST_ACTIVITY_HOOK") != "1" {
		return
	}
	sessionActivitySpawn = func(string, string, string, string) error { return os.ErrPermission }
	root := newRootCmd()
	for i, arg := range os.Args {
		if arg == "--" {
			root.SetArgs(append([]string{"--url", os.Getenv("PAD_TEST_ACTIVITY_URL")}, os.Args[i+1:]...))
			break
		}
	}
	if root.Execute() != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestSessionActivityUnreachable(t *testing.T) {
	assertSessionActivityHookSilent(t, "session", "activity", "busy", "--detached-at", "2026-10-23T00:00:00Z")
}

func TestSessionActivitySpawnFailureSilent(t *testing.T) {
	assertSessionActivityHookSilent(t, "session", "activity", "busy")
}

func TestSessionActivityResolutionErrorSilent(t *testing.T) {
	// CaptureSessionOwner rejects this malformed harness pid. Neither identity
	// resolver may turn that failure into output or a nonzero hook exit.
	t.Setenv("PAD_SESSION_PID", "invalid-pid")
	assertSessionActivityHookSilent(t, "session", "activity", "busy")
}

func TestSessionActivityInvalidArgs(t *testing.T) {
	for _, args := range [][]string{{"foo"}, {}, {"busy", "extra"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			assertSessionActivityHookSilent(t, append([]string{"session", "activity"}, args...)...)
		})
	}
}

func TestSessionActivityInvalidFormat(t *testing.T) {
	assertSessionActivityHookSilent(t, "--format", "invalid", "session", "activity", "busy")
}

func TestSessionActivityInvalidFlags(t *testing.T) {
	for _, flag := range []string{"--unknown", "--format", "--url"} {
		t.Run(flag, func(t *testing.T) {
			assertSessionActivityHookSilent(t, "session", "activity", "busy", flag)
		})
	}
}

func TestSessionActivityBlockedBodyProcess(t *testing.T) {
	if os.Getenv("PAD_TEST_ACTIVITY_BLOCKED") != "1" {
		return
	}
	sessionActivityTimeout = 25 * time.Millisecond
	sessionActivityBody = func(context.Context, string, string, string, string) { select {} }
	root := newRootCmd()
	root.SetArgs([]string{"session", "activity", "busy", "--detached-at", "2026-10-23T00:00:00Z"})
	if root.Execute() != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestSessionActivityOverallDeadline(t *testing.T) {
	t.Setenv("PAD_TEST_ACTIVITY_BLOCKED", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSessionActivityBlockedBodyProcess$")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("blocked hook must exit 0 within its total deadline: %v; stderr: %s", err, &stderr)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("blocked hook took %v with a 25ms deadline", elapsed)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("blocked hook must be silent: stdout=%q stderr=%q", &stdout, &stderr)
	}
}

func assertSessionActivityHookSilent(t *testing.T, args ...string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_DATA_DIR", t.TempDir())
	t.Setenv("PAD_DB_PATH", "")
	t.Setenv("PAD_WORKER_ID", "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	t.Setenv("PAD_TEST_ACTIVITY_HOOK", "1")
	t.Setenv("PAD_TEST_ACTIVITY_URL", "http://"+addr)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	// A subprocess catches os.Exit, prompts, and writes to real stdout/stderr.
	argv := append([]string{"-test.run=^TestSessionActivityHookProcess$", "--"}, args...)
	cmd := exec.CommandContext(ctx, os.Args[0], argv...)
	cmd.Dir = t.TempDir() // An unlinked directory, as in a fresh hook session.
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook must exit 0: %v; stderr: %s", err, &stderr)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
