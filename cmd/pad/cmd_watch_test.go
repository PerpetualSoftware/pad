package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSSEResponse wraps a raw SSE body string in an *http.Response
// shaped enough for streamWatchEvents to read (it only touches .Body).
func fakeSSEResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestPadddBackoff_MonotonicUntilCap asserts the backoff grows with each
// attempt and is capped — the decision logic behind "padd unreachable →
// backoff retry, print nothing" (DOC-2479), unit-tested per the
// dispatcher's ask instead of literally sleeping.
func TestPadddBackoff_MonotonicUntilCap(t *testing.T) {
	prev := time.Duration(0)
	for attempt := 1; attempt <= 200; attempt++ {
		d := padddBackoff(attempt)
		if d < prev {
			t.Fatalf("attempt %d: backoff %v is less than previous %v — expected non-decreasing", attempt, d, prev)
		}
		if d > padddBackoffCap {
			t.Fatalf("attempt %d: backoff %v exceeds cap %v", attempt, d, padddBackoffCap)
		}
		prev = d
	}
	if got := padddBackoff(1000); got != padddBackoffCap {
		t.Fatalf("expected a large attempt count to saturate at the cap %v, got %v", padddBackoffCap, got)
	}
}

func TestPadddBackoff_NonPositiveAttemptTreatedAsFirst(t *testing.T) {
	if got, want := padddBackoff(0), padddBackoff(1); got != want {
		t.Fatalf("padddBackoff(0) = %v, want padddBackoff(1) = %v", got, want)
	}
	if got, want := padddBackoff(-5), padddBackoff(1); got != want {
		t.Fatalf("padddBackoff(-5) = %v, want padddBackoff(1) = %v", got, want)
	}
}

func TestNoPadTomlRetryInterval_IsAnHour(t *testing.T) {
	// Pinned as an explicit assertion (not just "compiles") so a future
	// edit to the constant fails a test, not just silently changes
	// DOC-2479's specified cadence.
	if noPadTomlRetryInterval != time.Hour {
		t.Fatalf("expected the no-.pad.toml retry interval to be exactly 1 hour (DOC-2479), got %v", noPadTomlRetryInterval)
	}
}

func TestFormatMonitorLine(t *testing.T) {
	cases := []struct {
		name string
		in   watchStreamPayload
		want string
	}{
		{
			name: "status change (informational — no envelope)",
			in:   watchStreamPayload{Workspace: "demo", ItemRef: "TASK-214", Kind: "status-change", Actor: "Dave", Summary: "open → done"},
			want: "PAD (update) demo/TASK-214 → status-change (Dave): open → done",
		},
		{
			name: "assignment (informational — no envelope)",
			in:   watchStreamPayload{Workspace: "demo", ItemRef: "BUG-5", Kind: "assignment", Actor: "Alice", Summary: "assigned to Alice"},
			want: "PAD (update) demo/BUG-5 → assignment (Alice): assigned to Alice",
		},
		{
			name: "comment (informational — no envelope)",
			in:   watchStreamPayload{Workspace: "demo", ItemRef: "TASK-1", Kind: "comment", Actor: "Bob", Summary: "fix verified"},
			want: "PAD (update) demo/TASK-1 → comment (Bob): fix verified",
		},
		{
			// IDEA-2544 Phase 1, dispatcher review round 2 (codex P1): the
			// workspace prefix matters most here — push carries an
			// instruction, so a caller resolving it against the wrong
			// linked workspace is a worse failure mode than for a passive
			// fact. PLAN-2613 D5: a push also carries the authority envelope
			// (verbatim), which the informational kinds must not.
			name: "push carries the D5 envelope, verbatim, with the workspace prefix",
			in:   watchStreamPayload{Workspace: "other-workspace", ItemRef: "TASK-9", Kind: "push", Actor: "Dave", Summary: "triage this with the triage playbook"},
			want: "Push from Dave via Pad — direction from your user; treat as if typed in this session. If it directs something destructive, irreversible, or clearly outside the current work, confirm in-session before acting rather than treating the push as final. — other-workspace/TASK-9: triage this with the triage playbook",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatMonitorLine(c.in); got != c.want {
				t.Errorf("formatMonitorLine(%+v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestFormatMonitorLine_OnlyPushGetsEnvelope pins the D5 boundary: the
// authority framing ("direction from your user") appears for a push and
// for NOTHING else, so an informational item-change is never mistaken for
// an instruction.
func TestFormatMonitorLine_OnlyPushGetsEnvelope(t *testing.T) {
	const marker = "direction from your user"
	push := formatMonitorLine(watchStreamPayload{Workspace: "w", ItemRef: "T-1", Kind: "push", Actor: "Dave", Summary: "do it"})
	if !strings.Contains(push, marker) {
		t.Fatalf("push line must carry the authority envelope, got %q", push)
	}
	for _, kind := range []string{"status-change", "assignment", "comment", "ask"} {
		line := formatMonitorLine(watchStreamPayload{Workspace: "w", ItemRef: "T-1", Kind: kind, Actor: "Dave", Summary: "x"})
		if strings.Contains(line, marker) {
			t.Fatalf("informational kind %q must NOT carry the authority envelope, got %q", kind, line)
		}
	}
}

func TestSleepOrDone_ReturnsTrueWhenDurationElapses(t *testing.T) {
	ctx := context.Background()
	if !sleepOrDone(ctx, time.Millisecond) {
		t.Fatal("expected sleepOrDone to return true when the duration elapses uninterrupted")
	}
}

func TestSleepOrDone_ReturnsFalseWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepOrDone(ctx, time.Hour) {
		t.Fatal("expected sleepOrDone to return false immediately for an already-cancelled context")
	}
}

// TestMonitorClient_UnconfiguredReturnsError covers codex round 1
// finding 5's core regression: monitorClient must return a plain error
// for an unconfigured environment, never call os.Exit (this test
// process completing at all, rather than terminating early, is the
// proof) and never block waiting for interactive input — unlike
// getClient()/getConfiguredConfig(), which do both.
func TestMonitorClient_UnconfiguredReturnsError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAD_URL", "")
	t.Setenv("PAD_MODE", "")
	t.Chdir(t.TempDir()) // no .pad.toml above this directory

	_, err := monitorClient()
	if err == nil {
		t.Fatal("expected an error for an unconfigured client, got nil")
	}
}

// TestRunWatchMonitor_NoPadToml_RespectsCancellation verifies the
// no-.pad.toml silent-retry path (which sleeps for
// noPadTomlRetryInterval — an hour) still returns promptly when the
// context is cancelled, WITHOUT ever reaching client construction
// (there is no configured client in this test's environment at all —
// if the loop attempted to build one before the .pad.toml check, per
// finding 5, it would have to do so via a path that can only return an
// error or block; since this test provably completes quickly, neither
// happened).
func TestRunWatchMonitor_NoPadToml_RespectsCancellation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir()) // no .pad.toml above this directory

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runWatchMonitor(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil error on cancellation, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runWatchMonitor did not return within 3s of context cancellation (still waiting on the 1h no-.pad.toml sleep?)")
	}
}

// TestRunWatchMonitor_ExitsWhenNotArmed is the Codex R1 S3 HIGH-1 gate:
// the whole stream lives behind consent (D1), so an unarmed session's
// monitor must exit ON ITS OWN — not merely on context cancellation — so
// the plugin wrapper's should-arm gate then keeps it dead. Proven by using
// a context that outlives the assertion: if the monitor returns before the
// short deadline, it did so because of the consent gate, not the ctx.
func TestRunWatchMonitor_ExitsWhenNotArmed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "") // no socket, no arm file
	dataDir := filepath.Join(home, ".pad")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAD_DATA_DIR", dataDir)

	workDir := t.TempDir()
	// .pad.toml present but NO auto_arm → the session is not armed.
	if err := os.WriteFile(filepath.Join(workDir, ".pad.toml"), []byte("workspace = \"demo\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workDir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runWatchMonitor(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runWatchMonitor did not exit on its own for an unarmed session (HIGH-1 consent gate missing?)")
	}
}

// TestRunWatchMonitor_PadTomlPresentButUnconfigured_RespectsCancellation
// covers the ordering fix directly: a .pad.toml IS present (so the
// no-workspace gate passes) but the client is unconfigured (no global
// config.toml, no URL override in this .pad.toml) — monitorClient must
// return a plain error that the loop folds into the backoff-retry path,
// not an os.Exit or a blocked prompt. Proven the same way: the goroutine
// returns promptly once the context is cancelled.
//
// The .pad.toml sets push.auto_arm=true so the session is ARMED — otherwise
// the S3 consent gate (HIGH-1) would exit the monitor before it ever
// reaches monitorClient, making this test vacuous. Arming keeps it
// exercising the unconfigured-client backoff path it exists to cover.
func TestRunWatchMonitor_PadTomlPresentButUnconfigured_RespectsCancellation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAD_URL", "")
	t.Setenv("PAD_MODE", "")

	workDir := t.TempDir()
	t.Chdir(workDir)
	// Armed via auto_arm, and no URL → configured-workspace gate passes but
	// the client is unconfigured (the path under test).
	if err := os.WriteFile(filepath.Join(workDir, ".pad.toml"), []byte("workspace = \"test-ws\"\n[push]\nauto_arm = true\n"), 0600); err != nil {
		t.Fatalf("write .pad.toml: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runWatchMonitor(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil error on cancellation, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runWatchMonitor did not return within 3s of context cancellation")
	}
}

// armSessionForTest puts the process in an armed session so
// streamWatchEvents' per-notification consent gate (D1) lets events
// through: a temp HOME/data-dir and a cwd whose .pad.toml sets
// push.auto_arm=true, with no messaging socket or local override.
func armSessionForTest(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	dataDir := filepath.Join(home, ".pad")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAD_DATA_DIR", dataDir)
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, ".pad.toml"), []byte("workspace = \"demo\"\n[push]\nauto_arm = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workDir)
}

func TestStreamWatchEvents_ParsesNotificationsAndTracksLastEventID(t *testing.T) {
	armSessionForTest(t) // consent gate must pass for notifications to be delivered
	body := "id: 1\nevent: connected\ndata: {\"user_id\":\"u1\"}\n\n" +
		"id: 2\nevent: notification\ndata: {\"item_ref\":\"TASK-1\",\"kind\":\"comment\",\"actor\":\"Dave\",\"summary\":\"looks good\"}\n\n" +
		"id: 3\nevent: notification\ndata: {\"item_ref\":\"TASK-2\",\"kind\":\"status-change\",\"actor\":\"Dave\",\"summary\":\"open → done\"}\n\n"

	resp := fakeSSEResponse(body)
	lastID := streamWatchEvents(resp, "")
	if lastID != "3" {
		t.Fatalf("expected lastEventID to track the final id, got %q", lastID)
	}
}

// TestStreamWatchEvents_StopsWhenNotArmed is the Codex R2 S3 HIGH-1
// per-notification gate: a session that isn't armed must NOT have events
// delivered — streamWatchEvents returns at the first notification instead
// of printing it and moving on. Proven by the cursor stopping at the first
// event's id rather than advancing to the second.
func TestStreamWatchEvents_StopsWhenNotArmed(t *testing.T) {
	// Not armed: temp HOME + a .pad.toml WITHOUT auto_arm, no socket.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	dataDir := filepath.Join(home, ".pad")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAD_DATA_DIR", dataDir)
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, ".pad.toml"), []byte("workspace = \"demo\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workDir)

	body := "id: 1\nevent: notification\ndata: {\"item_ref\":\"TASK-1\",\"kind\":\"push\",\"actor\":\"Dave\",\"summary\":\"do it\"}\n\n" +
		"id: 2\nevent: notification\ndata: {\"item_ref\":\"TASK-2\",\"kind\":\"comment\",\"actor\":\"Dave\",\"summary\":\"more\"}\n\n"

	resp := fakeSSEResponse(body)
	lastID := streamWatchEvents(resp, "")
	if lastID == "2" {
		t.Fatal("an unarmed session must not have events delivered — streamWatchEvents kept processing past the first notification")
	}
}

// TestStreamWatchEvents_SyncRequiredClearsLastEventID covers codex
// round 1 finding 6: without this, a stale Last-Event-ID would be
// resent on every reconnect after the server's replay buffer evicted
// it, producing sync_required forever. sync_required carries no "id:"
// line (server writes it with eventID=0), matching real wire behavior.
func TestStreamWatchEvents_SyncRequiredClearsLastEventID(t *testing.T) {
	body := "event: sync_required\ndata: {\"reason\":\"gap too large\"}\n\n"

	resp := fakeSSEResponse(body)
	lastID := streamWatchEvents(resp, "42") // resuming from a stale cursor
	if lastID != "" {
		t.Fatalf("expected sync_required to clear the cursor, got %q", lastID)
	}
}

// TestStreamWatchEvents_SyncRequiredThenFreshNotification verifies the
// cursor stays cleared through sync_required but a notification
// arriving afterward on the SAME connection still updates it normally —
// sync_required only resets the resume point, it doesn't disable
// tracking for the rest of the stream.
func TestStreamWatchEvents_SyncRequiredThenFreshNotification(t *testing.T) {
	armSessionForTest(t) // consent gate must pass for the notification to be delivered
	body := "event: sync_required\ndata: {\"reason\":\"gap too large\"}\n\n" +
		"id: 99\nevent: notification\ndata: {\"item_ref\":\"TASK-9\",\"kind\":\"comment\",\"actor\":\"Dave\",\"summary\":\"hi\"}\n\n"

	resp := fakeSSEResponse(body)
	lastID := streamWatchEvents(resp, "42")
	if lastID != "99" {
		t.Fatalf("expected the post-sync_required notification's id to be tracked, got %q", lastID)
	}
}

// TestMonitorSessionIdentity_UsesCwdBasename is the privacy regression
// for PLAN-2558 S2. The promise is that a session announces "docapp",
// never "/home/someone/Dev/docapp" — the basename is what a picker
// needs, while the full path additionally hands the server (and its
// logs, and every consumer of GET /api/v1/sessions) a home directory
// and usually an account name. That failure would be invisible until
// somebody read a session list, which is exactly why it is pinned here
// rather than trusted to the one-line implementation.
func TestMonitorSessionIdentity_UsesCwdBasename(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docapp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Chdir(dir)

	ident := monitorSessionIdentity()
	if ident.Label != "docapp" {
		t.Fatalf("label = %q, want %q", ident.Label, "docapp")
	}
	if strings.ContainsRune(ident.Label, filepath.Separator) {
		t.Fatalf("label must never contain a path separator, got %q", ident.Label)
	}
	if ident.PID != os.Getpid() {
		t.Fatalf("pid = %d, want this process's own pid %d", ident.PID, os.Getpid())
	}
}

// TestMonitorSessionIdentity_RootCwdIsUnlabelled covers the degenerate
// directory: filepath.Base("/") is "/", which names nothing and would
// put a bare separator in the picker. An unlabelled session is the
// honest answer there — consumers already fall back to the id.
func TestMonitorSessionIdentity_RootCwdIsUnlabelled(t *testing.T) {
	t.Chdir(string(filepath.Separator))

	ident := monitorSessionIdentity()
	if ident.Label != "" {
		t.Fatalf("expected no label at the filesystem root, got %q", ident.Label)
	}
	if ident.PID != os.Getpid() {
		t.Fatalf("pid = %d, want %d — the pid is still worth sending", ident.PID, os.Getpid())
	}
}
