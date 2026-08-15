package main

// Tests for pollAndSaveCLIAuth's exit paths introduced by BUG-2572: the
// wall-clock timeout and the consecutive-transient-poll-error bound. These
// deliberately run serially (no t.Parallel) — withFastCLIAuthPolling mutates
// the package-level cliAuthPollInterval / cliAuthPollTimeout /
// cliAuthMaxConsecutivePollErrs vars for the duration of the test, so
// parallel execution would race across tests in this file.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// withFastCLIAuthPolling shrinks pollAndSaveCLIAuth's interval/timeout/error
// bound for the duration of the test, then restores them. Mirrors
// internal/cli/bootstrap_test.go's withFastPolling.
func withFastCLIAuthPolling(t *testing.T, interval, timeout time.Duration, maxErrs int) {
	t.Helper()
	prevInterval := cliAuthPollInterval
	prevTimeout := cliAuthPollTimeout
	prevMaxErrs := cliAuthMaxConsecutivePollErrs
	cliAuthPollInterval = interval
	cliAuthPollTimeout = timeout
	cliAuthMaxConsecutivePollErrs = maxErrs
	t.Cleanup(func() {
		cliAuthPollInterval = prevInterval
		cliAuthPollTimeout = prevTimeout
		cliAuthMaxConsecutivePollErrs = prevMaxErrs
	})
}

// cliAuthPollServer builds an httptest.Server serving GET
// /api/v1/auth/cli/sessions/{code}, delegating each request to handle so
// tests can script status sequences or failures.
func cliAuthPollServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/auth/cli/sessions/") {
			http.NotFound(w, r)
			return
		}
		handle(w, r)
	}))
}

func testSession() *cli.CLIAuthSessionResponse {
	return &cli.CLIAuthSessionResponse{SessionCode: "test-code"}
}

// TestPollAndSaveCLIAuth_TimeoutFires asserts the new wall-clock timer ends
// the loop with a distinct, remedy-neutral error when the server never
// reports anything but "pending" — the scenario an unreachable-but-still-
// responding server can't distinguish from a slow human, and the case a
// permanently-unresponsive server degrades to once its consecutive-error
// bound would otherwise not apply (server IS reachable here, just stuck).
func TestPollAndSaveCLIAuth_TimeoutFires(t *testing.T) {
	withFastCLIAuthPolling(t, 5*time.Millisecond, 50*time.Millisecond, 1000)

	srv := cliAuthPollServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cli.CLIAuthSessionStatus{Status: "pending"})
	})
	defer srv.Close()

	client := cli.NewClientFromURL(srv.URL)
	cfg := remoteHeadlessCfg(srv.URL)

	err := pollAndSaveCLIAuth(t.Context(), client, cfg, testSession())
	if err == nil {
		t.Fatal("pollAndSaveCLIAuth returned nil after the wall-clock timeout, want error")
	}
	if !strings.Contains(err.Error(), "timed out waiting for approval") {
		t.Errorf("error %q should mention the timeout", err.Error())
	}
	// Remedy-neutral: this helper is shared by login, setup, and pad init's
	// linking path, so it must not steer everyone toward "pad auth login".
	if strings.Contains(err.Error(), "pad auth login") {
		t.Errorf("error %q should not suggest a specific remedy — callers add their own", err.Error())
	}
}

// TestPollAndSaveCLIAuth_ConsecutiveErrorsBound asserts a server that only
// ever errors trips the consecutive-error bound with a network-shaped error
// well before the (much larger) wall-clock timeout would fire, and that the
// error wraps the last poll failure.
func TestPollAndSaveCLIAuth_ConsecutiveErrorsBound(t *testing.T) {
	withFastCLIAuthPolling(t, 5*time.Millisecond, 5*time.Second, 3)

	srv := cliAuthPollServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	})
	defer srv.Close()

	client := cli.NewClientFromURL(srv.URL)
	cfg := remoteHeadlessCfg(srv.URL)

	start := time.Now()
	err := pollAndSaveCLIAuth(t.Context(), client, cfg, testSession())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("pollAndSaveCLIAuth returned nil after repeated poll errors, want error")
	}
	if !strings.Contains(err.Error(), "could not reach server") {
		t.Errorf("error %q should mention the server being unreachable", err.Error())
	}
	if elapsed >= 5*time.Second {
		t.Errorf("took %s — consecutive-error bound should fire well before the wall-clock timeout", elapsed)
	}
}

// TestPollAndSaveCLIAuth_TransientErrorsRecover asserts the consecutive-error
// counter resets on any successful poll rather than accumulating across the
// whole run, so isolated blips don't eventually trip the bound.
func TestPollAndSaveCLIAuth_TransientErrorsRecover(t *testing.T) {
	isolateHome(t) // approval saves credentials via $HOME/.pad/credentials.json
	withFastCLIAuthPolling(t, 5*time.Millisecond, 5*time.Second, 2)

	var reqs atomic.Int32
	srv := cliAuthPollServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Single errors at n=1 and n=3, each followed by a success (n=2
		// "pending", n=4 "approved") that should reset the counter. With
		// maxErrs=2, a single consecutive run never reaches the bound — but
		// a counter that accumulated across the whole run instead of
		// resetting on success would hit 2 total errors by n=3 and fail
		// this test, which is exactly the bug this test guards against.
		n := reqs.Add(1)
		if n == 1 || n == 3 {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if n == 2 {
			_ = json.NewEncoder(w).Encode(cli.CLIAuthSessionStatus{Status: "pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(cli.CLIAuthSessionStatus{
			Status: "approved",
			Token:  "padsess_test",
			User:   cli.LoginUser{ID: "user-1", Email: "admin@example.com", Name: "Test Admin"},
		})
	})
	defer srv.Close()

	client := cli.NewClientFromURL(srv.URL)
	cfg := remoteHeadlessCfg(srv.URL)

	err := pollAndSaveCLIAuth(t.Context(), client, cfg, testSession())
	if err != nil {
		t.Fatalf("pollAndSaveCLIAuth: unexpected error: %v", err)
	}
}
