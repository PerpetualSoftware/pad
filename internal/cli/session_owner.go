package cli

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
)

// Probe failures that mean "the owner is GONE" (as opposed to "the owner
// could not be examined"). See pidLiveness.
var (
	errProcZombie           = errors.New("process is a zombie")
	errProcStatMalformed    = errors.New("/proc stat is malformed")
	errProcStartUnsupported = errors.New("process start token unsupported on this platform")
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

// OwnerLiveness is the shared verdict. EVERY recorded signal must agree:
// the socket (when recorded) must still exist with the identity captured
// at registration — inode/device, else mtime, so a reused path is a
// different session — AND the pid (when recorded) must be alive and, when
// a start token was recorded, still carry it. Either signal dead → dead.
// Neither alone suffices for the registry: a socket node outlives a
// SIGKILLed owner (the kernel does not unlink it), so a socket-only verdict
// would report a crashed harness alive indefinitely (codex round 1 P1);
// and a pid alone cannot exclude reuse on platforms without a start token.
//
// Unknown is the third answer, and it is reserved for "could not examine",
// never "not sure it is alive": a platform that cannot probe pids
// (Windows), a socket the caller cannot stat (a permission or I/O error,
// as opposed to ENOENT), or a /proc entry that cannot be read (hidepid, a
// namespace boundary). A dead verdict is only ever issued on positive
// evidence of absence or of a different owner (P2). The two consumers
// then choose: the consent gate treats unknown as not-armed, the pruner
// leaves it alone.
//
// A record with NO signal at all (no socket, no pid) is dead.
func OwnerLiveness(o *SessionOwner) Liveness {
	if o == nil || (o.Socket == "" && o.PID <= 0) {
		return LivenessDead
	}
	verdict := LivenessAlive
	if o.Socket != "" {
		switch socketLiveness(o) {
		case LivenessDead:
			return LivenessDead
		case LivenessUnknown:
			verdict = LivenessUnknown
		}
	}
	if o.PID > 0 {
		switch pidLiveness(o) {
		case LivenessDead:
			return LivenessDead
		case LivenessUnknown:
			verdict = LivenessUnknown
		}
	}
	return verdict
}

// socketLiveness judges the recorded socket instance.
func socketLiveness(o *SessionOwner) Liveness {
	info, err := os.Stat(o.Socket)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LivenessDead // socket vanished — session gone
		}
		return LivenessUnknown // could not examine (EACCES, EIO, ...)
	}
	if o.SocketMtimeUnixNano == 0 {
		return LivenessDead // no identity recorded — cannot prove it is ours
	}
	if o.SocketIno != 0 {
		if ino, dev, ok := statIdentity(info); ok {
			if ino == o.SocketIno && dev == o.SocketDev {
				return LivenessAlive
			}
			return LivenessDead // rebound at the same path — a different session
		}
	}
	if info.ModTime().UnixNano() == o.SocketMtimeUnixNano {
		return LivenessAlive
	}
	return LivenessDead
}

// pidLiveness judges the recorded owner pid. A live pid with no token
// recorded is alive on bare liveness — the documented residual for records
// written where no start token exists (non-Linux) and for legacy rows,
// which never had one; pid reuse cannot be excluded there.
func pidLiveness(o *SessionOwner) Liveness {
	if runtime.GOOS == "windows" {
		// pidAlive cannot probe here (Signal(0) is unsupported), so a
		// verdict either way would be invented.
		return LivenessUnknown
	}
	if !pidAlive(o.PID) {
		return LivenessDead
	}
	if o.ProcStart == "" {
		return LivenessAlive
	}
	now, err := procStartTokenErr(o.PID)
	switch {
	case err == nil:
		if now == o.ProcStart {
			return LivenessAlive
		}
		return LivenessDead // a reused pid — a different process
	case errors.Is(err, os.ErrNotExist), errors.Is(err, errProcZombie):
		return LivenessDead // gone between the signal probe and the read, or exited awaiting reap
	default:
		return LivenessUnknown // /proc unreadable or unsupported here — not evidence of absence
	}
}
