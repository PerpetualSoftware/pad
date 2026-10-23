package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSessionActivityParentNeverReports(t *testing.T) {
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
			sessionActivityBody = func(context.Context, string, string) { called = true }
			spawns := 0
			start := time.Now()
			sessionActivitySpawn = func(state, at string) error {
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
	sessionActivitySpawn = func(string, string) error { t.Error("child tried to spawn"); return nil }
	const timestamp = "2026-10-23T00:00:00.123456789Z"
	for _, at := range []string{timestamp, "", "invalid"} {
		called := false
		sessionActivityBody = func(ctx context.Context, state, got string) {
			called = true
			if state != "busy" || got != timestamp {
				t.Errorf("child body: %q %q", state, got)
			}
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 3*time.Second {
				t.Error("missing background deadline")
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
