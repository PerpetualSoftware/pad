package buildtools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// withoutEnv returns a copy of env with any entries whose name is in drop
// removed, so a test does not depend on what the caller's shell exported.
func withoutEnv(env []string, drop ...string) []string {
	dropSet := make(map[string]bool, len(drop))
	for _, k := range drop {
		dropSet[k] = true
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !dropSet[name] {
			out = append(out, kv)
		}
	}
	return out
}

// readLastLine returns the last non-empty line of path.
func readLastLine(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		t.Fatalf("%s is empty", path)
	}
	lines := strings.Split(trimmed, "\n")
	return lines[len(lines)-1]
}

// BUG-3718: the restart runs under the STOPPED server's settings environment
// (PAD_*, PG[A-Z]*, PUBLIC_URL, HOME), not the installer shell's, and never
// prints setting values (which may be secrets).
func TestInstallRefresh_RestartsUnderTheServersSettings(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	envLog := filepath.Join(home, "env.log")
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	baseEnv := withoutEnv(e.env(), "PAD_REFRESH_TEST_SETTING", "PGAPPNAME", "PUBLIC_URL", "PAD_ONLY_IN_SHELL")

	pre := exec.Command(built, "server", "start", "--host", "127.0.0.1")
	pre.Env = append(baseEnv,
		"PAD_REFRESH_TEST_SETTING=from-server",
		"PGAPPNAME=from-server",
		"PUBLIC_URL=http://from-server.invalid",
		"STUB_ENV_LOG="+envLog,
	)
	startPreServer(t, pre, "127.0.0.1", port)

	cmd := exec.Command("bash", scriptPath(t), built, installed, commit)
	cmd.Env = append(baseEnv,
		"STUB_ENV_LOG="+envLog,
		"PAD_ONLY_IN_SHELL=from-shell",
	)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	requireSafeScriptEnv(t, cmd.Env, cmd.Dir)
	if err := cmd.Run(); err != nil {
		t.Fatalf("refresh failed: %v\nstdout=%s\nstderr=%s", err, out.String(), errb.String())
	}

	if strings.TrimSpace(errb.String()) != "" {
		t.Errorf("successful refresh wrote to stderr: %q", errb.String())
	}

	last := readLastLine(t, envLog)
	for _, want := range []string{
		"PAD_REFRESH_TEST_SETTING=from-server",
		"PGAPPNAME=from-server",
		"PUBLIC_URL=http://from-server.invalid",
	} {
		if !strings.Contains(last, want) {
			t.Errorf("restarted server env missing %q; got %q", want, last)
		}
	}
	if strings.Contains(last, "PAD_ONLY_IN_SHELL") {
		t.Errorf("restarted server inherited installer-only setting PAD_ONLY_IN_SHELL; got %q", last)
	}

	combined := out.String() + errb.String()
	for _, wantName := range []string{"PAD_REFRESH_TEST_SETTING", "PAD_ONLY_IN_SHELL"} {
		if !strings.Contains(combined, wantName) {
			t.Errorf("output did not name %q in capture notes; stdout=%s", wantName, out.String())
		}
	}
	for _, secret := range []string{"from-server", "http://from-server.invalid", "from-shell"} {
		if strings.Contains(combined, secret) {
			t.Errorf("output leaked setting value %q; stdout=%s\nstderr=%s", secret, out.String(), errb.String())
		}
	}
}

// BUG-3718: after capturing the target server's environment, the script's own
// data-dir decisions (log file, refresh marker, pid file) follow the SERVER's
// PAD_DATA_DIR rather than the installer shell's.
func TestInstallRefresh_UsesTheServersDataDir(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	dirA := filepath.Join(t.TempDir(), "dataA")
	dirB := filepath.Join(t.TempDir(), "dataB")
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	envLog := filepath.Join(home, "env.log")

	preEnv := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home, dataDir: dirA}
	pre := exec.Command(built, "server", "start", "--host", "127.0.0.1")
	pre.Env = append(preEnv.env(), "STUB_ENV_LOG="+envLog)
	startPreServer(t, pre, "127.0.0.1", port)

	scriptEnv := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home, dataDir: dirB}
	cmd := exec.Command("bash", scriptPath(t), built, installed, commit)
	cmd.Env = append(scriptEnv.env(), "STUB_ENV_LOG="+envLog, "STUB_ANNOUNCE=1")
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	requireSafeScriptEnv(t, cmd.Env, cmd.Dir)
	if err := cmd.Run(); err != nil {
		t.Fatalf("refresh failed: %v\nstdout=%s\nstderr=%s", err, out.String(), errb.String())
	}

	wantLog := filepath.Join(dirA, "logs", "server.log")
	want := fmt.Sprintf("LISTENING %d", port)
	var got []byte
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if got, _ = os.ReadFile(wantLog); strings.Contains(string(got), want) {
			break
		}
	}
	if !strings.Contains(string(got), want) {
		t.Fatalf("restarted server output is not in %s (want %q); file holds %q\nstdout=%s", wantLog, want, got, out.String())
	}
	otherLog := filepath.Join(dirB, "logs", "server.log")
	if _, err := os.Stat(otherLog); !os.IsNotExist(err) {
		t.Errorf("installer data dir log %s should not have been created (stat err=%v)", otherLog, err)
	}
	last := readLastLine(t, envLog)
	if wantEnv := "PAD_DATA_DIR=" + dirA; !strings.Contains(last, wantEnv) {
		t.Errorf("restarted server env does not show %q; got %q", wantEnv, last)
	}
}

// BUG-3718: on a /proc system, an unreadable /proc/<pid>/environ refuses
// BEFORE anything is stopped or installed, leaving the running server intact.
func TestInstallRefresh_RefusesAnUnreadableServerEnvironment(t *testing.T) {
	requireScriptDeps(t)
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux /proc and PR_SET_DUMPABLE")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can read non-dumpable /proc/<pid>/environ")
	}
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	pre := exec.Command(built, "server", "start", "--host", "127.0.0.1", "--port", strconv.Itoa(port))
	pre.Env = append(e.env(), "STUB_NODUMP=1")
	startPreServer(t, pre, "127.0.0.1", port)

	// PATH without ss or lsof: ss cannot show a non-dumpable listener's pid
	// and would make the script refuse earlier on the port.
	stubPath := t.TempDir()
	for _, tool := range []string{"bash", "sed", "awk", "curl", "cp", "mv", "mkdir", "rm", "chmod", "pkill", "pgrep", "ps", "sleep", "mktemp", "dirname", "basename", "git", "head", "setsid", "nohup", "printf", "cat", "readlink", "grep", "cut", "sort", "tail"} {
		if full, err := exec.LookPath(tool); err == nil {
			_ = os.Symlink(full, filepath.Join(stubPath, tool))
		}
	}
	env := withoutEnv(e.env(), "PATH")
	env = append(env, "PATH="+stubPath)

	cmd := exec.Command("bash", scriptPath(t), built, installed, commit)
	cmd.Env = env
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	requireSafeScriptEnv(t, cmd.Env, cmd.Dir)
	if err := cmd.Run(); err == nil {
		t.Fatalf("script proceeded with an unreadable /proc/<pid>/environ; stdout=%s", out.String())
	}
	if !strings.Contains(errb.String(), "cannot be read: /proc/") || !strings.Contains(errb.String(), "environ") {
		t.Errorf("refusal did not name /proc/.../environ; stderr=%s", errb.String())
	}
	if _, err := os.Stat(installed); err == nil {
		t.Errorf("installed despite refusing: %s", installed)
	}
	if got, err := pidAnswersAt(port); err != nil || got != strconv.Itoa(pre.Process.Pid) {
		t.Errorf("the server was touched by a refused refresh: %q %v", got, err)
	}
}

// BUG-3718: without /proc (macOS or PAD_NO_PROC=1), the environment cannot be
// read reliably; the script notes this and keeps today's behaviour (restarting
// under the caller's shell environment).
func TestInstallRefresh_WithoutProcKeepsTheShellEnvironment(t *testing.T) {
	requireScriptDeps(t, "lsof")
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	envLog := filepath.Join(home, "env.log")
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	baseEnv := withoutEnv(e.env(), "PAD_REFRESH_TEST_SETTING")

	pre := exec.Command(built, "server", "start", "--host", "127.0.0.1", "--port", strconv.Itoa(port))
	pre.Env = append(baseEnv,
		"STUB_ENV_LOG="+envLog,
		"PAD_REFRESH_TEST_SETTING=from-server",
	)
	startPreServer(t, pre, "127.0.0.1", port)

	cmd := exec.Command("bash", scriptPath(t), built, installed, commit)
	cmd.Env = append(baseEnv,
		"STUB_ENV_LOG="+envLog,
		"PAD_NO_PROC=1",
	)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	requireSafeScriptEnv(t, cmd.Env, cmd.Dir)
	if err := cmd.Run(); err != nil {
		t.Fatalf("script failed without /proc: %v\nstdout=%s\nstderr=%s", err, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "server environment NOT captured") {
		t.Errorf("stdout did not note that server environment was not captured; stdout=%s", out.String())
	}
	last := readLastLine(t, envLog)
	if strings.Contains(last, "PAD_REFRESH_TEST_SETTING") {
		t.Errorf("without /proc, expected shell env (without PAD_REFRESH_TEST_SETTING); got %q", last)
	}
}
