package cli

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
)

// Session owner identity (TASK-2767, IDEA-2750 part 2).
//
// Two local files describe "the session this process belongs to": the
// arm-state file (session_arm_state.go) and the session registry
// (session_registry.go). Both must answer the same question — is the
// OWNER of this record still alive, and is it the SAME owner that wrote
// it — and they used to answer it in two places with two different
// vocabularies. SessionOwner is the one identity they share, and
// OwnerLiveness is the one verdict.
//
// WHAT "OWNER" MEANS. The process a short-lived `pad` command was run
// FROM — an agent harness's session process — not the `pad` command
// itself, whose pid is dead before anyone reads the record. The harness
// names it: Claude Code exports CLAUDE_PID; any other harness can export
// PAD_SESSION_PID (the runtime-agnostic override, exactly as PAD_AGENT is
// for the agent name). With neither, the owner is this process — correct
// for a long-lived `pad` process such as a monitor, and the documented
// residual for a bare shell.
//
// WHY LIVENESS IS TRI-STATE. A consent gate and a reaper need opposite
// defaults. A consent gate treats anything uncertain as dead (a stale file
// must never arm a future session — PLAN-2613 constraint 2). A reaper
// treating uncertain as dead would delete every live record on a platform
// where pids cannot be probed (pidAlive reports dead for every pid on
// Windows). So the verdict carries its uncertainty, and each consumer
// applies its own posture: armStateOwnerAlive is `== LivenessAlive`;
// PruneSessions deletes only LivenessDead and leaves LivenessUnknown to
// an explicit age bound.

// Liveness is the verdict on a recorded session owner.
type Liveness string

const (
	// LivenessAlive: the recorded owner exists now AND its identity matches
	// what was recorded (socket inode/mtime, or pid + process start token).
	LivenessAlive Liveness = "alive"
	// LivenessDead: the owner is gone, or the thing at its address is a
	// different owner (a rebound socket, a reused pid).
	LivenessDead Liveness = "dead"
	// LivenessUnknown: this platform cannot probe the owner at all. Not
	// dead — a reaper must not act on it — and not alive — a gate must not
	// trust it.
	LivenessUnknown Liveness = "unknown"
)

// SessionOwner identifies the session a record belongs to, with enough
// identity to tell a reused address from the original. JSON tags are the
// session registry's on-disk keys (messaging_socket_path is the v1 key,
// kept so legacy files still parse).
type SessionOwner struct {
	// PID is the owner process. Zero in a legacy registry file.
	PID int `json:"session_pid,omitempty"`
	// PIDSource records where PID came from: "PAD_SESSION_PID", "CLAUDE_PID",
	// or "self" (os.Getpid()). Legacy records derive one — see
	// legacyOwner. Recorded because a self-keyed record from a short-lived
	// command is dead by the time anyone reads it, and a reader should be
	// able to see that this is why.
	PIDSource string `json:"session_pid_source,omitempty"`
	// ProcStart is the owner's process start token (procStartToken) when
	// the platform supplies one — the pid-reuse defence. Empty elsewhere.
	ProcStart string `json:"proc_start,omitempty"`
	// Socket is CLAUDE_CODE_MESSAGING_SOCKET at capture time, recorded only
	// when the socket existed then (its identity below is what makes it a
	// liveness signal; a path alone proves nothing).
	Socket string `json:"messaging_socket_path,omitempty"`
	// SocketMtimeUnixNano, SocketIno, SocketDev bind the record to the
	// specific socket INSTANCE — the same identity the arm-state file uses
	// (Codex R1 HIGH-2 / R2 finding 2 on PLAN-2613 S2): a socket rebound at
	// the same path is a different session.
	SocketMtimeUnixNano int64  `json:"socket_mtime_unix_nano,omitempty"`
	SocketIno           uint64 `json:"socket_ino,omitempty"`
	SocketDev           uint64 `json:"socket_dev,omitempty"`
}

// CaptureSessionOwner reads this process's session owner from the
// environment. Precedence for the pid, most explicit first:
//
//  1. $PAD_SESSION_PID — the harness-agnostic override.
//  2. $CLAUDE_PID — Claude Code's own export (verified against a live
//     session: present in the tool shell AND in the plugin monitor's
//     environment).
//  3. os.Getpid() — this process.
//
// A set-but-invalid value is an error, not a fall-through: a harness that
// exports garbage would otherwise key every record on a dead command pid
// while reporting success, which is the silent misregistration this whole
// unit exists to end.
func CaptureSessionOwner() (SessionOwner, error) {
	o := SessionOwner{PID: os.Getpid(), PIDSource: "self"}
	for _, env := range []string{"PAD_SESSION_PID", "CLAUDE_PID"} {
		v := os.Getenv(env)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return SessionOwner{}, fmt.Errorf("%s=%q is not a positive integer", env, v)
		}
		o.PID, o.PIDSource = n, env
		break
	}
	// Best effort: empty where the platform has no token, and empty when
	// the pid cannot be read (a harness pid we cannot see is recorded
	// without a token and judged by bare liveness, the documented residual).
	o.ProcStart, _ = procStartToken(o.PID)

	if sock := os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"); sock != "" {
		if info, err := os.Stat(sock); err == nil {
			o.Socket = sock
			o.SocketMtimeUnixNano = info.ModTime().UnixNano()
			if ino, dev, ok := statIdentity(info); ok {
				o.SocketIno, o.SocketDev = ino, dev
			}
		}
	}
	return o, nil
}

// OwnerLiveness is the shared verdict. The socket, when recorded, decides
// on its own: it outlives every short-lived command and vanishes with the
// harness session, and its inode/mtime identity rejects a reused path. A
// record with no socket falls to the pid: on Windows pids cannot be
// probed, so the answer is unknown; elsewhere a dead pid is dead, a live
// pid with a recorded start token must re-read and match (a reused pid is
// a different owner), and a live pid with no token recorded is alive on
// bare liveness — the documented residual on platforms without a token.
//
// Nothing here is "alive because we cannot tell": every uncertain case is
// dead or unknown, and the two consumers choose how to treat unknown.
func OwnerLiveness(o *SessionOwner) Liveness {
	if o == nil {
		return LivenessDead
	}
	if o.Socket != "" {
		info, err := os.Stat(o.Socket)
		if err != nil {
			return LivenessDead // socket vanished — session gone
		}
		if o.SocketMtimeUnixNano == 0 {
			return LivenessDead // no identity recorded — cannot prove it is ours
		}
		if o.SocketIno != 0 {
			if ino, dev, ok := statIdentity(info); ok {
				if ino == o.SocketIno && dev == o.SocketDev {
					return LivenessAlive
				}
				return LivenessDead
			}
		}
		if info.ModTime().UnixNano() == o.SocketMtimeUnixNano {
			return LivenessAlive
		}
		return LivenessDead
	}
	if o.PID <= 0 {
		return LivenessDead
	}
	if runtime.GOOS == "windows" {
		// pidAlive cannot probe here (Signal(0) is unsupported), so a
		// verdict either way would be invented. See the package comment
		// for why this is unknown rather than dead.
		return LivenessUnknown
	}
	if !pidAlive(o.PID) {
		return LivenessDead
	}
	if o.ProcStart != "" {
		now, ok := procStartToken(o.PID)
		if !ok || now != o.ProcStart {
			return LivenessDead // gone from /proc, a zombie, or a reused pid
		}
		return LivenessAlive
	}
	return LivenessAlive
}
