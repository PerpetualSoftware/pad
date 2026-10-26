package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestSessionActivityParentNeverReports(t *testing.T) {
	preserveRootFlagGlobals(t)
	oldBody := sessionActivityBody
	oldSpawn := sessionActivitySpawn
	t.Cleanup(func() { sessionActivityBody, sessionActivitySpawn = oldBody, oldSpawn })
	for _, tc := range []struct {
		args   []string
		spawns int
	}{
		{[]string{"busy"}, 1}, {[]string{"idle"}, 1}, {[]string{"foo"}, 0}, {nil, 0}, {[]string{"busy", "extra"}, 0},
		{[]string{"busy", "--unknown"}, 0}, {[]string{"busy", "--format"}, 0}, {[]string{"busy", "--url"}, 0},
		{[]string{"busy", "--format", "invalid"}, 1},
	} {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			called := false
			sessionActivityBody = func(context.Context, string, string, string, string) { called = true }
			spawns := 0
			start := time.Now()
			sessionActivitySpawn = func(state, at, agent, worker string) error {
				spawns++
				parsed, err := time.Parse(time.RFC3339Nano, at)
				if state != tc.args[0] || err != nil || !strings.HasSuffix(at, "Z") || parsed.Before(start) || parsed.After(time.Now()) {
					t.Errorf("spawn state=%q at=%q: %v", state, at, err)
				}
				return errors.New("spawn failed") // Never fall back to the body.
			}
			root := newRootCmd()
			root.SetArgs(append([]string{"session", "activity"}, tc.args...))
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			if err := root.Execute(); err != nil {
				t.Fatalf("parent failed: %v", err)
			}
			if called {
				t.Error("parent ran the reporting body")
			}
			if spawns != tc.spawns {
				t.Errorf("spawn count=%d, want %d", spawns, tc.spawns)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("parent output: %q %q", &stdout, &stderr)
			}
		})
	}
}

func TestSessionActivityDetachedTimestamp(t *testing.T) {
	oldBody, oldSpawn := sessionActivityBody, sessionActivitySpawn
	t.Cleanup(func() { sessionActivityBody, sessionActivitySpawn = oldBody, oldSpawn })
	sessionActivitySpawn = func(string, string, string, string) error { t.Error("child tried to spawn"); return nil }
	const timestamp = "2026-10-23T00:00:00.123456789Z"
	for _, at := range []string{timestamp, "", "invalid"} {
		called := false
		sessionActivityBody = func(ctx context.Context, state, got, agent, worker string) {
			called = true
			if state != "busy" || got != timestamp {
				t.Errorf("child body: %q %q", state, got)
			}
			deadline, ok := ctx.Deadline()
			remaining := time.Until(deadline)
			if !ok || remaining < models.SessionActivityReportBound-time.Second || remaining > models.SessionActivityReportBound {
				t.Errorf("child deadline has %v remaining, want report bound %v within 1s", remaining, models.SessionActivityReportBound)
			}
		}
		cmd := sessionActivityCmd()
		if flag := cmd.Flags().Lookup("detached-at"); flag == nil || !flag.Hidden {
			t.Fatal("detached-at must be hidden")
		}
		cmd.SetArgs([]string{"busy", "--detached-at", at})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if called != (at == timestamp) {
			t.Errorf("timestamp %q: child body called=%v", at, called)
		}
	}
}

func TestSessionActivityParentIdentityResolution(t *testing.T) {
	oldResolve, oldSpawn := sessionActivityResolveIdentity, sessionActivitySpawn
	oldTimeout := sessionActivityResolutionTimeout
	t.Cleanup(func() {
		sessionActivityResolveIdentity, sessionActivitySpawn = oldResolve, oldSpawn
		sessionActivityResolutionTimeout = oldTimeout
	})
	sessionActivityResolutionTimeout = 25 * time.Millisecond
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "resolved", true: "deadline"}[blocked], func(t *testing.T) {
			release, done := make(chan struct{}), make(chan struct{})
			defer func() { close(release); <-done }()
			sessionActivityResolveIdentity = func() (string, string) {
				defer close(done)
				if blocked {
					<-release // Deliberately ignores the parent's cancellation.
				}
				return "Captured/Agent", "wk_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			}
			spawns := 0
			sessionActivitySpawn = func(state, at, agent, worker string) error {
				spawns++
				wantAgent, wantWorker := "Captured/Agent", "wk_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				if blocked {
					wantAgent, wantWorker = "", ""
				}
				if agent != wantAgent || worker != wantWorker {
					t.Errorf("spawn identity = %q %q, want %q %q", agent, worker, wantAgent, wantWorker)
				}
				return nil
			}
			cmd := sessionActivityCmd()
			cmd.SetArgs([]string{"busy"})
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			start := time.Now()
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
				t.Errorf("resolution took %s with a 25ms deadline", elapsed)
			}
			if spawns != 1 || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Errorf("spawns=%d stdout=%q stderr=%q", spawns, &stdout, &stderr)
			}
		})
	}
}
