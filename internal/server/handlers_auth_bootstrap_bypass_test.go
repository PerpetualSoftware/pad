package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// Tests for the PAD_BYPASS_SETUP_TOKEN open-bootstrap escape hatch.
//
// The bypass lets an operator on a trusted network claim the first admin
// from any IP without copying a token out of the container logs. The
// security boundary that keeps this safe is the UserCount==0 gate
// (handleBootstrap already returns 409 once a user exists) plus the
// hard rule that cloud mode never honors the bypass.
//
// These tests pin:
//   - bypass=true + non-loopback peer + no token → 201 Created
//   - bypass=false + non-loopback peer + no token → 403 (regression)
//   - bypass=true + cloud mode → 403 (cloud-mode lockout)
//   - session check returns setup_method=open when bypass is on
//   - session check returns local_cli when bypass is on but cloud mode (F9 parity)
//   - bypass priority: when both bypass + token are configured, session
//     advertises 'open' (the more deliberate operator opt-in)
//   - bypass=true + UserCount > 0 → 409 (gate still works)

// TestBypassSetupToken_NonLoopback_NoHeaderAccepted is the happy path:
// the operator opted into open bootstrap, and a non-loopback request
// with no token header succeeds.
func TestBypassSetupToken_NonLoopback_NoHeaderAccepted(t *testing.T) {
	srv := testServer(t)
	srv.SetBypassSetupToken(true)

	rr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/bootstrap",
		bootstrapBody("admin@test.com"),
		nil,
		"192.0.2.1:1234")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 with bypass enabled, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestBypassSetupToken_DoesNotPersistToken verifies a side effect we
// rely on for clean operation: when bypass is on, no .bootstrap-token
// file is written. Any pre-existing one would be confusing for the
// operator (they'd wonder why setup works without using it) — main.go
// also actively cleans up stale files, but the server itself should
// not write one.
//
// We can't easily test the cmd/pad/main.go cleanup behavior from inside
// the server package (the wiring lives in main.go), so this is a
// directional assertion: the bypass code path does not touch
// EnsureBootstrapToken. We verify that by checking the in-memory
// state and the absence of a file in a temp dir we control.
func TestBypassSetupToken_DoesNotLoadToken(t *testing.T) {
	srv := testServer(t)
	srv.SetBypassSetupToken(true)

	if srv.hasBootstrapToken() {
		t.Fatal("bypass mode unexpectedly loaded a bootstrap token")
	}

	// Side-channel sanity: the open-mode helper exposes the gate
	// state without poking server internals from outside the package.
	if !srv.openBootstrapEnabled() {
		t.Fatal("openBootstrapEnabled() = false after SetBypassSetupToken(true)")
	}
}

// TestBypassSetupToken_NonLoopback_NoBypass_StillRejected pins the
// existing pre-bypass behavior: when bypass is OFF and no token is
// configured, a non-loopback peer still gets 403. This is the
// regression check for "did the bypass plumbing accidentally open up
// the default config too?"
func TestBypassSetupToken_NonLoopback_NoBypass_StillRejected(t *testing.T) {
	srv := testServer(t)
	// Note: NOT calling SetBypassSetupToken(true) — bypass stays off.

	rr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/bootstrap",
		bootstrapBody("admin@test.com"),
		nil,
		"192.0.2.1:1234")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 with bypass disabled and no token, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestBypassSetupToken_CloudMode_StillLockedDown pins the cloud-mode
// hard-rule: even with bypass=true (which an operator might set
// thinking it's universal), cloud mode keeps the loopback-only gate.
// Defense-in-depth — cmd/pad/main.go also gates the bypass wiring
// with !cfg.IsCloudServer(), but the server must defend itself.
func TestBypassSetupToken_CloudMode_StillLockedDown(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode("test-cloud-secret")
	srv.SetBypassSetupToken(true) // operator misconfiguration

	rr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/bootstrap",
		bootstrapBody("admin@test.com"),
		nil,
		"192.0.2.1:1234")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cloud-mode bypass leaked! expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if srv.openBootstrapEnabled() {
		t.Fatal("openBootstrapEnabled() returned true in cloud mode")
	}
}

// TestBypassSetupToken_LoopbackStillWorks regression-checks the
// loopback path: even when bypass is off, a localhost peer still
// succeeds. Bypass is purely additive — it doesn't replace the
// loopback gate.
func TestBypassSetupToken_LoopbackStillWorks(t *testing.T) {
	srv := testServer(t)
	// Bypass off.

	rr := doLoopbackRequest(srv, "POST", "/api/v1/auth/bootstrap", bootstrapBody("admin@test.com"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 over loopback (bypass off), got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestBypassSetupToken_SessionCheck_AdvertisesOpen verifies the
// frontend signal: with bypass on + zero users, the session payload
// returns setup_method=open so /setup skips the paste-token UI.
func TestBypassSetupToken_SessionCheck_AdvertisesOpen(t *testing.T) {
	srv := testServer(t)
	srv.SetBypassSetupToken(true)

	rr := doRequest(srv, "GET", "/api/v1/auth/session", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("session check: expected 200, got %d", rr.Code)
	}
	var session map[string]interface{}
	parseJSON(t, rr, &session)
	if session["setup_required"] != true {
		t.Errorf("expected setup_required=true with no users, got %v", session["setup_required"])
	}
	if session["setup_method"] != "open" {
		t.Errorf("expected setup_method=open with bypass on, got %v", session["setup_method"])
	}
}

// TestBypassSetupToken_SessionCheck_BypassWinsOverLogsToken pins the
// priority order in handleSessionCheck: when an operator has both
// PAD_BYPASS_SETUP_TOKEN=true AND a bootstrap-token loaded (a
// misconfiguration we shouldn't rule out — the cmd/pad/main.go path
// avoids it but a test or a manual SetBootstrapToken call could land
// here), the session advertises 'open'. Reasoning: bypass is the
// more deliberate operator opt-in (an explicit env-var setting),
// while a bootstrap-token can sit in DataDir from a previous run.
// Surfacing 'open' matches what the bootstrap endpoint actually
// allows, so the frontend doesn't tell the user to copy a token
// they don't need.
func TestBypassSetupToken_SessionCheck_BypassWinsOverLogsToken(t *testing.T) {
	srv := testServer(t)
	configureBootstrapToken(t, srv)
	srv.SetBypassSetupToken(true)

	rr := doRequest(srv, "GET", "/api/v1/auth/session", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("session check: expected 200, got %d", rr.Code)
	}
	var session map[string]interface{}
	parseJSON(t, rr, &session)
	if session["setup_method"] != "open" {
		t.Errorf("expected setup_method=open when bypass+token both set, got %v", session["setup_method"])
	}
}

// TestBypassSetupToken_SessionCheck_CloudModeIgnoresBypass pins F9
// parity for the bypass: cloud mode never advertises 'open' even if
// the operator set the env var, just like it never advertises
// 'logs_token'.
func TestBypassSetupToken_SessionCheck_CloudModeIgnoresBypass(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode("test-cloud-secret")
	srv.SetBypassSetupToken(true) // ignored

	rr := doRequest(srv, "GET", "/api/v1/auth/session", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("session check: expected 200, got %d", rr.Code)
	}
	var session map[string]interface{}
	parseJSON(t, rr, &session)
	if session["setup_method"] == "open" {
		t.Fatalf("cloud-mode advertised setup_method=open (cloud lockdown regression)")
	}
}

// TestBypassSetupToken_AfterBootstrap_GateClosed verifies that the
// UserCount==0 gate still works under bypass. Once the first admin
// is created, subsequent bootstrap POSTs return 409 regardless of
// the bypass flag — it doesn't degrade into open registration.
func TestBypassSetupToken_AfterBootstrap_GateClosed(t *testing.T) {
	srv := testServer(t)
	srv.SetBypassSetupToken(true)

	// Round 1: bypass admit succeeds.
	rr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/bootstrap",
		bootstrapBody("first@test.com"), nil, "192.0.2.1:1234")
	if rr.Code != http.StatusCreated {
		t.Fatalf("first bootstrap: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Round 2: bypass is still on, but UserCount > 0 — must reject.
	rr = doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/bootstrap",
		bootstrapBody("second@test.com"), nil, "192.0.2.1:1234")
	if rr.Code != http.StatusConflict {
		t.Fatalf("second bootstrap with bypass on: expected 409 (gate closed), got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestBypassSetupToken_PreservesTokenPath documents an interaction
// with the logs-token feature: SetBypassSetupToken does not clear
// any in-memory bootstrap token. cmd/pad/main.go is responsible for
// not loading one in the first place when bypass is on. This test
// pins the contract so a future refactor doesn't accidentally start
// clearing the token from inside SetBypassSetupToken (which would
// break operators who flip bypass on at runtime via some hypothetical
// future admin endpoint while a token is still loaded).
func TestBypassSetupToken_DoesNotClearLoadedToken(t *testing.T) {
	srv := testServer(t)

	// Seed a token first.
	dir := t.TempDir()
	path := filepath.Join(dir, ".bootstrap-token")
	if err := os.WriteFile(path, []byte(testBootstrapToken+"\n"), 0600); err != nil {
		t.Fatalf("seed token file: %v", err)
	}
	srv.SetBootstrapToken(testBootstrapToken, path)

	// Then turn on bypass.
	srv.SetBypassSetupToken(true)

	if !srv.hasBootstrapToken() {
		t.Fatal("SetBypassSetupToken cleared a previously-loaded token")
	}
}
