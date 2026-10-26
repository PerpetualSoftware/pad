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
	errProcZombie        = errors.New("process is a zombie")
	errProcStatMalformed = errors.New("/proc stat is malformed")
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
	// LivenessUnknown: the owner cannot be examined from here — this
	// platform cannot probe it, or it lives in another PID namespace
	// (BUG-4090). Not dead — a reaper must not act on it — and not alive —
	// a gate must not trust it.
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
	// PIDVerified is true when the claimed pid was checked against this
	// process's ancestry at capture time (pidIsSelfOrAncestor — Linux) or
	// is this process itself. A harness-supplied pid is otherwise a bare
	// claim: any positive integer names SOME process, and the start token
	// only guards against later reuse, not against a wrong pid in the
	// first place (codex round 3). False on platforms without an ancestry
	// walk, and for legacy rows.
	PIDVerified bool `json:"session_pid_verified,omitempty"`
	// ProcStart is the owner's process start token (procStartToken) when
	// the platform supplies one — the pid-reuse defence. Empty elsewhere.
	ProcStart string `json:"proc_start,omitempty"`
	// PIDNamespace is the inode of the PID namespace PID was captured in
	// (currentPIDNamespace — Linux), recorded only when PID named a process
	// visible there. A reader in a different namespace cannot examine the
	// pid at all, and must say unknown rather than dead (BUG-4090). Zero
	// off Linux, in rows written before it existed, and for a harness pid
	// inherited from outside a sandbox, which the recorder cannot see.
	// An additive on-disk key: older readers ignore it, and a row without
	// it is judged by pidNamespaceRelation's unstamped rule.
	PIDNamespace uint64 `json:"pid_ns,omitempty"`
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
	if o.PIDSource == "self" {
		o.PIDVerified = true
	} else {
		// A wrong-but-live pid must not be recorded as if it were checked;
		// an unreadable /proc leaves it unverified too.
		o.PIDVerified, _ = pidIsSelfOrAncestor(o.PID)
	}
	// Best effort: empty where the platform has no token, and empty when
	// the pid cannot be read (a harness pid we cannot see is recorded
	// without a token and judged by bare liveness, the documented residual).
	o.ProcStart, _ = procStartToken(o.PID)
	// The namespace is recorded only for a pid this namespace can signal:
	// kill(2) resolves pids in the caller's namespace, so a pid it cannot
	// reach belongs to some other one, and stamping ours on it would be a
	// claim the record cannot back (BUG-4090).
	if ns, err := pidNamespaceFn(); err == nil && pidAlive(o.PID) {
		o.PIDNamespace = ns
	}

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
//
// PID NAMESPACES (BUG-4090). A pid means something only inside one PID
// namespace, and a reader in another one (a bwrap sandbox, a container)
// gets ESRCH for every pid outside it. So the reader's namespace is
// compared with the record's first (pidNamespaceRelation), and where they
// differ — or may differ — absence is not evidence: neither the pid probe
// nor a missing socket can say dead, since a pid namespace says nothing
// about whether the reader sees the recorder's /run either. The one dead
// verdict that survives the boundary is a socket that answers with a
// DIFFERENT identity: that is positive evidence of another session.
func OwnerLiveness(o *SessionOwner) Liveness {
	if o == nil || (o.Socket == "" && o.PID <= 0) {
		return LivenessDead
	}
	rel := pidNamespaceRelation(o)
	verdict := LivenessAlive
	if o.Socket != "" {
		switch socketLiveness(o, rel) {
		case LivenessDead:
			return LivenessDead
		case LivenessUnknown:
			verdict = LivenessUnknown
		}
	}
	if o.PID > 0 {
		switch pidLiveness(o, rel) {
		case LivenessDead:
			return LivenessDead
		case LivenessUnknown:
			verdict = LivenessUnknown
		}
	}
	return verdict
}

// pidNSRelation is how the reader's PID namespace stands to the one a
// record's pid was captured in (BUG-4090).
type pidNSRelation int

const (
	// pidNSSame: the same namespace, or no reason to doubt it (an
	// unstamped row read from the init namespace, or a platform without
	// namespaces). Every probe means what it says.
	pidNSSame pidNSRelation = iota
	// pidNSCrossed: the record names a namespace and the reader is in a
	// different one, or cannot read its own to compare. The pid cannot be
	// examined at all.
	pidNSCrossed
	// pidNSUnstampedForeign: the record names no namespace (written before
	// pid_ns existed, or for a pid the recorder could not see) and the
	// reader is not in the init namespace, so the boundary may or may not
	// be there. A positive pid probe still stands, token-checked; a
	// negative one is not evidence (lead ruling, option (a)).
	pidNSUnstampedForeign
)

// pidNamespaceFn is currentPIDNamespace behind a seam, so the namespace
// cases are testable from a single namespace — and so this package's
// tests, which build owners by hand without a pid_ns, keep one answer when
// `go test` itself runs in a sandbox (the Nix build does). See TestMain.
var pidNamespaceFn = currentPIDNamespace

func pidNamespaceRelation(o *SessionOwner) pidNSRelation {
	reader, err := pidNamespaceFn()
	if o.PIDNamespace != 0 {
		if err != nil || reader != o.PIDNamespace {
			return pidNSCrossed
		}
		return pidNSSame
	}
	if err == nil && reader != initPIDNamespace {
		return pidNSUnstampedForeign
	}
	return pidNSSame
}

// socketLiveness judges the recorded socket instance. Across a possible
// namespace boundary only a socket that answers with a different identity
// is dead; a missing or unidentifiable one is unknown (see OwnerLiveness).
func socketLiveness(o *SessionOwner, rel pidNSRelation) Liveness {
	absent := LivenessDead
	if rel != pidNSSame {
		absent = LivenessUnknown
	}
	info, err := os.Stat(o.Socket)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return absent // socket vanished — session gone, if we see its /run
		}
		return LivenessUnknown // could not examine (EACCES, EIO, ...)
	}
	if o.SocketMtimeUnixNano == 0 {
		return absent // no identity recorded — cannot prove it is ours
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
func pidLiveness(o *SessionOwner, rel pidNSRelation) Liveness {
	if runtime.GOOS == "windows" {
		// pidAlive cannot probe here (Signal(0) is unsupported), so a
		// verdict either way would be invented.
		return LivenessUnknown
	}
	if rel == pidNSCrossed {
		// The pid names a process in another namespace: probing it here
		// would ask about whatever this namespace has at that number.
		return LivenessUnknown
	}
	absent := LivenessDead
	if rel == pidNSUnstampedForeign {
		absent = LivenessUnknown
	}
	if !pidAlive(o.PID) {
		return absent
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
