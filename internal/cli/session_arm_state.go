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
	// Disarmed makes the file TRI-STATE (PLAN-2613 S3). S2 had two states:
	// a present (armed) file or none. S3 adds an explicit-OFF value so a
	// within-session `pad session disarm` can withdraw consent even in an
	// auto_arm=true repo — the disconnect verb must not be a lie there. A
	// file with Disarmed=true is a session-scoped explicit OFF that BEATS
	// auto_arm for its owner session, under the SAME liveness rules as an
	// armed file: it dies with the session (reaped when the owner is dead),
	// so across sessions auto_arm remains the standing contract (permanent
	// off is a .pad.toml edit, symmetric with how it turned on). When
	// Disarmed is true the Armed marker is false; presence with a live
	// owner still identifies the session, the Disarmed flag decides the
	// meaning. See SessionArmState.
	Disarmed bool `json:"disarmed,omitempty"`
	// Booted records that this session has already had its first-connect
	// boot ritual fired (PLAN-2613 D8): /pad:connect injects the
	// workspace's on-session-start playbooks on the FIRST connect only, and
	// re-arms/reconnects must not re-fire it. It is per-session state that
	// dies with the session (the whole file is reaped when the owner dies),
	// and it is carried forward across arm/disarm rewrites so toggling
	// consent doesn't reset the boot flag. See MarkFirstConnect.
	Booted bool `json:"booted,omitempty"`
	// PID is the process that wrote the file. For a socket-keyed session
	// it is informational (the socket is the liveness signal); for the
	// headless fallback it IS the liveness signal.
	PID int `json:"pid"`
	// Socket is CLAUDE_CODE_MESSAGING_SOCKET at arm time, or "" for the
	// headless fallback. When set, its continued existence on disk is a
	// necessary liveness signal — it outlives the short-lived arm command
	// and vanishes with the Claude Code session.
	Socket string `json:"socket,omitempty"`
	// SocketMtimeUnixNano is the socket file's modification time at arm
	// time (0 when headless). Existence alone is not enough: a socket
	// PATH can be reused by a later, unrelated Claude Code session, and a
	// stale arm file keyed on that path would then arm the new session —
	// consent-grandfathering (Codex R1 HIGH-2). The socket file's mtime is
	// its bind (creation) time, so a reused path is a DIFFERENT socket
	// with a different mtime; the reader requires an exact match, which
	// ties the file to the specific socket instance it was armed for.
	SocketMtimeUnixNano int64 `json:"socket_mtime_unix_nano,omitempty"`
	// SocketIno / SocketDev are the socket node's inode and device at arm
	// time (0 on platforms where they can't be read). They are the
	// STRONGEST identity signal: a socket rebound at the same path gets a
	// new inode, so this rejects both a lingering stale node reused as-is
	// and an mtime collision on a coarse-resolution filesystem (Codex R2
	// finding 2). Where unavailable (non-unix), the reader falls back to
	// the mtime check alone — the documented residual on those platforms.
	SocketIno uint64 `json:"socket_ino,omitempty"`
	SocketDev uint64 `json:"socket_dev,omitempty"`
	// ProcStart is an opaque owner-identity token for the HEADLESS
	// fallback (empty when socket-keyed, or when the platform can't supply
	// one). It disambiguates PID reuse: a bare "is this pid alive" check
	// can't tell the original arming process from an unrelated later
	// process that happens to reuse its pid. On Linux this is the
	// process's start time from /proc; where unavailable it is empty and
	// the reader falls back to bare pid-liveness (the documented residual
	// on the secondary path — see armStateOwnerAlive).
	ProcStart string `json:"proc_start,omitempty"`
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

// WriteArmState arms the current session (an explicit ON marker). See
// writeArmStateFile.
func WriteArmState() (path string, err error) {
	return writeArmStateFile(false)
}

// WriteDisarmState writes a session-scoped explicit-OFF marker (PLAN-2613
// S3): the session withdraws consent for its remaining life, beating a
// repo's auto_arm opt-in, and the marker dies with the session under the
// same liveness rules (see ArmState.Disarmed). This is what `pad session
// disarm` writes — it does NOT remove the file, because absence means
// "resolve auto_arm", which in an auto_arm repo would immediately re-arm.
func WriteDisarmState() (path string, err error) {
	return writeArmStateFile(true)
}

// writeArmStateFile writes (or idempotently overwrites) this session's
// arm-state file, mode 0600, with the given disarmed value, and returns
// the path. It refuses only when there is no key to write under at all —
// no messaging socket AND no working directory — because a file with
// neither could never be matched to a session or a repo by a reader.
func writeArmStateFile(disarmed bool) (path string, err error) {
	socket, cwd := currentSessionArmKey()
	if socket == "" && cwd == "" {
		return "", fmt.Errorf("cannot record arm state: no messaging socket and no working directory to key the session on")
	}
	p, err := armStatePath(socket, cwd)
	if err != nil {
		return "", err
	}
	// Carry the boot flag forward across arm/disarm rewrites so toggling
	// consent within a session doesn't re-fire the first-connect ritual
	// (D8). Only a LIVE prior file counts — a dead-owner one is a stale
	// session and its boot state is irrelevant.
	booted := false
	if prev, _, rerr := readArmState(); rerr == nil && prev != nil && armStateOwnerAlive(prev) {
		booted = prev.Booted
	}
	st := ArmState{
		Armed:     !disarmed,
		Disarmed:  disarmed,
		Booted:    booted,
		PID:       os.Getpid(),
		Socket:    socket,
		Cwd:       cwd,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if socket != "" {
		// Bind the file to this specific socket instance — its mtime plus,
		// where available, its inode+device — not just the path, so a
		// reused path (or a lingering stale node) can't revive it (HIGH-2,
		// R2 finding 2).
		if info, statErr := os.Stat(socket); statErr == nil {
			st.SocketMtimeUnixNano = info.ModTime().UnixNano()
			if ino, dev, ok := statIdentity(info); ok {
				st.SocketIno, st.SocketDev = ino, dev
			}
		}
	} else {
		// Headless: record an owner-identity token so pid reuse can't
		// revive this file (best effort — empty where unsupported).
		st.ProcStart, _ = procStartToken(st.PID)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal arm state: %w", err)
	}
	// Atomic write: a reader must never observe a half-written file (a
	// partial file would be an unparseable "malformed" state, and now that
	// readers reap malformed files, a torn write could be reaped mid-arm).
	// Write to a temp file in the same directory, then rename — rename is
	// atomic within a filesystem.
	if err := atomicWriteFile(p, data, 0600); err != nil {
		return "", fmt.Errorf("write arm state: %w", err)
	}
	return p, nil
}

// atomicWriteFile writes data to a temp file in path's directory and
// renames it into place, so a concurrent reader sees either the old file
// or the complete new one, never a partial write.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".arm-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
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
// 2), hardened against owner-identity reuse (Codex R1 HIGH-2). A stale
// armed file must never arm a future monitor, so "alive" means the
// recorded owner is not merely present but the SAME owner:
//
//   - Socket-keyed: the socket path must still exist AND its recorded
//     identity must match — inode+device where available (the strongest
//     signal: a rebound socket gets a new inode), else the socket's
//     mtime. A reused path, a lingering stale node, or an mtime collision
//     are all rejected. A file that recorded no mtime (only possible if
//     the socket had vanished at arm time) can't prove identity and is
//     dead.
//   - Headless: the pid must be alive AND, when an owner-identity token
//     was RECORDED, it must be re-readable now AND match — so a reused pid
//     (or one whose /proc entry we can no longer verify) is rejected
//     rather than trusted. Only a file that recorded NO token (a non-Linux
//     arm) falls back to bare pid-liveness: the documented residual on the
//     secondary path (the sanctioned headless arming path is auto_arm, not
//     this file).
//
// Anything uncertain is dead (fail closed).
func armStateOwnerAlive(st *ArmState) bool {
	if st == nil {
		return false
	}
	if st.Socket != "" {
		info, err := os.Stat(st.Socket)
		if err != nil {
			return false // socket vanished — session gone
		}
		if st.SocketMtimeUnixNano == 0 {
			return false // no identity recorded — can't prove it's ours
		}
		// Prefer inode+device when both the file recorded them and the
		// current node exposes them: it distinguishes a rebound socket at
		// the same path from the original. Fall back to mtime only when
		// identity isn't available on this platform.
		if st.SocketIno != 0 {
			if ino, dev, ok := statIdentity(info); ok {
				return ino == st.SocketIno && dev == st.SocketDev
			}
		}
		return info.ModTime().UnixNano() == st.SocketMtimeUnixNano
	}
	// Headless fallback: the pid is the owner we have. A pid <= 0 is never
	// a live owner; a short-lived arm command's pid will usually be gone
	// by the time a reader looks, which is why this path is documented as
	// secondary to auto_arm.
	if st.PID <= 0 || !pidAlive(st.PID) {
		return false
	}
	if st.ProcStart != "" {
		// A token was recorded (a Linux arm), so it must be re-readable and
		// match. If it can't be read now (pid gone from /proc, or a zombie —
		// procStartToken reports not-ok for state 'Z'), fail closed rather
		// than trusting bare pid-liveness, which a reused pid would pass
		// (Codex R2 finding 3).
		now, ok := procStartToken(st.PID)
		return ok && now == st.ProcStart
	}
	return true
}

// LocalArmState is the tri-state of THIS session's local arm-state file
// (PLAN-2613 S3): a live explicit ON, a live explicit OFF, or absent (no
// live local override, so the auto_arm config decides).
type LocalArmState int

const (
	// LocalArmAbsent: no local override file — resolve auto_arm.
	LocalArmAbsent LocalArmState = iota
	// LocalArmOn: a live explicit `pad session arm` — announce armed.
	LocalArmOn
	// LocalArmOff: a live explicit `pad session disarm` — announce NOT
	// armed, beating a repo's auto_arm for this session.
	LocalArmOff
	// LocalArmError: a local-state file EXISTS but can't be read or parsed.
	// It resolves to NOT armed and does NOT fall back to auto_arm (Codex R1
	// S3 HIGH-2): a corrupt file could be a disarm we can't read, and
	// re-arming despite a possible disarm is the consent fail-open this
	// gate exists to prevent. Distinct from LocalArmAbsent (no file at
	// all), which safely resolves auto_arm.
	LocalArmError
)

// SessionArmState reads THIS session's local arm-state file and returns
// its tri-state. It applies the mandatory liveness check and reaps a
// dead-owner or malformed file, so a crashed session's override — armed OR
// disarmed — can't linger (constraint 2). It never errors: every failure
// path is LocalArmAbsent, which falls back to auto_arm, because this gates
// consent and must fail to the safe, config-decided answer.
//
// This is the reader half of the file contract. The full announced value
// (folding in auto_arm) is ResolveAnnouncedArmed — see cmd/pad's monitor
// wiring.
func SessionArmState() LocalArmState {
	st, path, err := readArmState()
	if err != nil {
		// A file exists but can't be read/parsed. Fail CLOSED and do NOT
		// reap: reaping would make the NEXT read see "absent" and fall back
		// to auto_arm, re-arming despite a possible-but-unreadable disarm
		// (Codex R1 S3 HIGH-2). Leaving it keeps the session consistently
		// not-armed until a re-arm/disarm overwrites it; it is session-keyed
		// and dies with the session anyway.
		return LocalArmError
	}
	if st == nil {
		return LocalArmAbsent
	}
	if !armStateOwnerAlive(st) {
		// Dead owner: reap the stale file so it can't override a future
		// session (constraint 2). Absent falls back to auto_arm — which is
		// exactly the "across sessions, auto_arm remains the contract"
		// ruling for a disarmed file too.
		reapArmFile(path)
		return LocalArmAbsent
	}
	if st.Disarmed {
		return LocalArmOff
	}
	return LocalArmOn
}

// SessionArmedLocally reports whether this session has a live explicit ARM
// (LocalArmOn). Retained for callers that only ask the ON question; the
// full tri-state, including an explicit OFF that beats auto_arm, is
// SessionArmState.
func SessionArmedLocally() bool {
	return SessionArmState() == LocalArmOn
}

// ResolveAnnouncedArmed is the full S3 resolution of what a stream this
// session opens should announce for ?armed (PLAN-2613 S3): a live explicit
// local override wins — ON arms, OFF does not (beating auto_arm for this
// session) — and only in the absence of a live override does the repo's
// auto_arm config decide.
func ResolveAnnouncedArmed() bool {
	switch SessionArmState() {
	case LocalArmOn:
		return true
	case LocalArmOff, LocalArmError:
		// OFF and ERROR both suppress arming: an explicit disarm wins, and
		// an unreadable local state fails closed rather than falling to
		// auto_arm (HIGH-2).
		return false
	default: // LocalArmAbsent
		return ResolveAutoArmFromDisk().Armed
	}
}

// MarkFirstConnect reports whether THIS session's first-connect boot ritual
// should run now, and marks it done so a later call (a reconnect) returns
// false (PLAN-2613 D8). Idempotent per session: the first live call returns
// true, every later one false; the flag dies with the session.
//
// It operates on the arm-state file, which /pad:connect writes before it
// boots (connect arms first). If no live arm-state file exists — an
// unexpected call order — it returns true WITHOUT persisting: skipping a
// boot ritual silently is worse than running it twice (the playbooks are
// meant to be re-runnable), so this fails toward running it.
func MarkFirstConnect() (first bool, err error) {
	st, path, rerr := readArmState()
	if rerr != nil || st == nil || !armStateOwnerAlive(st) {
		return true, nil
	}
	if st.Booted {
		return false, nil
	}
	st.Booted = true
	data, merr := json.MarshalIndent(st, "", "  ")
	if merr != nil {
		return true, fmt.Errorf("marshal arm state: %w", merr)
	}
	if werr := atomicWriteFile(path, data, 0600); werr != nil {
		return true, fmt.Errorf("write arm state: %w", werr)
	}
	return true, nil
}

// reapArmFile removes a stale arm file, but only after RE-READING it and
// confirming it is still stale — so a concurrent `pad session arm` that
// rewrote a fresh, live file between our first read and now is not
// destroyed (a non-destructive reap; Codex R1 MED-1). A tiny window
// remains between this re-check and the removal, but both outcomes are
// safe: at worst a just-armed session is disarmed and must re-arm (fail
// closed), never the reverse. Best effort throughout — a lingering file
// is simply re-evaluated on the next read.
func reapArmFile(path string) {
	if st, _, err := readArmState(); err == nil && st != nil && armStateOwnerAlive(st) {
		return // someone re-armed with a live owner — leave it
	}
	_ = os.Remove(path)
}
