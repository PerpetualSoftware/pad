package server

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestEnsureBootstrapToken_GeneratesAndPersists verifies a fresh data
// directory yields a freshly-generated token written with mode 0600 in
// the expected on-disk location.
func TestEnsureBootstrapToken_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	token, path, err := EnsureBootstrapToken(dir)
	if err != nil {
		t.Fatalf("EnsureBootstrapToken: %v", err)
	}
	if token == "" {
		t.Fatal("token is empty")
	}
	// 32 bytes base64url-no-padding ≈ 43 chars. Allow a little wiggle
	// for any future encoding tweak but reject anything obviously short.
	if len(token) < 40 {
		t.Fatalf("token length = %d, want >= 40 (32 bytes base64url-no-padding)", len(token))
	}
	if path != filepath.Join(dir, ".bootstrap-token") {
		t.Fatalf("path = %q, want %q", path, filepath.Join(dir, ".bootstrap-token"))
	}

	// File exists, mode 0600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0600 {
			t.Fatalf("token file mode = %o, want 0600", got)
		}
	}

	// File contents match returned token (with a trailing newline; the
	// loader trims it).
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != token {
		t.Fatalf("file contents = %q, want %q", got, token)
	}
}

// TestEnsureBootstrapToken_LoadsExisting verifies a second call against
// the same data directory returns the same token (D1: persists across
// restarts; do not regenerate).
func TestEnsureBootstrapToken_LoadsExisting(t *testing.T) {
	dir := t.TempDir()
	first, _, err := EnsureBootstrapToken(dir)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	second, _, err := EnsureBootstrapToken(dir)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if first != second {
		t.Fatalf("token regenerated across calls: %q vs %q", first, second)
	}
}

// TestEnsureBootstrapToken_RejectsOverlyPermissiveFile mirrors the
// EnsureEncryptionKey check — an operator who accidentally chmods the
// token file 0644 hands the secret to other local users on a multi-user
// host, which defeats the whole point of the file-system gate.
func TestEnsureBootstrapToken_RejectsOverlyPermissiveFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are POSIX-only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".bootstrap-token")
	if err := os.WriteFile(path, []byte("hand-seeded-token\n"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	_, _, err := EnsureBootstrapToken(dir)
	if err == nil {
		t.Fatal("expected error for 0644 token file, got nil")
	}
	// The error formats the mode as %o ("644"), not %04o ("0644"). Match
	// what's actually emitted so the test pins the user-facing message.
	if !strings.Contains(err.Error(), "mode 644") || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("error = %q, want it to mention the bad mode and the chmod fix", err.Error())
	}
}

// TestEnsureBootstrapToken_EmptyFileIsAnError protects against a partially-
// created token file being silently treated as valid (which would leave
// any X-Bootstrap-Token: "" request matching).
func TestEnsureBootstrapToken_EmptyFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".bootstrap-token")
	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatalf("seed empty file: %v", err)
	}
	_, _, err := EnsureBootstrapToken(dir)
	if err == nil {
		t.Fatal("expected error for empty token file, got nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error = %q, want it to mention the file is empty", err.Error())
	}
}

// TestEnsureBootstrapToken_ReadOnlyDataDir verifies D7: failure to persist
// is reported as an error (caller treats it as non-fatal at startup).
// Server must NOT abort startup over this — the caller's responsibility
// is to log a warning and proceed with no token.
func TestEnsureBootstrapToken_ReadOnlyDataDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are POSIX-only")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses the read-only check")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "data")
	if err := os.Mkdir(dir, 0500); err != nil {
		t.Fatalf("mkdir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) }) // allow t.TempDir cleanup

	_, _, err := EnsureBootstrapToken(dir)
	if err == nil {
		t.Fatal("expected error on read-only data dir, got nil")
	}
}

// TestEnsureBootstrapToken_ConcurrentRace ensures the temp+hardlink
// pattern protects against two simultaneous startups racing on the same
// data directory. Both should converge on identical tokens — the loser
// reads the winner's fully-written file.
func TestEnsureBootstrapToken_ConcurrentRace(t *testing.T) {
	dir := t.TempDir()
	const N = 8

	tokens := make([]string, N)
	errs := make([]error, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			tok, _, err := EnsureBootstrapToken(dir)
			tokens[idx] = tok
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	for i := 1; i < N; i++ {
		if tokens[i] != tokens[0] {
			t.Fatalf("goroutine %d got %q, want %q (winner)", i, tokens[i], tokens[0])
		}
	}
}

// TestCleanupStaleBootstrapToken covers the D4 path: a token file left
// behind by a previous successful bootstrap whose `os.Remove` somehow
// failed. Startup with UserCount > 0 is supposed to mop it up.
func TestCleanupStaleBootstrapToken(t *testing.T) {
	dir := t.TempDir()

	// No file → no error.
	if err := CleanupStaleBootstrapToken(dir); err != nil {
		t.Fatalf("cleanup of missing file: %v", err)
	}

	// Seed a stale file → cleanup deletes it.
	path := filepath.Join(dir, ".bootstrap-token")
	if err := os.WriteFile(path, []byte("stale\n"), 0600); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	if err := CleanupStaleBootstrapToken(dir); err != nil {
		t.Fatalf("cleanup of stale file: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale file still exists after cleanup: stat err = %v", err)
	}
}

// TestServer_BootstrapTokenLifecycle covers the in-memory state on the
// Server: SetBootstrapToken plumbs values, hasBootstrapToken reports
// presence, checkBootstrapToken validates the header, consume clears
// in-memory + removes the file.
func TestServer_BootstrapTokenLifecycle(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, ".bootstrap-token")
	if err := os.WriteFile(tokenPath, []byte("real-token-value\n"), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	srv := &Server{}

	// Empty by default.
	if srv.hasBootstrapToken() {
		t.Fatal("hasBootstrapToken() = true on fresh server, want false")
	}

	srv.SetBootstrapToken("real-token-value", tokenPath)
	if !srv.hasBootstrapToken() {
		t.Fatal("hasBootstrapToken() = false after SetBootstrapToken, want true")
	}

	// Header validation.
	srv.bootstrapMu.Lock()
	defer srv.bootstrapMu.Unlock()

	r := httptest.NewRequest("POST", "/api/v1/auth/bootstrap", nil)
	if srv.checkBootstrapToken(r) {
		t.Fatal("checkBootstrapToken with no header returned true")
	}
	r.Header.Set(BootstrapTokenHeader, "wrong-token")
	if srv.checkBootstrapToken(r) {
		t.Fatal("checkBootstrapToken with wrong token returned true")
	}
	r.Header.Set(BootstrapTokenHeader, "real-token-value")
	if !srv.checkBootstrapToken(r) {
		t.Fatal("checkBootstrapToken with correct header returned false")
	}

	// Consume clears in-memory and removes file.
	if err := srv.consumeBootstrapToken(); err != nil {
		t.Fatalf("consumeBootstrapToken: %v", err)
	}
	if srv.bootstrapToken != "" {
		t.Fatal("bootstrapToken not cleared after consume")
	}
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("token file still exists after consume: stat err = %v", err)
	}

	// Subsequent check returns false (token is "" and header would no longer match).
	r.Header.Set(BootstrapTokenHeader, "real-token-value")
	if srv.checkBootstrapToken(r) {
		t.Fatal("checkBootstrapToken after consume returned true")
	}

	// Consume is idempotent (file already gone).
	if err := srv.consumeBootstrapToken(); err != nil {
		t.Fatalf("second consume: %v", err)
	}
}

// TestServer_BootstrapTokenHeaderOnly_NoQuerySupport pins the API contract
// that the token is accepted via X-Bootstrap-Token only (F6). A token in
// ?token=<x> must not satisfy checkBootstrapToken — that path lives on the
// frontend GET and is scrubbed by replaceState; the POST endpoint never
// reads from the query.
func TestServer_BootstrapTokenHeaderOnly_NoQuerySupport(t *testing.T) {
	srv := &Server{}
	srv.SetBootstrapToken("real-token-value", "")

	srv.bootstrapMu.Lock()
	defer srv.bootstrapMu.Unlock()

	r := httptest.NewRequest("POST", "/api/v1/auth/bootstrap?token=real-token-value", nil)
	// No header set → must reject regardless of query content.
	if srv.checkBootstrapToken(r) {
		t.Fatal("checkBootstrapToken accepted query-only token (security regression)")
	}
}
