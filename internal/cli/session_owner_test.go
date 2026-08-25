package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// clearSessionEnv unsets every variable CaptureSessionOwner and
// RegisterSession read, so a test's own harness (this suite runs inside
// Claude Code sometimes — CLAUDE_PID is then set for real) cannot leak
// into the case under test.
func clearSessionEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PAD_SESSION_PID", "CLAUDE_PID", "CLAUDE_CODE_MESSAGING_SOCKET", "PAD_SESSION_ID", "CLAUDE_CODE_SESSION_ID"} {
		t.Setenv(k, "")
	}
}

func TestCaptureSessionOwner_PidPrecedence(t *testing.T) {
	self, parent := os.Getpid(), os.Getppid()
	tests := []struct {
		name       string
		padPID     string
		claudePID  string
		wantPID    int
		wantSource string
		wantErr    bool
	}{
		{"nothing set keys on self", "", "", self, "self", false},
		{"CLAUDE_PID is the harness owner", "", itoa(parent), parent, "CLAUDE_PID", false},
		{"PAD_SESSION_PID overrides CLAUDE_PID", itoa(parent), itoa(self), parent, "PAD_SESSION_PID", false},
		{"non-numeric PAD_SESSION_PID is an error, not a fall-through", "abc", itoa(parent), 0, "", true},
		{"zero CLAUDE_PID is an error", "", "0", 0, "", true},
		{"negative PAD_SESSION_PID is an error", "-4", "", 0, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearSessionEnv(t)
			t.Setenv("PAD_SESSION_PID", tc.padPID)
			t.Setenv("CLAUDE_PID", tc.claudePID)
			o, err := CaptureSessionOwner()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got owner %+v", o)
				}
				return
			}
			if err != nil {
				t.Fatalf("CaptureSessionOwner: %v", err)
			}
			if o.PID != tc.wantPID || o.PIDSource != tc.wantSource {
				t.Fatalf("got pid=%d source=%q, want pid=%d source=%q", o.PID, o.PIDSource, tc.wantPID, tc.wantSource)
			}
			if runtime.GOOS == "linux" && o.ProcStart == "" {
				t.Fatalf("on linux a live owner pid must record a proc-start token")
			}
		})
	}
}

func TestCaptureSessionOwner_SocketIdentity(t *testing.T) {
	clearSessionEnv(t)
	sock := filepath.Join(t.TempDir(), "123.sock")
	if err := os.WriteFile(sock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", sock)
	o, err := CaptureSessionOwner()
	if err != nil {
		t.Fatal(err)
	}
	if o.Socket != sock || o.SocketMtimeUnixNano == 0 {
		t.Fatalf("an existing socket must be recorded with its mtime: %+v", o)
	}
	if runtime.GOOS != "windows" && o.SocketIno == 0 {
		t.Fatalf("on unix the socket's inode must be recorded: %+v", o)
	}

	// A socket that does not exist at capture time is NOT recorded: a bare
	// path carries no identity, and OwnerLiveness would judge the record
	// dead on sight.
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", filepath.Join(t.TempDir(), "gone.sock"))
	o, err = CaptureSessionOwner()
	if err != nil {
		t.Fatal(err)
	}
	if o.Socket != "" {
		t.Fatalf("a vanished socket must not be recorded, got %q", o.Socket)
	}
}

func TestOwnerLiveness_Pid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pid liveness is unknown on windows; see TestOwnerLiveness_WindowsIsUnknown")
	}
	if got := OwnerLiveness(nil); got != LivenessDead {
		t.Fatalf("nil owner: got %s, want dead", got)
	}
	if got := OwnerLiveness(&SessionOwner{}); got != LivenessDead {
		t.Fatalf("zero owner (no pid, no socket): got %s, want dead", got)
	}

	// Live control: this process, with its real start token.
	tok, _ := procStartToken(os.Getpid())
	if got := OwnerLiveness(&SessionOwner{PID: os.Getpid(), ProcStart: tok}); got != LivenessAlive {
		t.Fatalf("self with matching token: got %s, want alive", got)
	}
	// Live pid, no token recorded (a non-Linux capture): bare liveness.
	if got := OwnerLiveness(&SessionOwner{PID: os.Getpid()}); got != LivenessAlive {
		t.Fatalf("self without token: got %s, want alive (documented residual)", got)
	}
	// Dead leg: a reaped child.
	if got := OwnerLiveness(&SessionOwner{PID: exitedProcessPID(t)}); got != LivenessDead {
		t.Fatalf("reaped pid: got %s, want dead", got)
	}
	// Reused-pid leg: live pid, token that cannot match.
	if got := OwnerLiveness(&SessionOwner{PID: os.Getpid(), ProcStart: "0"}); got != LivenessDead {
		t.Fatalf("live pid with mismatched token: got %s, want dead (pid reuse)", got)
	}
}

func TestOwnerLiveness_Socket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	if err := os.WriteFile(sock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	o := SessionOwner{Socket: sock, SocketMtimeUnixNano: info.ModTime().UnixNano()}
	if ino, dev, ok := statIdentity(info); ok {
		o.SocketIno, o.SocketDev = ino, dev
	}
	if got := OwnerLiveness(&o); got != LivenessAlive {
		t.Fatalf("socket with matching identity: got %s, want alive", got)
	}

	// The socket decides ALONE: a dead pid alongside a live socket is still
	// alive (the socket outlives every short-lived command and is the
	// stronger signal), so the pid leg must not be consulted here.
	withDeadPID := o
	withDeadPID.PID = exitedProcessPID(t)
	if got := OwnerLiveness(&withDeadPID); got != LivenessAlive {
		t.Fatalf("live socket + dead pid: got %s, want alive (socket decides)", got)
	}

	// No identity recorded → cannot prove it is ours → dead.
	if got := OwnerLiveness(&SessionOwner{Socket: sock}); got != LivenessDead {
		t.Fatalf("socket without identity: got %s, want dead", got)
	}
	// Inode mismatch → a rebound socket → dead, even with a matching mtime.
	if o.SocketIno != 0 {
		rebound := o
		rebound.SocketIno++
		if got := OwnerLiveness(&rebound); got != LivenessDead {
			t.Fatalf("socket with mismatched inode: got %s, want dead", got)
		}
	}
	// Socket vanished → dead, regardless of the pid.
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	gone := o
	gone.PID = os.Getpid()
	if got := OwnerLiveness(&gone); got != LivenessDead {
		t.Fatalf("vanished socket: got %s, want dead", got)
	}
}

// TestOwnerLiveness_WindowsIsUnknown pins the tri-state's reason to exist:
// on Windows pidAlive cannot probe, and the verdict must be UNKNOWN — not
// dead, which a reaper would act on. This runs only on Windows; on every
// other platform the case is unreachable and the package's Windows
// posture is enforced by code review, not by this suite. That is the
// boundary of what this test file covers.
func TestOwnerLiveness_WindowsIsUnknown(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only leg")
	}
	if got := OwnerLiveness(&SessionOwner{PID: os.Getpid()}); got != LivenessUnknown {
		t.Fatalf("windows pid liveness: got %s, want unknown", got)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
