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
	f, _ := runPidFileRefresh(t, pidFileCase{body: func(int) string { return jsonRecord(999999, "/x/pad") }})
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

// pidIsDeadWith runs the script's own numeric and pid_is_dead (extracted, the
// rest of the script never runs) with fakePS first on PATH when non-empty.
func pidIsDeadWith(t *testing.T, fakePS string, pid int) bool {
	t.Helper()
	requireScriptDeps(t)
	script := scriptPath(t)
	var fns strings.Builder
	for _, expr := range []string{`/^numeric() /p`, `/^pid_is_dead() {/,/^}/p`} {
		out, err := exec.Command("sed", "-n", expr, script).Output()
		if err != nil || len(out) == 0 {
			t.Fatalf("extract %q from the script: %v", expr, err)
		}
		fns.Write(out)
	}
	prog := fns.String() + fmt.Sprintf("\nif pid_is_dead %d; then echo DEAD; else echo ALIVE; fi\n", pid)
	cmd := exec.Command("bash", "-c", prog)
	cmd.Env = os.Environ()
	if fakePS != "" {
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd.Env = append(cmd.Env, "PATH="+bin+":"+os.Getenv("PATH"))
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run extracted functions: %v", err)
	}
	return strings.TrimSpace(string(out)) == "DEAD"
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

func TestInstallRefresh_PIDFile_UnknownLivenessIsNotDead(t *testing.T) {
	requireScriptDeps(t)
	pid := reapedPid(t)
	if pidIsDeadWith(t, "fake", pid) {
		t.Errorf("pid_is_dead called pid %d dead although ps reports it exists (the EPERM case)", pid)
	}
}

func TestInstallRefresh_PIDFile_ANonexistentPidIsDeadWithTheRealPS(t *testing.T) {
	requireScriptDeps(t)
	pid := reapedPid(t)
	if !pidIsDeadWith(t, "", pid) {
		t.Errorf("pid_is_dead did not call the nonexistent pid %d dead", pid)
	}
}
