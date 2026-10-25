package buildtools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// BUG-4068: section 4 of install-refresh.sh removes <data dir>/pad.pid only
// when it names the pid this refresh STOPPED or a pid that is provably DEAD.
// It keeps the file when it names any live process (a sibling server sharing
// the data dir, or a reused pid), when liveness is unknown (EPERM), and when
// it does not parse. The testdata stub never writes pad.pid itself, so after a
// refresh the file is either the fixture's bytes or absent.

type pidFileCase struct {
	// body builds the pad.pid content from the stopped stub's pid.
	body func(stopped int) string
	// server starts a stub on the port first, so the refresh stops it.
	server bool
}

func jsonRecord(pid int, exe string) string {
	return fmt.Sprintf("{\n  \"pid\": %d,\n  \"started_at\": \"2026-10-25T00:00:00Z\",\n  \"exe\": %q\n}\n", pid, exe)
}

// liveSibling starts a long-lived process this test owns, unrelated to the
// stub, and returns its pid.
func liveSibling(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "600")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sibling: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// runPidFileRefresh runs the real script against a stub on the test's own
// port and returns the pad.pid path and the fixture bytes written to it.
func runPidFileRefresh(t *testing.T, c pidFileCase) (pidFile, written string) {
	t.Helper()
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home,
		dataDir: filepath.Join(t.TempDir(), "data")}
	if err := os.MkdirAll(e.dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stopped := 0
	if c.server {
		pre := exec.Command(built, "server", "start", "--host", "127.0.0.1")
		pre.Env = e.env()
		startPreServer(t, pre, "127.0.0.1", port)
		stopped = pre.Process.Pid
	}
	pidFile = filepath.Join(e.dataDir, "pad.pid")
	written = c.body(stopped)
	if err := os.WriteFile(pidFile, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runScript(t, e, built, installed, commit)
	if res.err != nil {
		t.Fatalf("refresh failed: %v\nstdout=%s\nstderr=%s", res.err, res.stdout, res.stderr)
	}
	return pidFile, written
}

func wantRemoved(t *testing.T, pidFile string) {
	t.Helper()
	if b, err := os.ReadFile(pidFile); err == nil {
		t.Errorf("pad.pid survived the refresh with content %q", b)
	} else if !os.IsNotExist(err) {
		t.Errorf("stat pad.pid: %v", err)
	}
}

func wantKept(t *testing.T, pidFile, written string) {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("pad.pid was removed (%v); it must be kept byte-identical", err)
	}
	if string(b) != written {
		t.Errorf("pad.pid changed: got %q, want %q", b, written)
	}
}

func TestInstallRefresh_PIDFile_RemovesAJSONRecordNamingTheStoppedServer(t *testing.T) {
	requireScriptDeps(t)
	f, _ := runPidFileRefresh(t, pidFileCase{server: true, body: func(p int) string { return jsonRecord(p, "/x/pad") }})
	wantRemoved(t, f)
}

func TestInstallRefresh_PIDFile_KeepsARecordNamingALiveSiblingWhileTheServerIsStopped(t *testing.T) {
	requireScriptDeps(t)
	sib := liveSibling(t)
	f, w := runPidFileRefresh(t, pidFileCase{server: true, body: func(int) string { return jsonRecord(sib, "/x/pad") }})
	wantKept(t, f, w)
}

func TestInstallRefresh_PIDFile_RemovesALegacyBareIntegerNamingTheStoppedServer(t *testing.T) {
	requireScriptDeps(t)
	f, _ := runPidFileRefresh(t, pidFileCase{server: true, body: func(p int) string { return strconv.Itoa(p) + "\n" }})
	wantRemoved(t, f)
}

func TestInstallRefresh_PIDFile_RemovesADeadPidWhenNothingWasStopped(t *testing.T) {
	requireScriptDeps(t)
	dead := reapedPid(t)
	f, _ := runPidFileRefresh(t, pidFileCase{body: func(int) string { return jsonRecord(dead, "/x/pad") }})
	wantRemoved(t, f)
}

func TestInstallRefresh_PIDFile_KeepsALiveSiblingWhenNothingWasStopped(t *testing.T) {
	requireScriptDeps(t)
	sib := liveSibling(t)
	f, w := runPidFileRefresh(t, pidFileCase{body: func(int) string { return jsonRecord(sib, "/x/pad") }})
	wantKept(t, f, w)
}

func TestInstallRefresh_PIDFile_KeepsAFileThatDoesNotParse(t *testing.T) {
	requireScriptDeps(t)
	f, w := runPidFileRefresh(t, pidFileCase{body: func(int) string { return "garbage\n" }})
	wantKept(t, f, w)
}

func TestInstallRefresh_PIDFile_RemovesTheStoppedPidEvenWhenTheExeDiffersBecauseTheStopProvedTheProcess(t *testing.T) {
	requireScriptDeps(t)
	f, _ := runPidFileRefresh(t, pidFileCase{server: true, body: func(p int) string { return jsonRecord(p, "/somewhere/else/pad") }})
	wantRemoved(t, f)
}

// scriptFns runs prog after the script's own numeric, pid_file_pid,
// pid_liveness and pid_file_removable, extracted with sed: the rest of the
// script never runs. pre is bash placed between the functions and prog, used
// to stand in a fake kill for the builtin.
func scriptFns(t *testing.T, pre, prog string) string {
	t.Helper()
	requireScriptDeps(t)
	script := scriptPath(t)
	var fns strings.Builder
	for _, expr := range []string{`/^numeric() /p`, `/^pid_file_pid() {/,/^}/p`, `/^pid_liveness() {/,/^}/p`, `/^pid_file_removable() {/,/^}/p`} {
		out, err := exec.Command("sed", "-n", expr, script).Output()
		if err != nil || len(out) == 0 {
			t.Fatalf("extract %q from the script: %v", expr, err)
		}
		fns.Write(out)
	}
	cmd := exec.Command("bash", "-c", fns.String()+"\n"+pre+"\n"+prog)
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run extracted functions: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// reapedPid is a pid that existed and is gone now.
func reapedPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// The reader checks lines against the SHAPE internal/cli/pidfile.go writes
// (BUG-4068 r1 items 1 and 3, r2 items 1 and 2): braces on their own lines,
// each inner line STARTING like a two-space-indent quoted key (the rest of
// the line is unchecked), one pid, at most 4194304; or a bare integer. It is a
// line-shape check, not full JSON validation. The REFUSED rows do not parse,
// and the file is kept.
func TestInstallRefresh_PIDFile_ReadsOnlyWhatPadWrites(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"MarshalIndent", jsonRecord(4242, "/x/pad"), "4242"},
		{"CRLF", strings.ReplaceAll(jsonRecord(4242, "/x/pad"), "\n", "\r\n"), "4242"},
		{"bare", " 777 \n", "777"},
		{"bare leading zeros", "007\n", "7"},
		{"duplicate pid keys", "{\n  \"pid\": 5,\n  \"pid\": 6\n}\n", "REFUSED"},
		{"nested pid", "{\n  \"x\": {\n    \"pid\": 5\n  }\n}\n", "REFUSED"},
		{"compact one-line JSON is not what pad writes", "{\"pid\": 4242}\n", "REFUSED"},
		{"inner whitespace", "1 2\n", "REFUSED"},
		{"more than 10 digits", "12345678901\n", "REFUSED"},
		{"zero", "0\n", "REFUSED"},
		{"negative", "-5\n", "REFUSED"},
		{"garbage", "garbage\n", "REFUSED"},
		{"missing closing brace", "{\n  \"pid\": 5,\n garbage\n", "REFUSED"},
		{"garbage after the pid line", "{\n  \"pid\": 5,\ngarbage\n}\n", "REFUSED"},
		{"a CR inside a bare pid", "1\r2\n", "REFUSED"},
		{"Linux's largest pid_max", "4194304\n", "4194304"},
		{"above any pid_max", "4194305\n", "REFUSED"},
		{"would wrap as a pid_t", "2147483648\n", "REFUSED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "pad.pid")
			if err := os.WriteFile(f, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			got := scriptFns(t, "", fmt.Sprintf("pid_file_pid %q || printf REFUSED", f))
			if got != tc.want {
				t.Errorf("pid_file_pid(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// DEAD is ESRCH only (BUG-4068 r1 item 2): a fake kill prints each message
// the builtin can, so EPERM and anything unrecognised are not dead.
func TestInstallRefresh_PIDFile_OnlyESRCHIsDead(t *testing.T) {
	for _, tc := range []struct{ name, msg, want string }{
		{"EPERM", "kill: (5) - Operation not permitted", "unknown"},
		{"ESRCH", "kill: (5) - No such process", "dead"},
		{"unrecognised", "kill: something else", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := fmt.Sprintf("kill() { echo %q >&2; return 1; }", tc.msg)
			if got := scriptFns(t, fake, "pid_liveness 5"); got != tc.want {
				t.Errorf("pid_liveness with %q = %q, want %q", tc.msg, got, tc.want)
			}
		})
	}
	if got := scriptFns(t, "", fmt.Sprintf("pid_liveness %d", reapedPid(t))); got != "dead" {
		t.Errorf("the real kill on a reaped pid: %q, want dead", got)
	}
	if got := scriptFns(t, "", fmt.Sprintf("pid_liveness %d", os.Getpid())); got != "alive" {
		t.Errorf("the real kill on this test's own pid: %q, want alive", got)
	}
}

// The stopped-pid leg on its own (BUG-4068 r1 item 5). The e2e stopped-server
// rows reach it too: their pre-server is this test's own child, a zombie
// after the TERM until cleanup reaps it, and kill -0 on a zombie succeeds, so
// the dead leg cannot answer for them. This row does not depend on reaping: a
// LIVE pid named as the stopped pid is removed, and with no stopped pid the
// same file is kept.
func TestInstallRefresh_PIDFile_TheStoppedPidLegRemovesEvenALivePid(t *testing.T) {
	sib := liveSibling(t)
	f := filepath.Join(t.TempDir(), "pad.pid")
	if err := os.WriteFile(f, []byte(jsonRecord(sib, "/x/pad")), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := scriptFns(t, "", fmt.Sprintf("pid_file_removable %q %d && echo REMOVE", f, sib)); got != "REMOVE" {
		t.Errorf("a file naming the stopped pid %d was not removable: %q", sib, got)
	}
	if got := scriptFns(t, "", fmt.Sprintf("pid_file_removable %q '' && echo REMOVE; true", f)); !strings.Contains(got, "is alive") {
		t.Errorf("with no stopped pid, a live pid's file was not kept as alive: %q", got)
	}
}
