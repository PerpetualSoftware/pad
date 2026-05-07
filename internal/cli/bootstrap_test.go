package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// fakeSessionServer returns an httptest.Server whose /api/v1/auth/session
// handler responds with the SessionResponse produced by `gen` (called once
// per request so tests can flip setup_required mid-poll). On any other
// path it 404s — caller is responsible for not hitting other endpoints.
func fakeSessionServer(t *testing.T, gen func() SessionResponse) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/session" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gen())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTestConfig points DataDir at a fresh tmp dir and BrowserURL at the
// given test server. Most tests want both wired up together.
func newTestConfig(t *testing.T, browserURL string) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataDir = t.TempDir()
	// BrowserURL prefers cfg.URL when set, which sidesteps the
	// host-rewriting branch and gives us a stable URL to assert on.
	cfg.URL = browserURL
	return cfg
}

// writeBootstrapToken drops a token file at <dataDir>/.bootstrap-token in
// the same shape EnsureBootstrapToken would produce — token + trailing
// newline, mode 0600 — so readBootstrapToken sees what it expects.
func writeBootstrapToken(t *testing.T, dataDir, token string) {
	t.Helper()
	path := filepath.Join(dataDir, bootstrapTokenFilename)
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		t.Fatalf("write bootstrap token: %v", err)
	}
}

// TestRunBrowserBootstrap_Idempotent confirms the helper is a no-op when
// the server already reports setup_required: false — important for
// `pad init` against a server where setup is already done. Should not
// touch the token file.
func TestRunBrowserBootstrap_Idempotent(t *testing.T) {
	srv := fakeSessionServer(t, func() SessionResponse {
		return SessionResponse{Authenticated: true, SetupRequired: false}
	})
	cfg := newTestConfig(t, srv.URL)
	client := NewClientFromURL(srv.URL)

	// Deliberately do NOT write a token file. If the helper short-circuits
	// correctly on setup_required: false, it should never need to read it.
	if err := RunBrowserBootstrap(context.Background(), client, cfg); err != nil {
		t.Fatalf("RunBrowserBootstrap returned %v on already-bootstrapped server, want nil", err)
	}
}

// TestRunBrowserBootstrap_LogsTokenSuccess covers the happy path:
// setup_required: true with a valid token on disk, server flips to
// setup_required: false on the second poll, helper returns nil.
func TestRunBrowserBootstrap_LogsTokenSuccess(t *testing.T) {
	withFastPolling(t, 10*time.Millisecond, 5*time.Second)

	var calls atomic.Int32
	srv := fakeSessionServer(t, func() SessionResponse {
		// First call (initial CheckSession): setup_required: true.
		// Subsequent polls: setup_required: false on the 2nd poll-tick to
		// keep the test fast.
		n := calls.Add(1)
		if n <= 2 {
			return SessionResponse{SetupRequired: true, SetupMethod: "logs_token"}
		}
		return SessionResponse{Authenticated: false, SetupRequired: false}
	})
	cfg := newTestConfig(t, srv.URL)
	writeBootstrapToken(t, cfg.DataDir, "test-token-abc")

	client := NewClientFromURL(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := RunBrowserBootstrap(ctx, client, cfg)
	if err != nil {
		t.Fatalf("RunBrowserBootstrap: %v", err)
	}
	// Sanity: we should have made at least 2 calls (initial + at least one
	// poll). The exact count depends on tick timing but 2 is the floor.
	if calls.Load() < 2 {
		t.Fatalf("expected at least 2 session calls, got %d", calls.Load())
	}
}

// TestRunBrowserBootstrap_OpenMode covers PAD_BYPASS_SETUP_TOKEN=true:
// no token file required, helper should still print a /setup URL and
// poll to completion.
func TestRunBrowserBootstrap_OpenMode(t *testing.T) {
	withFastPolling(t, 10*time.Millisecond, 5*time.Second)

	var calls atomic.Int32
	srv := fakeSessionServer(t, func() SessionResponse {
		n := calls.Add(1)
		if n <= 2 {
			return SessionResponse{SetupRequired: true, SetupMethod: "open"}
		}
		return SessionResponse{SetupRequired: false}
	})
	cfg := newTestConfig(t, srv.URL)
	// Deliberately no token file — open mode shouldn't need one.

	client := NewClientFromURL(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := RunBrowserBootstrap(ctx, client, cfg); err != nil {
		t.Fatalf("RunBrowserBootstrap (open mode): %v", err)
	}
}

// TestRunBrowserBootstrap_TokenMissing — server says setup_method=logs_token
// but the file isn't on disk. Helper should fail loudly with a path-aware
// error directing the user at --cli-prompt.
func TestRunBrowserBootstrap_TokenMissing(t *testing.T) {
	srv := fakeSessionServer(t, func() SessionResponse {
		return SessionResponse{SetupRequired: true, SetupMethod: "logs_token"}
	})
	cfg := newTestConfig(t, srv.URL)
	// No writeBootstrapToken — file is absent.

	client := NewClientFromURL(srv.URL)
	err := RunBrowserBootstrap(context.Background(), client, cfg)
	if err == nil {
		t.Fatal("RunBrowserBootstrap returned nil with missing token, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--cli-prompt") {
		t.Errorf("error %q should mention --cli-prompt fallback", msg)
	}
	if !strings.Contains(msg, bootstrapTokenFilename) {
		t.Errorf("error %q should mention the token filename %q for operator guidance", msg, bootstrapTokenFilename)
	}
}

// TestRunBrowserBootstrap_TokenEmpty — file exists but is empty (0 bytes
// or only whitespace). EnsureBootstrapToken would never produce this, but
// a corrupted file or partial write deserves a clear error rather than
// silently building a `#token=` URL with an empty token.
func TestRunBrowserBootstrap_TokenEmpty(t *testing.T) {
	srv := fakeSessionServer(t, func() SessionResponse {
		return SessionResponse{SetupRequired: true, SetupMethod: "logs_token"}
	})
	cfg := newTestConfig(t, srv.URL)
	writeBootstrapToken(t, cfg.DataDir, "   ")

	client := NewClientFromURL(srv.URL)
	err := RunBrowserBootstrap(context.Background(), client, cfg)
	if err == nil {
		t.Fatal("RunBrowserBootstrap returned nil with empty token, want error")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error %q should call out empty token", err.Error())
	}
}

// TestRunBrowserBootstrap_LocalCLIMethod — the server failed to provision
// a bootstrap token (read-only DataDir, etc.) and the bypass flag isn't
// set, so setup_method=local_cli. Helper can't drive the browser flow;
// must fail with a --cli-prompt hint.
func TestRunBrowserBootstrap_LocalCLIMethod(t *testing.T) {
	srv := fakeSessionServer(t, func() SessionResponse {
		return SessionResponse{SetupRequired: true, SetupMethod: "local_cli"}
	})
	cfg := newTestConfig(t, srv.URL)

	client := NewClientFromURL(srv.URL)
	err := RunBrowserBootstrap(context.Background(), client, cfg)
	if err == nil {
		t.Fatal("RunBrowserBootstrap returned nil with setup_method=local_cli, want error")
	}
	if !strings.Contains(err.Error(), "--cli-prompt") {
		t.Errorf("error %q should mention --cli-prompt fallback", err.Error())
	}
}

// TestRunBrowserBootstrap_UnknownMethod — newer server speaking a method
// this CLI doesn't know about. Helper should bail with a clear "this CLI
// may be older than the server" error rather than silently mishandling it.
func TestRunBrowserBootstrap_UnknownMethod(t *testing.T) {
	srv := fakeSessionServer(t, func() SessionResponse {
		return SessionResponse{SetupRequired: true, SetupMethod: "future_method"}
	})
	cfg := newTestConfig(t, srv.URL)

	client := NewClientFromURL(srv.URL)
	err := RunBrowserBootstrap(context.Background(), client, cfg)
	if err == nil {
		t.Fatal("RunBrowserBootstrap returned nil with unknown setup_method, want error")
	}
	if !strings.Contains(err.Error(), "future_method") {
		t.Errorf("error %q should echo back the unknown method for diagnosis", err.Error())
	}
}

// withFastPolling shrinks the helper's tick interval and timeout to
// millisecond scale for the duration of a test, then restores them.
// Required for asserting on the timeout-error branch (5-min default
// would make the test hang forever) and for keeping the happy-path
// assertions fast.
func withFastPolling(t *testing.T, interval, timeout time.Duration) {
	t.Helper()
	prevInterval := bootstrapPollInterval
	prevTimeout := bootstrapPollTimeout
	bootstrapPollInterval = interval
	bootstrapPollTimeout = timeout
	t.Cleanup(func() {
		bootstrapPollInterval = prevInterval
		bootstrapPollTimeout = prevTimeout
	})
}

// TestRunBrowserBootstrap_TimeoutFires asserts the helper's own internal
// timeout branch surfaces a friendly, actionable error (not just a context
// error). Distinct from the ContextCancelled test below: that one drives
// cancellation from the caller's context; this one fires the helper's
// 5-minute (here: 100ms) safety timer.
func TestRunBrowserBootstrap_TimeoutFires(t *testing.T) {
	withFastPolling(t, 10*time.Millisecond, 100*time.Millisecond)

	srv := fakeSessionServer(t, func() SessionResponse {
		// Server never finishes setup — forces the helper into its timeout.
		return SessionResponse{SetupRequired: true, SetupMethod: "logs_token"}
	})
	cfg := newTestConfig(t, srv.URL)
	writeBootstrapToken(t, cfg.DataDir, "tok")

	client := NewClientFromURL(srv.URL)
	err := RunBrowserBootstrap(context.Background(), client, cfg)
	if err == nil {
		t.Fatal("RunBrowserBootstrap returned nil after internal timeout, want error")
	}
	// Should be the friendly "timed out" error, NOT context.DeadlineExceeded
	// — the latter would mean the helper conflated caller cancellation with
	// its own internal timer (the bug the prior round caught).
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v: helper conflated its own timeout with caller-ctx cancellation", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %q, want a 'timed out' message", err.Error())
	}
	if !strings.Contains(err.Error(), "--cli-prompt") {
		t.Errorf("err = %q should mention --cli-prompt fallback", err.Error())
	}
}

// TestRunBrowserBootstrap_ContextCancelled — operator hits Ctrl+C while
// the helper is polling. The caller's context is cancelled; helper should
// return ctx.Err() promptly so the outer command can exit cleanly.
func TestRunBrowserBootstrap_ContextCancelled(t *testing.T) {
	srv := fakeSessionServer(t, func() SessionResponse {
		// Always say setup-required, so the helper polls forever absent
		// cancellation.
		return SessionResponse{SetupRequired: true, SetupMethod: "logs_token"}
	})
	cfg := newTestConfig(t, srv.URL)
	writeBootstrapToken(t, cfg.DataDir, "tok")

	client := NewClientFromURL(srv.URL)

	// Cancel after 100ms — well inside the 2s tick interval, so we're
	// asserting the select branch fires on ctx.Done(), not on a stale
	// tick.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := RunBrowserBootstrap(ctx, client, cfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("RunBrowserBootstrap returned nil after ctx cancellation, want error")
	}
	// Should propagate the context error verbatim (DeadlineExceeded here).
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	// And it should return promptly — within roughly the tick interval.
	// 3s gives generous headroom on a slow CI box.
	if elapsed > 3*time.Second {
		t.Errorf("RunBrowserBootstrap took %s to honour ctx cancellation, want <3s", elapsed)
	}
}

// TestBuildBootstrapURL_LogsTokenFragment locks down the token-in-fragment
// shape: /setup#token=<TOKEN>. The fragment-not-query choice is load-
// bearing — fragments aren't sent to the server, never appear in access
// logs, and /setup scrubs them from the address bar before paint. A
// regression here that quietly switched to ?token= would defeat all of
// that.
func TestBuildBootstrapURL_LogsTokenFragment(t *testing.T) {
	cfg := newTestConfig(t, "http://example.test:7777")
	writeBootstrapToken(t, cfg.DataDir, "abc123")

	got, err := buildBootstrapURL(cfg, "logs_token")
	if err != nil {
		t.Fatalf("buildBootstrapURL: %v", err)
	}
	want := "http://example.test:7777/setup#token=abc123"
	if got != want {
		t.Errorf("buildBootstrapURL = %q, want %q", got, want)
	}
}

// TestReadBootstrapToken_TrimsTrailingNewline — EnsureBootstrapToken
// writes "<token>\n", so a faithful read must strip the trailing newline
// (and any incidental whitespace). Otherwise the URL would carry a stray
// %0A and the server's constant-time compare would reject it.
func TestReadBootstrapToken_TrimsTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, bootstrapTokenFilename), []byte("hello-token\n"), 0600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	got, err := readBootstrapToken(dir)
	if err != nil {
		t.Fatalf("readBootstrapToken: %v", err)
	}
	if got != "hello-token" {
		t.Errorf("readBootstrapToken = %q, want %q", got, "hello-token")
	}
}
