//go:build linux

package cli

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// pidnsHelperOutput is what the reader inside the namespace reports back.
type pidnsHelperOutput struct {
	Listed  []SessionRecord `json:"listed"`
	Removed int             `json:"removed"`
	Err     string          `json:"err,omitempty"`
}

// TestPIDNamespaceHelper is not a test on its own: it is the reader half of
// TestSessionRegistry_ReadFromForeignPIDNamespace, run by the test binary
// re-executed inside a separate PID namespace. Without the env var it
// returns at once.
func TestPIDNamespaceHelper(t *testing.T) {
	if os.Getenv(pidnsHelperEnv) != "1" {
		return
	}
	var out pidnsHelperOutput
	recs, err := ListSessions()
	if err != nil {
		out.Err = err.Error()
	}
	out.Listed = recs
	if rep, err := PruneSessions(0, time.Now()); err != nil {
		out.Err = err.Error()
	} else {
		out.Removed = len(rep.Removed)
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
	os.Exit(0) // keep the testing package's PASS line off stdout
}

// TestSessionRegistry_ReadFromForeignPIDNamespace is BUG-4090's
// reproduction: a row for a LIVE host session, read by `pad session list`
// and `pad session prune` from a shell in a separate PID namespace that
// shares the filesystem (bwrap --dev-bind / / --unshare-pid). There every
// host pid is ESRCH, so the reader cannot examine the owner: the row must
// read unknown and survive the prune, not read dead and be deleted.
//
// It needs bwrap and unprivileged user namespaces, so it skips where those
// are missing (GitHub runners, the Nix sandbox). The seam tables in
// session_owner_test.go are the gate there.
func TestSessionRegistry_ReadFromForeignPIDNamespace(t *testing.T) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap not installed")
	}
	nsArgs := []string{"--dev-bind", "/", "/", "--unshare-pid", "--proc", "/proc"}
	if err := exec.Command(bwrap, append(nsArgs, "true")...).Run(); err != nil {
		t.Skipf("bwrap cannot create a PID namespace here: %v", err)
	}

	home := t.TempDir()
	isolateHome(t, home)
	clearSessionEnv(t)

	// The live owner: a child of this test, outside the namespace.
	owner := exec.Command("sleep", "300")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })

	// Its messaging socket. A short directory: unix socket paths are capped
	// near 108 bytes, and t.TempDir paths can exceed that.
	sockDir, err := os.MkdirTemp("", "pidns")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	t.Setenv("PAD_SESSION_PID", itoa(owner.Process.Pid))
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", sock)
	// Record with the REAL namespace, as a host registration would; the
	// package's TestMain pins the seam to the init namespace.
	prev := pidNamespaceFn
	pidNamespaceFn = currentPIDNamespace
	rec, err := RegisterSession(home, "pidns-test")
	pidNamespaceFn = prev
	if err != nil {
		t.Fatal(err)
	}
	// Control: from the recorder's own namespace the row is alive, so a
	// non-alive answer below is the namespace and not the fixture.
	if rec.Liveness != LivenessAlive {
		t.Fatalf("control: the host reads its own live row as %s, want alive", rec.Liveness)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	readFromNamespace := func(t *testing.T) pidnsHelperOutput {
		t.Helper()
		cmd := exec.Command(bwrap, append(nsArgs, self, "-test.run=^TestPIDNamespaceHelper$")...)
		cmd.Env = append(os.Environ(), pidnsHelperEnv+"=1")
		cmd.Stderr = os.Stderr
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("namespaced reader: %v\n%s", err, raw)
		}
		var out pidnsHelperOutput
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("namespaced reader output: %v\n%s", err, raw)
		}
		if out.Err != "" {
			t.Fatalf("namespaced reader: %s", out.Err)
		}
		return out
	}
	assertKept := func(t *testing.T, out pidnsHelperOutput) {
		t.Helper()
		if len(out.Listed) != 1 {
			t.Fatalf("namespaced reader listed %d rows, want 1: %+v", len(out.Listed), out.Listed)
		}
		if got := out.Listed[0].Liveness; got != LivenessUnknown {
			t.Errorf("a live host row read from a foreign PID namespace is %s, want unknown", got)
		}
		if out.Removed != 0 {
			t.Errorf("prune from a foreign PID namespace removed %d live host row(s), want 0", out.Removed)
		}
		if _, err := os.Stat(rec.Path); err != nil {
			t.Errorf("the live row's file is gone after the namespaced prune: %v", err)
		}
	}

	t.Run("stamped row", func(t *testing.T) {
		assertKept(t, readFromNamespace(t))
	})

	// P3: the same live row in its pre-fix shape, with no pid_ns or mnt_ns.
	// The reader is not in the init namespace, so its ESRCH is not evidence
	// (option (a)), and the socket is judged by the old rule, under which it
	// is still there with its identity.
	t.Run("unstamped row", func(t *testing.T) {
		raw, err := os.ReadFile(rec.Path)
		if err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		if _, ok := row["pid_ns"]; !ok {
			t.Fatalf("fixture: the registered row has no pid_ns to strip: %s", raw)
		}
		delete(row, "pid_ns")
		delete(row, "mnt_ns")
		stripped, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rec.Path, stripped, 0600); err != nil {
			t.Fatal(err)
		}
		assertKept(t, readFromNamespace(t))
	})
}

// Namespace inodes for the seam tables. Arbitrary, distinct, and neither is
// the init namespace.
const (
	recordedNS  uint64 = 4026530001
	foreignNS   uint64 = 4026530002
	recordedMnt uint64 = 4026530011
	foreignMnt  uint64 = 4026530012
)

// stubReaderNS pins the reader's PID namespace for one test.
func stubReaderNS(t *testing.T, ns uint64, err error) {
	t.Helper()
	prev := pidNamespaceFn
	pidNamespaceFn = func() (uint64, error) { return ns, err }
	t.Cleanup(func() { pidNamespaceFn = prev })
}

// stubReaderMountNS pins the reader's mount namespace for one test.
func stubReaderMountNS(t *testing.T, ns uint64, err error) {
	t.Helper()
	prev := mountNamespaceFn
	mountNamespaceFn = func() (uint64, error) { return ns, err }
	t.Cleanup(func() { mountNamespaceFn = prev })
}

// TestOwnerLiveness_PIDNamespace is BUG-4090's verdict table. Across a
// namespace boundary — or a possible one — absence is not evidence: the pid
// probe and a missing socket both give unknown, and only a socket that
// answers with a different identity stays dead (lead ruling R1).
func TestOwnerLiveness_PIDNamespace(t *testing.T) {
	tok, _ := procStartToken(os.Getpid())
	if tok == "" {
		t.Fatal("no proc-start token for this process on linux")
	}
	live := SessionOwner{PID: os.Getpid(), ProcStart: tok}
	dead := SessionOwner{PID: exitedProcessPID(t)}

	sock := filepath.Join(t.TempDir(), "s.sock")
	if err := os.WriteFile(sock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	ino, dev, ok := statIdentity(info)
	if !ok {
		t.Fatal("no socket identity on linux")
	}
	withSocket := func(o SessionOwner) SessionOwner {
		o.Socket, o.SocketMtimeUnixNano, o.SocketIno, o.SocketDev = sock, info.ModTime().UnixNano(), ino, dev
		return o
	}
	withGoneSocket := func(o SessionOwner) SessionOwner {
		o = withSocket(o)
		o.Socket = filepath.Join(filepath.Dir(sock), "gone.sock")
		return o
	}
	withReboundSocket := func(o SessionOwner) SessionOwner {
		o = withSocket(o)
		o.SocketIno++
		return o
	}
	stamped := func(o SessionOwner) SessionOwner { o.PIDNamespace = recordedNS; return o }
	reused := live
	reused.ProcStart = "0"
	errNS := errors.New("no /proc/self/ns/pid")

	mntStamped := func(o SessionOwner) SessionOwner { o.MountNamespace = recordedMnt; return o }
	sockOnly := SessionOwner{}

	type reader struct {
		pid    uint64
		pidErr error
		mnt    uint64
		mntErr error
	}
	home := reader{pid: recordedNS, mnt: recordedMnt}           // the recorder's own namespaces
	pidAway := reader{pid: foreignNS, mnt: recordedMnt}         // another PID ns, same filesystem view
	away := reader{pid: foreignNS, mnt: foreignMnt}             // another PID ns and mount ns (bwrap)
	initHome := reader{pid: initPIDNamespace, mnt: recordedMnt} // the host
	tests := []struct {
		name  string
		owner SessionOwner
		r     reader
		want  Liveness
	}{
		// Same namespace: every probe means what it says (unchanged).
		{"same ns, dead pid", stamped(dead), home, LivenessDead},
		{"same ns, live pid", stamped(live), home, LivenessAlive},
		{"same ns, reused pid", stamped(reused), home, LivenessDead},
		{"same ns, socket gone", withGoneSocket(stamped(live)), home, LivenessDead},

		// PID ns crossed: the pid cannot be examined, so it never decides.
		{"pid crossed, dead pid", stamped(dead), pidAway, LivenessUnknown},
		{"pid crossed, live pid is not probed", stamped(live), pidAway, LivenessUnknown},
		{"pid crossed, socket matches", withSocket(stamped(dead)), pidAway, LivenessUnknown},
		{"pid stamped, reader pid ns unreadable", stamped(dead), reader{pidErr: errNS, mnt: recordedMnt}, LivenessUnknown},
		{"pid stamped, reader in init ns", stamped(dead), initHome, LivenessUnknown},

		// Unstamped pid, reader in the init ns: today's rule.
		{"pid unstamped, init reader, dead pid", dead, initHome, LivenessDead},
		// Unstamped pid, reader NOT in the init ns: option (a).
		{"pid unstamped, foreign reader, dead pid", dead, away, LivenessUnknown},
		{"pid unstamped, foreign reader, live pid", live, away, LivenessAlive},
		{"pid unstamped, foreign reader, reused pid", reused, away, LivenessDead},
		// Unstamped pid, reader cannot tell: the platform-without-namespaces rule.
		{"pid unstamped, reader pid ns unreadable, dead pid", dead, reader{pidErr: errNS, mnt: recordedMnt}, LivenessDead},

		// The socket is judged by the MOUNT namespace (P1).
		{"mnt same, socket gone", withGoneSocket(mntStamped(sockOnly)), home, LivenessDead},
		{"mnt same, socket rebound", withReboundSocket(mntStamped(sockOnly)), home, LivenessDead},
		{"mnt same, other pid ns, socket gone", withGoneSocket(mntStamped(sockOnly)), pidAway, LivenessDead},
		{"mnt crossed, socket gone is unknown (R1)", withGoneSocket(mntStamped(sockOnly)), away, LivenessUnknown},
		{"mnt crossed, socket rebound is unknown (H3)", withReboundSocket(mntStamped(sockOnly)), away, LivenessUnknown},
		{"mnt crossed, socket matches", withSocket(mntStamped(sockOnly)), away, LivenessAlive},
		{"mnt stamped, reader mnt ns unreadable, socket gone", withGoneSocket(mntStamped(sockOnly)), reader{pid: foreignNS, mntErr: errNS}, LivenessUnknown},
		// Unstamped socket: the pre-fix rule, whoever reads (H1: a container
		// reading its own rows did not cross anything).
		{"mnt unstamped, foreign reader, socket gone (H1)", withGoneSocket(sockOnly), away, LivenessDead},
		{"mnt unstamped, foreign reader, socket rebound", withReboundSocket(sockOnly), away, LivenessDead},
		{"mnt unstamped, init reader, socket gone", withGoneSocket(live), initHome, LivenessDead},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubReaderNS(t, tc.r.pid, tc.r.pidErr)
			stubReaderMountNS(t, tc.r.mnt, tc.r.mntErr)
			o := tc.owner
			if got := OwnerLiveness(&o); got != tc.want {
				t.Fatalf("got %s, want %s (owner %+v)", got, tc.want, o)
			}
		})
	}
}

// TestCaptureSessionOwner_PIDNamespace: the namespace is stamped only on a
// pid this namespace can see. A harness pid inherited from outside a
// sandbox is invisible inside it, and stamping the sandbox's namespace on
// it would make a host reader call a live session unknown — or worse, a
// sandbox reader trust a number that is not its own.
func TestCaptureSessionOwner_PIDNamespace(t *testing.T) {
	clearSessionEnv(t)
	stubReaderNS(t, recordedNS, nil)
	o, err := CaptureSessionOwner()
	if err != nil {
		t.Fatal(err)
	}
	if o.PIDNamespace != recordedNS {
		t.Fatalf("a visible owner pid: pid_ns=%d, want %d", o.PIDNamespace, recordedNS)
	}

	t.Setenv("CLAUDE_PID", itoa(exitedProcessPID(t)))
	o, err = CaptureSessionOwner()
	if err != nil {
		t.Fatal(err)
	}
	if o.PIDNamespace != 0 {
		t.Fatalf("an owner pid this namespace cannot see: pid_ns=%d, want 0", o.PIDNamespace)
	}

	t.Setenv("CLAUDE_PID", "")
	stubReaderNS(t, 0, errors.New("no /proc/self/ns/pid"))
	o, err = CaptureSessionOwner()
	if err != nil {
		t.Fatal(err)
	}
	if o.PIDNamespace != 0 {
		t.Fatalf("an unreadable namespace: pid_ns=%d, want 0", o.PIDNamespace)
	}
}

// TestPruneSessions_KeepsRowsFromAnotherPIDNamespace: prune and the prune
// that runs after every register both keep a row whose pid lives in a
// namespace the reader is not in, whatever that pid looks like from here.
func TestPruneSessions_KeepsRowsFromAnotherPIDNamespace(t *testing.T) {
	isolateHome(t, t.TempDir())
	clearSessionEnv(t)

	// A row whose pid is dead from where we stand, recorded in its own
	// namespace: the pid number proves nothing here.
	stubReaderNS(t, recordedNS, nil)
	owner := exec.Command("sleep", "300")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAD_SESSION_PID", itoa(owner.Process.Pid))
	rec, err := RegisterSession(t.TempDir(), "other-ns")
	if err != nil {
		t.Fatal(err)
	}
	_ = owner.Process.Kill()
	_ = owner.Wait()

	stubReaderNS(t, foreignNS, nil)
	rep, err := PruneSessions(0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Removed) != 0 {
		t.Fatalf("prune from another namespace removed %+v", rep.Removed)
	}
	// Registering from the foreign namespace prunes too (RegisterSession's
	// pruneLocked); it must keep the row as well.
	t.Setenv("PAD_SESSION_PID", "")
	if _, err := RegisterSession(t.TempDir(), "this-ns"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rec.Path); err != nil {
		t.Fatalf("the other namespace's row is gone after a register: %v", err)
	}

	// Control: from the row's own namespace the same dead pid IS dead, and
	// prune removes it. Without this leg the test would pass on a prune
	// that removes nothing.
	stubReaderNS(t, recordedNS, nil)
	rep, err = PruneSessions(0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Removed) != 1 || rep.Removed[0].Path != rec.Path {
		t.Fatalf("prune from the row's own namespace: removed %+v, want exactly %s", rep.Removed, rec.Path)
	}
}

// TestArmStateOwnerVerdict_PIDNamespace: a HEADLESS arm file (pid-keyed)
// carries its owner's namespace, and a reader elsewhere neither reaps it
// (dead) nor trusts it (alive). A socket-keyed file has no pid to judge.
func TestArmStateOwnerVerdict_PIDNamespace(t *testing.T) {
	st := &ArmState{PID: exitedProcessPID(t), PIDNamespace: recordedNS}
	stubReaderNS(t, recordedNS, nil)
	if got := armStateOwnerVerdict(st); got != LivenessDead {
		t.Fatalf("headless arm, same ns, dead pid: got %s, want dead", got)
	}
	stubReaderNS(t, foreignNS, nil)
	if got := armStateOwnerVerdict(st); got != LivenessUnknown {
		t.Fatalf("headless arm, other ns, dead pid: got %s, want unknown", got)
	}
}

// TestWriteArmState_HeadlessRecordsPIDNamespace: the headless write stamps
// its owner's namespace, and a reader in another one leaves the file alone
// and does not arm on it (unverifiable, BUG-2771's posture).
func TestWriteArmState_HeadlessRecordsPIDNamespace(t *testing.T) {
	armStateTestEnv(t, "") // no socket: headless, pid-keyed
	stubReaderNS(t, recordedNS, nil)
	path, err := WriteArmState()
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := readArmState()
	if err != nil || st == nil {
		t.Fatalf("readArmState: %v", err)
	}
	if st.PIDNamespace != recordedNS {
		t.Fatalf("headless arm file: pid_ns=%d, want %d", st.PIDNamespace, recordedNS)
	}
	stubReaderNS(t, foreignNS, nil)
	if got := SessionArmState(); got != LocalArmUnverifiable {
		t.Fatalf("headless arm read from another namespace: got %v, want unverifiable", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("headless arm file reaped from another namespace: %v", err)
	}
}

// TestCaptureSessionOwner_MountNamespace: mnt_ns is stamped only beside a
// socket the recorder stat'ed with its identity (lead ruling C1). A pad
// started inside a sandbox by a harness outside it may not see the
// harness's socket; stamping its own mount namespace on that row would let
// a later reader in the same sandbox trust an ENOENT.
func TestCaptureSessionOwner_MountNamespace(t *testing.T) {
	clearSessionEnv(t)
	stubReaderMountNS(t, recordedMnt, nil)

	sock := filepath.Join(t.TempDir(), "s.sock")
	if err := os.WriteFile(sock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", sock)
	o, err := CaptureSessionOwner()
	if err != nil {
		t.Fatal(err)
	}
	if o.Socket != sock || o.MountNamespace != recordedMnt {
		t.Fatalf("a visible socket: socket=%q mnt_ns=%d, want %q and %d", o.Socket, o.MountNamespace, sock, recordedMnt)
	}

	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", filepath.Join(t.TempDir(), "unseen.sock"))
	if o, err = CaptureSessionOwner(); err != nil {
		t.Fatal(err)
	}
	if o.Socket != "" || o.MountNamespace != 0 {
		t.Fatalf("a socket not visible at capture: socket=%q mnt_ns=%d, want neither", o.Socket, o.MountNamespace)
	}

	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	if o, err = CaptureSessionOwner(); err != nil {
		t.Fatal(err)
	}
	if o.MountNamespace != 0 {
		t.Fatalf("no socket at all: mnt_ns=%d, want 0", o.MountNamespace)
	}
}

// TestArmState_SocketKeyedMountNamespace: a socket-keyed arm file carries
// the mount namespace its socket was seen in. Read from that namespace a
// gone socket reaps it, as before; read from another it is kept and does
// not arm (H1 and R1 together).
func TestArmState_SocketKeyedMountNamespace(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	if err := os.WriteFile(sock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	armStateTestEnv(t, sock)
	stubReaderMountNS(t, recordedMnt, nil)
	path, err := WriteArmState()
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := readArmState()
	if err != nil || st == nil {
		t.Fatalf("readArmState: %v", err)
	}
	if st.MountNamespace != recordedMnt {
		t.Fatalf("socket-keyed arm file: mnt_ns=%d, want %d", st.MountNamespace, recordedMnt)
	}
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}

	stubReaderMountNS(t, foreignMnt, nil)
	if got := SessionArmState(); got != LocalArmUnverifiable {
		t.Fatalf("gone socket read from another mount namespace: got %v, want unverifiable", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("arm file reaped from another mount namespace: %v", err)
	}

	stubReaderMountNS(t, recordedMnt, nil)
	if got := SessionArmState(); got != LocalArmAbsent {
		t.Fatalf("gone socket read from its own mount namespace: got %v, want absent (reaped)", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("arm file not reaped from its own mount namespace: %v", err)
	}
}
