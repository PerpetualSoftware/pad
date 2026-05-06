package server

// First-run bootstrap token (TASK-1167 / PLAN-1166).
//
// Pad's POST /api/v1/auth/bootstrap is loopback-gated by default — see
// requestIsLoopback in handlers_auth.go. That works fine for local installs
// and `docker exec`, but fails the homelab UX: in a container, "loopback"
// is inside the container, so a user pointing their browser at
// http://<unraid-host>:7777/setup can never claim the first admin.
//
// This file implements the logs-token escape hatch: on first start with no
// users in self-host mode (cloud mode is excluded — D10), the server
// generates a one-time token, persists it to <DataDir>/.bootstrap-token
// (mode 0600), and logs it in a banner the operator can grab from
// `docker logs`. The token is required via the X-Bootstrap-Token header
// (header-only, never query — F6) and bypasses the loopback gate iff
// !cloudMode. After the first admin is successfully created the token is
// consumed (in-memory cleared + file deleted).
//
// Concurrency: handleBootstrap takes s.bootstrapMu for the entire
// validate-token → check-UserCount → CreateUser → consume sequence (F5).
// Two simultaneous valid-token requests with different emails would
// otherwise create two admins from one token; the mutex serializes them
// so only the first wins.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// bootstrapTokenFilename is the on-disk filename within DataDir.
const bootstrapTokenFilename = ".bootstrap-token"

// BootstrapTokenHeader is the HTTP header carrying the token. POST
// /api/v1/auth/bootstrap accepts the token via this header only — never
// via a query parameter — to keep it out of access logs and proxy/CDN
// records (F6 / D9).
const BootstrapTokenHeader = "X-Bootstrap-Token"

// BootstrapTokenPath returns the on-disk path for the bootstrap token file.
func BootstrapTokenPath(dataDir string) string {
	return filepath.Join(dataDir, bootstrapTokenFilename)
}

// EnsureBootstrapToken generates a fresh first-run bootstrap token (or
// loads the existing one if a valid file is already present). Returns
// the token string + the absolute path it was written to. Caller wires
// both into the Server via SetBootstrapToken.
//
// Generation uses the same atomic-create-via-temp+hardlink pattern as
// EnsureEncryptionKey in internal/config/config.go so two simultaneous
// startups can't clobber each other's file.
//
// On failure (read-only data dir, permission denied, etc.) the caller
// should treat this as non-fatal (D7) — log a warning, continue with
// no token. The user falls back to the loopback-only bootstrap path
// (`docker exec pad pad auth setup`).
func EnsureBootstrapToken(dataDir string) (token, path string, err error) {
	tokenPath := BootstrapTokenPath(dataDir)

	// File exists: load. Reject world/group permissions so an operator
	// who cat'd the token to /tmp doesn't accidentally hand it to other
	// local users (security parity with encryption.key).
	if info, statErr := os.Stat(tokenPath); statErr == nil {
		if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
			return "", tokenPath, fmt.Errorf("bootstrap token %s has mode %o (group/other bits set); run `chmod 600 %s` to restrict",
				tokenPath, info.Mode().Perm(), tokenPath)
		}
		data, rerr := os.ReadFile(tokenPath)
		if rerr != nil {
			return "", tokenPath, fmt.Errorf("read bootstrap token: %w", rerr)
		}
		t := strings.TrimSpace(string(data))
		if t == "" {
			return "", tokenPath, fmt.Errorf("bootstrap token file %s is empty; delete it to regenerate", tokenPath)
		}
		return t, tokenPath, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", tokenPath, fmt.Errorf("stat bootstrap token: %w", statErr)
	}

	// Tighten DataDir before dropping the token in.
	if mkErr := os.MkdirAll(dataDir, 0700); mkErr != nil {
		return "", tokenPath, fmt.Errorf("create data dir for bootstrap token: %w", mkErr)
	}

	// 32 bytes from crypto/rand → base64url, no padding (~43 chars).
	raw := make([]byte, 32)
	if _, rerr := rand.Read(raw); rerr != nil {
		return "", tokenPath, fmt.Errorf("generate bootstrap token: %w", rerr)
	}
	t := base64.RawURLEncoding.EncodeToString(raw)

	// Atomic temp+hardlink — see EnsureEncryptionKey for the rationale.
	tmpSuffix := make([]byte, 8)
	if _, rerr := rand.Read(tmpSuffix); rerr != nil {
		return "", tokenPath, fmt.Errorf("generate bootstrap token temp suffix: %w", rerr)
	}
	tmpPath := tokenPath + ".tmp." + base64.RawURLEncoding.EncodeToString(tmpSuffix)
	if werr := os.WriteFile(tmpPath, []byte(t+"\n"), 0600); werr != nil {
		return "", tokenPath, fmt.Errorf("write bootstrap token temp: %w", werr)
	}
	defer os.Remove(tmpPath) // best-effort cleanup; harmless on the winning path

	if lerr := os.Link(tmpPath, tokenPath); lerr != nil {
		if errors.Is(lerr, os.ErrExist) {
			// Lost a race against a sibling startup. Read the winner's token.
			data, rerr := os.ReadFile(tokenPath)
			if rerr != nil {
				return "", tokenPath, fmt.Errorf("reload bootstrap token after race: %w", rerr)
			}
			return strings.TrimSpace(string(data)), tokenPath, nil
		}
		return "", tokenPath, fmt.Errorf("link bootstrap token to %s: %w", tokenPath, lerr)
	}

	return t, tokenPath, nil
}

// CleanupStaleBootstrapToken removes a token file left behind by a previous
// run that completed bootstrap but failed to delete the file. Called at
// startup when UserCount > 0 (D4). Best-effort; silently ignores
// not-found.
func CleanupStaleBootstrapToken(dataDir string) error {
	err := os.Remove(BootstrapTokenPath(dataDir))
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// SetBootstrapToken wires the in-memory token + on-disk path into the
// Server. Called once at startup from cmd/pad/main.go after
// EnsureBootstrapToken succeeds. Empty token is allowed (means: no
// token configured — used when EnsureBootstrapToken failed or when the
// instance is in cloud mode).
func (s *Server) SetBootstrapToken(token, path string) {
	s.bootstrapMu.Lock()
	defer s.bootstrapMu.Unlock()
	s.bootstrapToken = token
	s.bootstrapTokenPath = path
}

// hasBootstrapToken reports whether a bootstrap token is currently
// configured. Lock-aware — safe to call without holding s.bootstrapMu.
// Used by handleSessionCheck to decide whether to advertise
// setup_method=logs_token.
func (s *Server) hasBootstrapToken() bool {
	s.bootstrapMu.Lock()
	defer s.bootstrapMu.Unlock()
	return s.bootstrapToken != ""
}

// checkBootstrapToken validates the X-Bootstrap-Token header on r against
// the in-memory token using a constant-time comparison. Returns false if
// no token is configured or the header is missing/wrong.
//
// Caller MUST hold s.bootstrapMu — handleBootstrap takes the lock for
// the entire validate-token → check-UserCount → CreateUser → consume
// sequence (F5).
func (s *Server) checkBootstrapToken(r *http.Request) bool {
	if s.bootstrapToken == "" {
		return false
	}
	provided := r.Header.Get(BootstrapTokenHeader)
	if provided == "" {
		return false
	}
	// Use constant-time comparison so the response time can't leak
	// per-character feedback to an attacker grinding through guesses.
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.bootstrapToken)) == 1
}

// SetBypassSetupToken wires the operator's PAD_BYPASS_SETUP_TOKEN choice
// into the Server. When true, handleBootstrap accepts a self-host
// first-admin POST from any IP without an X-Bootstrap-Token header.
// Cloud mode never honors this flag (see Server.bypassSetupToken doc).
//
// Called once at startup from cmd/pad/main.go before the HTTP server
// starts accepting connections, so no concurrent reader can observe a
// half-set value; the field reads in handleBootstrap and
// handleSessionCheck are therefore unguarded.
func (s *Server) SetBypassSetupToken(bypass bool) {
	s.bypassSetupToken = bypass
}

// openBootstrapEnabled reports whether open-bootstrap is active for this
// server: bypass flag is set AND we're not in cloud mode. Cloud mode
// always wins over the bypass flag; the field on its own is meaningless
// without that check.
func (s *Server) openBootstrapEnabled() bool {
	return s.bypassSetupToken && !s.cloudMode
}

// consumeBootstrapToken clears the in-memory token and deletes the on-disk
// file. Called after the first admin is successfully created.
//
// Caller MUST hold s.bootstrapMu.
//
// File-removal failure is logged by the caller (we return the error) but
// the in-memory token is cleared regardless — once consumption starts the
// token is "spent" from the server's perspective even if the rm syscall
// fails (defense-in-depth: a stale file with a no-longer-accepted token
// is harmless; the next startup's CleanupStaleBootstrapToken call mops it
// up).
func (s *Server) consumeBootstrapToken() error {
	s.bootstrapToken = ""
	if s.bootstrapTokenPath == "" {
		return nil
	}
	err := os.Remove(s.bootstrapTokenPath)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
