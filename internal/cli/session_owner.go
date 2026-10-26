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
	// platform cannot probe it, or it lives in another PID or mount namespace
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
	// inherited from outside a sandbox, which the recorder cannot see. A
	// Linux recorder that cannot read its own namespace (no procfs) writes
	// unidentifiedNamespace instead, which no reader matches (r3, F2).
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
	// MountNamespace is the inode of the mount namespace the socket was
	// seen in (currentMountNamespace — Linux), recorded only when the
	// recorder could stat the socket and read its identity (lead ruling C1,
	// mirroring PIDNamespace's visibility rule). A path's absence, or a
	// different node at it, is evidence only to a reader with the same view
	// of the filesystem; elsewhere the socket verdict is unknown (BUG-4090
	// r1, H1/H3). unidentifiedNamespace when Linux would not name it (r3,
	// F2). An additive on-disk key; a row without it keeps the
	// pre-BUG-4090 socket rule.
	MountNamespace uint64 `json:"mnt_ns,omitempty"`
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
	if pidAlive(o.PID) {
		o.PIDNamespace = namespaceStamp(pidNamespaceFn)
	}

	if sock := os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"); sock != "" {
		if info, err := os.Stat(sock); err == nil {
			o.Socket = sock
			o.SocketMtimeUnixNano = info.ModTime().UnixNano()
			if ino, dev, ok := statIdentity(info); ok {
				o.SocketIno, o.SocketDev = ino, dev
				o.MountNamespace = namespaceStamp(mountNamespaceFn)
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
// NAMESPACES (BUG-4090). A pid means something only inside one PID
// namespace, and a reader in another one (a bwrap sandbox, a container)
// gets ESRCH for every pid outside it; a path means something only inside
// one mount namespace, and a reader with another view of the filesystem
// may see a missing socket, or a different one, where the session's own
// is live. So each signal is judged against the namespace that gives it
// meaning — the pid against pid_ns (pidNamespaceRelation), the socket
// against mnt_ns (mountNamespaceCrossed) — and where the reader's differs,
// absence and mismatch are not evidence: the verdict is unknown.
func OwnerLiveness(o *SessionOwner) Liveness {
	if o == nil || (o.Socket == "" && o.PID <= 0) {
		return LivenessDead
	}
	rel := pidNamespaceRelation(o)
	verdict := LivenessAlive
	if o.Socket != "" {
		switch socketLiveness(o, mountNamespaceCrossed(o)) {
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
	// be there. Only a token match stands (alive); every negative —
	// ESRCH, a token mismatch, ENOENT, a zombie — is unknown (lead
	// ruling, option (a), as corrected at r2).
	pidNSUnstampedForeign
)

// pidNamespaceFn is currentPIDNamespace behind a seam, so the namespace
// cases are testable from a single namespace — and so this package's
// tests, which build owners by hand without a pid_ns, keep one answer when
// `go test` itself runs in a sandbox (the Nix build does). See TestMain.
var pidNamespaceFn = currentPIDNamespace

// mountNamespaceFn is currentMountNamespace behind the same kind of seam.
var mountNamespaceFn = currentMountNamespace

// unidentifiedNamespace is the stamp a Linux recorder writes when it could
// not read its own namespace (no procfs): "recorded somewhere I could not
// name". nsfs inodes come from the proc inum allocator and are never 1, so
// it equals no reader's namespace and every reader treats the row as
// crossed (BUG-4090 r3, F2).
const unidentifiedNamespace uint64 = 1

// namespaceStamp is the value a writer records for the namespace it just
// observed something in: the namespace's inode, the unidentified sentinel
// when Linux will not say, or zero (no stamp) off Linux.
func namespaceStamp(fn func() (uint64, error)) uint64 {
	if ns, err := fn(); err == nil {
		return ns
	}
	if namespacesSupported {
		return unidentifiedNamespace
	}
	return 0
}

func pidNamespaceRelation(o *SessionOwner) pidNSRelation {
	reader, err := pidNamespaceFn()
	if o.PIDNamespace != 0 {
		if err != nil || reader != o.PIDNamespace {
			return pidNSCrossed
		}
		return pidNSSame
	}
	if err != nil {
		// A reader that cannot name its own namespace may be in any of them
		// on Linux (F1); off Linux there is nothing to be across.
		if namespacesSupported {
			return pidNSUnstampedForeign
		}
		return pidNSSame
	}
	if reader != initPIDNamespace {
		return pidNSUnstampedForeign
	}
	return pidNSSame
}

// mountNamespaceCrossed reports whether the reader may not share the
// filesystem view the socket was recorded in: the record names a mount
// namespace and the reader is in another one, or cannot read its own. A
// row without mnt_ns (written before it existed, or off Linux) is judged
// as it always was — the disclosed residual for pre-fix rows read across a
// mount boundary, transient because sessions re-register on first use.
func mountNamespaceCrossed(o *SessionOwner) bool {
	reader, err := mountNamespaceFn()
	if o.MountNamespace == 0 {
		// The pre-fix rule — unless this reader cannot name its own mount
		// namespace, which on Linux makes it a possible boundary (F1).
		return err != nil && namespacesSupported
	}
	return err != nil || reader != o.MountNamespace
}

// socketLiveness judges the recorded socket instance. Across a mount
// namespace boundary only a socket that answers with the RECORDED identity
// says anything (alive); a missing, unidentifiable or different node is
// unknown, since the reader may simply be looking at another filesystem.
func socketLiveness(o *SessionOwner, crossed bool) Liveness {
	negative := LivenessDead
	if crossed {
		negative = LivenessUnknown
	}
	info, err := os.Stat(o.Socket)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return negative // socket vanished — session gone, if we see its filesystem
		}
		return LivenessUnknown // could not examine (EACCES, EIO, ...)
	}
	if o.SocketMtimeUnixNano == 0 {
		return negative // no identity recorded — cannot prove it is ours
	}
	if o.SocketIno != 0 {
		if ino, dev, ok := statIdentity(info); ok {
			if ino == o.SocketIno && dev == o.SocketDev {
				return LivenessAlive
			}
			return negative // rebound at the same path — a different session
		}
	}
	if info.ModTime().UnixNano() == o.SocketMtimeUnixNano {
		return LivenessAlive
	}
	return negative
}

// pidLiveness judges the recorded owner pid. A live pid with no token
// recorded is alive on bare liveness — the documented residual for records
// written where no start token exists (non-Linux) and for legacy rows,
// which never had one; pid reuse cannot be excluded there. That residual
// holds only for a reader in the recorder's namespace or the init one: a
// reader that may be elsewhere has no token to match and says unknown.
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
	// A reader that may be in another namespace never manufactures dead:
	// ESRCH, a different process at the number, or one that vanished or is
	// a zombie all describe whatever THIS namespace holds there. Only a
	// token match — positive evidence — stands (lead r2 ruling on (a)).
	negative := LivenessDead
	if rel == pidNSUnstampedForeign {
		negative = LivenessUnknown
	}
	if !pidAlive(o.PID) {
		return negative
	}
	if o.ProcStart == "" {
		if rel == pidNSUnstampedForeign {
			// No token, so nothing to match: whatever answers at this number
			// here may be any process at all (r4 ruling, A-1).
			return LivenessUnknown
		}
		return LivenessAlive
	}
	now, err := procStartTokenErr(o.PID)
	switch {
	case err == nil:
		if now == o.ProcStart {
			return LivenessAlive
		}
		return negative // a reused pid — a different process
	case errors.Is(err, os.ErrNotExist), errors.Is(err, errProcZombie):
		return negative // gone between the signal probe and the read, or exited awaiting reap

	default:
		return LivenessUnknown // /proc unreadable or unsupported here — not evidence of absence
	}
}
