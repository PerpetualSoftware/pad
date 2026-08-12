package cli

// On-disk session registry (TASK-2533, per IDEA-2465's recon comments).
//
// Forward-looking infra: Phase 1's nudge delivery ended up being plugin
// monitors (stdout lines the Claude Code harness delivers directly to
// the owning session), which need no session registry at all — the
// earlier messaging-socket-based delivery path this registry was
// originally scoped for was abandoned (see IDEA-2465's recon comments,
// "socket path dead"). `pad session register` remains a PLAN-2469 Phase
// 1 deliverable anyway: it's the discovery mechanism Phase 3's
// live-sessions/presence surface (IDEA-2464) will need, and the recon
// that found it (CLAUDE_CODE_MESSAGING_SOCKET + cwd passively available
// to every `pad` invocation inside a Claude Code session) is cheap to
// capture now rather than re-derive later. Nothing consumes this
// registry yet.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SessionRegistration is one running CLI session's on-disk record.
type SessionRegistration struct {
	PID int `json:"pid"`
	// Cwd is the directory the session was registered from — the same
	// signal `pad watch --stream --for-session`'s silent-start logic
	// uses to find (or fail to find) a .pad.toml.
	Cwd string `json:"cwd"`
	// MessagingSocketPath is CLAUDE_CODE_MESSAGING_SOCKET's value at
	// registration time, or "" when the process isn't running inside a
	// Claude Code session (or the harness hasn't exported it). Presence
	// registration doesn't require it — a bare pid+cwd is still useful
	// for Phase 3.
	MessagingSocketPath string `json:"messaging_socket_path,omitempty"`
	// RegisteredAt is RFC3339 UTC.
	RegisteredAt string `json:"registered_at"`
}

// SessionsDir returns ~/.pad/sessions, creating it if it doesn't exist.
func SessionsDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home directory: %w", err)
	}
	dir := filepath.Join(homeDir, ".pad", "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create sessions directory: %w", err)
	}
	return dir, nil
}

// RegisterSession writes a SessionRegistration for the CURRENT process
// (os.Getpid()) to ~/.pad/sessions/<pid>.json, mode 0600 — mirroring
// credentials.json's permissions (this file also names a filesystem
// path, so it deserves the same care). Re-registering the same pid
// (e.g. a second `pad session register` call in the same session)
// simply overwrites the file with a fresh RegisteredAt. Returns the
// path written.
func RegisterSession(cwd, messagingSocketPath string) (string, error) {
	dir, err := SessionsDir()
	if err != nil {
		return "", err
	}

	reg := SessionRegistration{
		PID:                 os.Getpid(),
		Cwd:                 cwd,
		MessagingSocketPath: messagingSocketPath,
		RegisteredAt:        time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal session registration: %w", err)
	}

	path := filepath.Join(dir, fmt.Sprintf("%d.json", reg.PID))
	if err := os.WriteFile(path, data, 0600); err != nil {
		return "", fmt.Errorf("write session registration: %w", err)
	}
	return path, nil
}
