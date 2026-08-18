package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Local arm-state file (PLAN-2613 S2, D4; lead ruling "OPTION 2" on the
// TASK-2617 trail). This is the concrete file contract S3's plugin
// monitor builds against, so its rules are load-bearing and stated in
// full here (and mirrored in the S2 evidence package):
//
//   - WHAT IT IS. One file per SESSION recording that the session has
//     armed — declared consent to receive `pad push` notifications. A
//     separate process (S3's monitor) reads it to decide whether the
//     stream it opens should announce armed=true. It is how a short-lived
//     `pad session arm` command hands its intent to the long-lived
//     monitor that actually holds the stream.
//
//   - IT IS LOCAL CLIENT STATE, NOT SERVER STATE (constraint 4). It feeds
//     exactly one thing: the ?armed=true query param the client sends at
//     connect. The SERVER's per-connection armed bit (PLAN-2613 D3, built
//     in S1) remains the SOLE authority for whether a push is delivered.
//     No reader may treat this file as proof that any server-side session
//     is armed — the server never sees it.
//
//   - LIVENESS IS MANDATORY (constraint 2, non-negotiable). A file whose
//     OWNER is dead is treated as DISARMED and may be removed by any
//     reader. "Owner dead" means: the recorded messaging socket has
//     vanished (the Claude Code session it belonged to is gone), or — for
//     the headless fallback with no socket — the recorded pid is no
//     longer running. This is the whole reason the file carries owner
//     identity rather than a bare bit: a crashed session's stale armed
//     file must NEVER arm a future monitor. That would be consent-
//     grandfathering through the filesystem, exactly the silent re-arm
//     PLAN-2613 D6 forbids. When in doubt the check fails CLOSED (treats
//     the owner as dead / the session as disarmed).
//
//   - KEYED PER SESSION (constraint 1). The key is derived from
//     CLAUDE_CODE_MESSAGING_SOCKET, which both the arming command and the
//     monitor see because they run in the same Claude Code session. With
//     no socket (a headless agent), the key falls back to the working
//     directory — PER-REPO semantics, documented as secondary: the
//     sanctioned headless arming path is .pad.toml auto_arm (D4), not
//     this file, because a short-lived `pad session arm` in a headless
//     shell owns nothing long-lived for liveness to track (its own pid
//     dies with the command). See armStateOwnerAlive.
//
//   - LIFECYCLE (constraint 3). arm writes/overwrites the file
//     (idempotent). disarm and a clean monitor exit remove it. Both are
//     ordinary CLI actions visible in the session transcript (D7).

// ArmState is one session's on-disk arm declaration. Presence of the file
// (with a live owner) is what "armed" means; the fields exist to prove
// the owner is still alive, not to carry a mutable armed bit. Armed is
// serialized as a constant true purely so a human inspecting this
// security artifact reads intent at a glance — a file that exists always
// means armed, and disarm removes it rather than flipping it.
type ArmState struct {
	Armed bool `json:"armed"`
	// PID is the process that wrote the file. For a socket-keyed session
	// it is informational (the socket is the liveness signal); for the
	// headless fallback it IS the liveness signal.
	PID int `json:"pid"`
	// Socket is CLAUDE_CODE_MESSAGING_SOCKET at arm time, or "" for the
	// headless fallback. When set, its continued existence on disk is the
	// authoritative liveness signal — it outlives the short-lived arm
	// command and vanishes with the Claude Code session.
	Socket string `json:"socket,omitempty"`
	// Cwd is the working directory at arm time — the fallback key's source
	// and useful human context when inspecting the file.
	Cwd string `json:"cwd"`
	// StartedAt is RFC3339 UTC.
	StartedAt string `json:"started_at"`
}

// armStateKey derives the per-session key and reports whether the
// headless (per-repo) fallback was used. socket wins when present; cwd is
// the fallback. Both are hashed so the filename is filesystem-safe and
// leaks neither the socket path nor the absolute cwd into a directory
// listing.
func armStateKey(socket, cwd string) (key string, headless bool) {
	if socket != "" {
		return "sess-" + shortHash(socket), false
	}
	return "repo-" + shortHash(cwd), true
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// armStatePath returns the on-disk path for this session's arm-state
// file, creating ~/.pad/sessions if needed.
func armStatePath(socket, cwd string) (string, error) {
	dir, err := SessionsDir()
	if err != nil {
		return "", err
	}
	key, _ := armStateKey(socket, cwd)
	return filepath.Join(dir, "arm-"+key+".json"), nil
}

// currentSessionArmKey returns the (socket, cwd) pair identifying THIS
// process's session, read from the environment and the working directory.
// A cwd error degrades to "" — the arm file just loses its human-readable
// cwd and, in the headless case, its key; callers that need cwd for
// keying handle the empty case by refusing (see WriteArmState).
func currentSessionArmKey() (socket, cwd string) {
	socket = os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")
	cwd, _ = os.Getwd()
	return socket, cwd
}

// WriteArmState arms the current session: it writes (or idempotently
// overwrites) this session's arm-state file, mode 0600, and returns the
// path. It refuses only when there is no key to write under at all — no
// messaging socket AND no working directory — because a file with neither
// could never be matched to a session or a repo by a reader.
func WriteArmState() (path string, err error) {
	socket, cwd := currentSessionArmKey()
	if socket == "" && cwd == "" {
		return "", fmt.Errorf("cannot arm: no messaging socket and no working directory to key the session on")
	}
	p, err := armStatePath(socket, cwd)
	if err != nil {
		return "", err
	}
	st := ArmState{
		Armed:     true,
		PID:       os.Getpid(),
		Socket:    socket,
		Cwd:       cwd,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal arm state: %w", err)
	}
	if err := os.WriteFile(p, data, 0600); err != nil {
		return "", fmt.Errorf("write arm state: %w", err)
	}
	return p, nil
}

// RemoveArmState disarms the current session by removing its arm-state
// file. It reports whether a file was actually present (so `pad session
// disarm` can tell the user "disarmed" vs "was not armed") and never
// treats a missing file as an error — disarm is idempotent.
func RemoveArmState() (removed bool, path string, err error) {
	socket, cwd := currentSessionArmKey()
	p, err := armStatePath(socket, cwd)
	if err != nil {
		return false, "", err
	}
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return false, p, nil
		}
		return false, p, fmt.Errorf("remove arm state: %w", err)
	}
	return true, p, nil
}

// readArmState reads and unmarshals the current session's arm-state file.
// A missing file returns (nil, path, nil) — the common "not armed" case,
// not an error. A malformed file returns an error the caller folds into
// "not armed" (fail closed).
func readArmState() (st *ArmState, path string, err error) {
	socket, cwd := currentSessionArmKey()
	p, err := armStatePath(socket, cwd)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, p, nil
		}
		return nil, p, err
	}
	var parsed ArmState
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, p, fmt.Errorf("parse arm state %s: %w", p, err)
	}
	return &parsed, p, nil
}

// armStateOwnerAlive implements the mandatory liveness check (constraint
// 2). A socket-keyed file is alive iff its recorded socket still exists on
// disk — that outlives the short-lived arm command and disappears with
// the Claude Code session. A headless (no-socket) file is alive iff its
// recorded pid is still running. Anything uncertain is dead (fail
// closed): a stale armed file must never arm a future monitor.
func armStateOwnerAlive(st *ArmState) bool {
	if st == nil {
		return false
	}
	if st.Socket != "" {
		if _, err := os.Stat(st.Socket); err != nil {
			return false
		}
		return true
	}
	// Headless fallback: the pid is the only owner we have. A pid <= 0 is
	// never a live owner; a short-lived arm command's pid will usually be
	// gone by the time a reader looks, which is exactly why this path is
	// documented as secondary to auto_arm.
	if st.PID <= 0 {
		return false
	}
	return pidAlive(st.PID)
}

// SessionArmedLocally reports whether THIS session has a live local arm
// declaration. It is the reader half of the file contract: it returns
// true only for a present file whose owner is still alive, and it
// opportunistically removes a file whose owner is dead so a crashed
// session's consent cannot linger (constraint 2). It never errors — every
// failure path is "not armed", because this gates a security-relevant
// declaration and must fail closed.
//
// This answers "should the stream I am about to open announce armed?" for
// the EXPLICIT path only. The caller ORs it with the auto_arm config
// resolution (ResolveAutoArmFromDisk) to get the full announced value —
// see cmd/pad's monitor wiring.
func SessionArmedLocally() bool {
	st, path, err := readArmState()
	if err != nil || st == nil {
		return false
	}
	if !armStateOwnerAlive(st) {
		// Dead owner: reap the stale file (best effort — a failed removal
		// still yields "not armed", which is the safe answer) so it can't
		// arm a future reader.
		_ = os.Remove(path)
		return false
	}
	return true
}
