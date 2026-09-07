package buildtools

// Tests for scripts/install-refresh.sh — the honest half of `make install`
// (BUG-2897, TASK-2787).
//
// Every stub gets a UNIQUE, SHORT name, and both properties are load-bearing:
//
//   - unique, because the script's stop step is `pkill -x <name>`, which is
//     system-wide. A shared name would let one test kill another's server;
//     the name "pad" would kill the developer's. That containment is also the
//     only reason these tests are safe to run twice, which is the reason this
//     logic moved out of the Makefile recipe at all.
//   - short, because `pgrep -x` matches /proc/<pid>/comm, which the kernel
//     TRUNCATES TO 15 CHARACTERS.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stubBinary is the compiled fake `pad`, built once for the package.
//
// COMPILED, not a shell script, and the reason is the property under test:
// the script locates the running server with `pgrep -x <name>`, which matches
// /proc/<pid>/comm — and comm for a shebang script is the INTERPRETER
// ("bash"), never the script's own name. The first version of these tests
// used a shell stub and failed reporting "no running server found"; that was
// the FIXTURE failing, not the script. A fixture that cannot be found the way
// the real artifact is found tests nothing.
var stubBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "install-refresh-stub")
	if err != nil {
		fmt.Fprintln(os.Stderr, "stub tempdir:", err)
		os.Exit(1)
	}
	stubBinary = filepath.Join(dir, "stub")
	build := exec.Command("go", "build", "-o", stubBinary, "./testdata/stub")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build stub:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// uniqueName returns a stub name that is unique per test and under the
// 15-character comm limit pgrep -x matches on.
// requireScriptDeps skips when the environment cannot run the script at all.
//
// The script is a developer-workflow tool: it needs a shell, process tools,
// curl, and a usable loopback. The NIX BUILD SANDBOX has none of that — the
// whole file failed there with "no running server found" and "did not
// answer", which are true statements about the sandbox and say nothing
// about the script. CI's ordinary Go jobs have the tools and run every test
// here, so this preserves the coverage where it is possible and stops
// claiming it where it is not.
//
// The probe is a CAPABILITY CHECK, not a platform guess: it asks for the
// exact things the script uses, so an environment that gains them starts
// running these tests without anyone remembering to update a skip list.
func requireScriptDeps(t *testing.T, extra ...string) {
	t.Helper()
	for _, tool := range append([]string{"bash", "pgrep", "pkill", "curl", "ps"}, extra...) {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available; this environment cannot run install-refresh.sh", tool)
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no usable loopback here: %v", err)
	}
	l.Close()

	// pgrep must actually SEE PROCESSES, not merely exist on PATH.
	//
	// Third narrow guard in this file, and the one that cost two CI rounds:
	// LookPath("pgrep") answers "is the binary installed", while the script
	// needs "can it find a running process". In the Nix build sandbox the
	// binary is there and the answer is empty, so the script reported "no
	// running server found", restarted with DEFAULT argv, and the probe then
	// failed on an address the server was never asked to bind — a failure
	// three steps downstream of a capability nothing had checked.
	//
	// The probe asks pgrep to find THIS process by its own comm, which is
	// the same question the script asks about the server.
	self, err := os.ReadFile("/proc/self/comm")
	comm := strings.TrimSpace(string(self))
	if err != nil || comm == "" {
		comm = filepath.Base(os.Args[0])
		if len(comm) > 15 {
			comm = comm[:15] // the kernel truncates comm; pgrep -x matches it
		}
	}
	out, err := exec.Command("pgrep", "-x", comm).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Skipf("pgrep cannot see processes here (looked for %q); this environment cannot run install-refresh.sh", comm)
	}
}

func uniqueName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	name := "padstb" + hex.EncodeToString(b) // 14 chars
	if len(name) > 15 {
		t.Fatalf("stub name %q exceeds the comm limit pgrep -x matches on", name)
	}
	return name
}

func installStub(t *testing.T, dir, name string) string {
	t.Helper()
	src, err := os.ReadFile(stubBinary)
	if err != nil {
		t.Fatalf("read stub: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, src, 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func killStub(t *testing.T, name string) {
	t.Helper()
	_ = exec.Command("pkill", "-KILL", "-x", name).Run()
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func scriptPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "scripts", "install-refresh.sh"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("script not found at %s: %v", p, err)
	}
	return p
}

type stubEnv struct {
	version string
	healthy string // "1" serves health; anything else exits immediately
	argvLog string
	port    int
	home    string
	// installedPath + installedVersion make the file at the FINAL path
	// report a different version from everything else, which is the only
	// way to exercise the destination check now that the script stages a
	// verified copy beside it.
	installedPath    string
	installedVersion string
}

func (e stubEnv) env() []string {
	return append(os.Environ(),
		"HOME="+e.home,
		fmt.Sprintf("PAD_PORT=%d", e.port),
		"PAD_PROBE_TIMEOUT=8",
		"STUB_VERSION="+e.version,
		"STUB_HEALTHY="+e.healthy,
		"STUB_ARGV_LOG="+e.argvLog,
		fmt.Sprintf("STUB_PORT=%d", e.port),
		"STUB_INSTALLED_PATH="+e.installedPath,
		"STUB_VERSION_INSTALLED="+e.installedVersion,
	)
}

type runResult struct {
	stdout, stderr string
	err            error
}

func runScript(t *testing.T, e stubEnv, built, installed, commit string) runResult {
	t.Helper()
	cmd := exec.Command("bash", scriptPath(t), built, installed, commit)
	cmd.Env = e.env()
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return runResult{stdout: out.String(), stderr: errb.String(), err: err}
}

func waitForListener(t *testing.T, host string, port int, d time.Duration) {
	t.Helper()
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
			c.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("stub server never listened on %s", addr)
}

// THE ARTIFACT CHECK (TASK-2787, and the ordering CONVE-2687's day-74 clause
// asks for): a binary at the shared path carrying a DIFFERENT commit — the
// reversed-ordering case, a sibling's build one merge behind — is refused,
// and refused BEFORE anything irreversible happens.
func TestInstallRefresh_RefusesAForeignBuildWithoutStoppingAnything(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	e := stubEnv{version: "pad version dev (deadbee 2026-01-01T00:00:00Z)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	res := runScript(t, e, built, installed, "cafef00")
	if res.err == nil {
		t.Fatalf("script accepted a binary built at a different commit; stdout=%s", res.stdout)
	}
	if !strings.Contains(res.stderr, "refusing to stop the server") {
		t.Errorf("refusal did not say the server was left alone; stderr=%s", res.stderr)
	}
	// The load-bearing half: the refusal came before the irreversible step,
	// so nothing was installed and nothing was killed.
	if _, err := os.Stat(installed); err == nil {
		t.Errorf("a refused build was installed anyway at %s", installed)
	}
}

// THE ARGV (BUG-2897): the server comes back with the flags the killed
// process had, not with whatever an auto-start defaults to.
func TestInstallRefresh_RestartsWithTheKilledArgv(t *testing.T) {
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
	argvLog := filepath.Join(home, "argv.log")
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: argvLog, port: port, home: home}

	// A "running server" with a NON-DEFAULT host — the shape that produced
	// the original defect, where the replacement bound elsewhere and
	// localhost went dead while every other check read correct.
	pre := exec.Command(built, "server", "start", "--host", "127.0.0.1")
	pre.Env = e.env()
	if err := pre.Start(); err != nil {
		t.Fatalf("start pre-server: %v", err)
	}
	waitForListener(t, "127.0.0.1", port, 8*time.Second)

	res := runScript(t, e, built, installed, commit)
	if res.err != nil {
		t.Fatalf("script failed: %v\nstdout=%s\nstderr=%s", res.err, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "captured server argv") {
		t.Fatalf("script did not capture the running server's argv; stdout=%s", res.stdout)
	}
	// STDERR MUST BE EMPTY on the success path. Codex round 6 found `local`
	// used at top level, which bash answers with an error on stderr for
	// every matching process — the script kept working and printed a
	// spurious error on every normal refresh. Nothing here asserted on
	// stderr, so the suite could not see it: a tool whose job is to stop
	// making unverified claims should not be muttering errors while it
	// succeeds.
	if strings.TrimSpace(res.stderr) != "" {
		t.Errorf("successful refresh wrote to stderr: %q", res.stderr)
	}
	logged, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(logged)), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "--host 127.0.0.1") {
		t.Errorf("restart lost the argv: last invocation was %q, want it to carry --host 127.0.0.1\nall=%v", last, lines)
	}
}

// THE UNCONDITIONAL CLAIM — the third defect, found by reading the recipe
// rather than from either filing: "Server restarted." was printed after a
// command ending in `|| true`, with no probe at all. A restart that does not
// come up must FAIL, not print success.
func TestInstallRefresh_FailsWhenTheServerDoesNotComeBack(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	// healthy != "1": `server start` returns immediately, which is exactly
	// what a restart that silently did not happen looks like from outside.
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "0",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	res := runScript(t, e, built, installed, commit)
	if res.err == nil {
		t.Fatalf("script reported success with no server listening; stdout=%s", res.stdout)
	}
	if strings.Contains(res.stdout, "server restarted and answering") {
		t.Errorf("script printed a restart claim it had not verified; stdout=%s", res.stdout)
	}
	if !strings.Contains(res.stderr, "did not answer on") {
		t.Errorf("failure did not name the addresses that failed; stderr=%s", res.stderr)
	}
	// The install half DID succeed, and saying so is the difference between
	// an honest failure and a confusing one.
	if _, err := os.Stat(installed); err != nil {
		t.Errorf("a probe failure should not unwind the install: %v", err)
	}
}

// CONTROL: the ordinary path succeeds. Every refusal above is meaningless if
// the script refuses everything.
func TestInstallRefresh_OrdinaryRefreshSucceeds(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	res := runScript(t, e, built, installed, commit)
	if res.err != nil {
		t.Fatalf("ordinary refresh failed: %v\nstdout=%s\nstderr=%s", res.err, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "server restarted and answering on: 127.0.0.1") {
		t.Errorf("success path did not report the probed addresses; stdout=%s", res.stdout)
	}
	if _, err := os.Stat(installed); err != nil {
		t.Errorf("ordinary refresh did not install: %v", err)
	}
}

// THE OUTCOME CHECK (TASK-2787's actual ask): the check that matters is on
// the DESTINATION, not on any step. Here the source is exactly what this
// invocation built and the copy at the install path reports a different
// commit — a cp that raced another writer, or half-succeeded.
//
// This test exists because removing the post-copy check left the suite GREEN
// while the pre-kill check alone stayed: a surviving mutant on a guard I had
// written myself, which is the case CONVE-30's family says to distrust first.
// The two checks answer different questions and now fail independently.
func TestInstallRefresh_RefusesWhenTheInstalledFileIsNotWhatWasBuilt(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	built := installStub(t, dir, name)
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	installed := filepath.Join(binDir, name)

	e := stubEnv{
		version:          "pad version dev (" + commit + " x)", // source: correct
		installedPath:    installed,
		installedVersion: "pad version dev (0badbad x)", // destination: wrong
		healthy:          "1",
		argvLog:          filepath.Join(home, "argv.log"),
		port:             port,
		home:             home,
	}

	res := runScript(t, e, built, installed, commit)
	if res.err == nil {
		t.Fatalf("script accepted an installed file carrying a different commit; stdout=%s", res.stdout)
	}
	if !strings.Contains(res.stderr, "installed binary is not the one this invocation built") {
		t.Errorf("refusal did not name the destination as the problem; stderr=%s", res.stderr)
	}
	// It must NOT have gone on to claim a restart.
	if strings.Contains(res.stdout, "server restarted and answering") {
		t.Errorf("script claimed a restart after refusing the install; stdout=%s", res.stdout)
	}
}

// ABBREVIATION LENGTH: an expected commit and an embedded one that differ in
// WIDTH still match, because both are resolved to full object ids and
// compared there.
//
// The test builds its OWN git repository and abbreviates a commit it made,
// rather than naming ids from this checkout. The first version hard-coded
// `a3a1d58` / `a3a1d586` — real commits here — and passed locally while
// FAILING IN CI, where `actions/checkout` fetches shallow and neither id is
// an object the runner has. That is the same defect this file keeps
// recording: a fixture holding a property of my machine rather than of the
// thing under test.
//
// (Production is unaffected by that shallowness: an honest build and install
// produce the SAME abbreviation, which the equality check accepts before any
// resolution is attempted. A width difference requires the object database
// to have grown, which a shallow clone's has not.)
func TestInstallRefresh_ToleratesDifferingCommitAbbreviationWidths(t *testing.T) {
	requireScriptDeps(t, "git")
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)

	// A repository of our own, so the ids resolve wherever this runs.
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@example.com", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "one"},
	} {
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	shaCmd := exec.Command("git", "rev-parse", "HEAD")
	shaCmd.Dir = repo
	shaOut, err := shaCmd.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	full := strings.TrimSpace(string(shaOut))
	short7, short8 := full[:7], full[:8]

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	e := stubEnv{version: "pad version dev (" + short7 + " 2026-09-07T12:33:21Z)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	cmd := exec.Command("bash", scriptPath(t), built, installed, short8)
	cmd.Env = e.env()
	cmd.Dir = repo // the script resolves ids in ITS cwd
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("script rejected a build whose commit differs only in abbreviation width: %v\nstdout=%s\nstderr=%s",
			err, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "server restarted and answering") {
		t.Errorf("success path did not complete; stdout=%s", out.String())
	}
}

// ...and a DIFFERENT commit of the same width is still refused, or the fix
// above would have turned the guard off rather than widened it.
func TestInstallRefresh_StillRefusesAGenuinelyDifferentCommit(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	e := stubEnv{version: "pad version dev (b0b0b0b 2026-09-07T12:33:21Z)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	res := runScript(t, e, built, installed, "a3a1d586")
	if res.err == nil {
		t.Fatalf("prefix matching accepted an unrelated commit; stdout=%s", res.stdout)
	}
}

// PORT FROM THE CAPTURED ARGV (codex round 1): a server started with
// `--port N` is restarted with `--port N`, so the probe must go to N.
//
// The finding was that probing a fixed 7777 fails a HEALTHY restart, with
// this script's most confident message. Verified in the code before
// accepting it: cmd_server.go declares `--port` with default 7777, so the
// case is reachable. The general form is the reason it matters — the script
// exists to preserve an invocation, and a probe that reads only half of
// that invocation is checking a different server.
func TestInstallRefresh_ProbesThePortFromTheCapturedArgv(t *testing.T) {
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
	argvLog := filepath.Join(home, "argv.log")
	// PAD_PORT is deliberately left at a DIFFERENT value from the argv's
	// --port, so a script that falls back to the environment fails this.
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: argvLog, port: port, home: home}
	env := append(e.env(), fmt.Sprintf("PAD_PORT=%d", freePort(t)))

	pre := exec.Command(built, "server", "start", "--host", "127.0.0.1", "--port", strconv.Itoa(port))
	pre.Env = env
	if err := pre.Start(); err != nil {
		t.Fatalf("start pre-server: %v", err)
	}
	waitForListener(t, "127.0.0.1", port, 8*time.Second)

	cmd := exec.Command("bash", scriptPath(t), built, installed, commit)
	cmd.Env = env
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		t.Fatalf("script failed a healthy restart on a non-default port: %v\nstdout=%s\nstderr=%s", err, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), fmt.Sprintf("(port %d)", port)) {
		t.Errorf("probe did not use the argv's port %d; stdout=%s", port, out.String())
	}
}

// THE `--flag=value` SPELLING (codex round 2, P1): Cobra accepts both
// `--port 8080` and `--port=8080`, so a parser that reads only the
// space-separated form probes the wrong port on a healthy restart.
//
// This test exists because removing the `=` branch left the suite GREEN —
// the second guard in this file to survive its own mutant, both times
// because every fixture used the one spelling I had in mind while writing
// the parser. A fixture drawn from the author's model of the input tests the
// model, not the input.
func TestInstallRefresh_ParsesEqualsSeparatedFlags(t *testing.T) {
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
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}
	// PAD_PORT points elsewhere, so a parser that misses the `=` form falls
	// back to the wrong port and the probe fails.
	env := append(e.env(), fmt.Sprintf("PAD_PORT=%d", freePort(t)))

	pre := exec.Command(built, "server", "start", "--host=127.0.0.1", "--port="+strconv.Itoa(port))
	pre.Env = env
	if err := pre.Start(); err != nil {
		t.Fatalf("start pre-server: %v", err)
	}
	waitForListener(t, "127.0.0.1", port, 8*time.Second)

	cmd := exec.Command("bash", scriptPath(t), built, installed, commit)
	cmd.Env = env
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("script failed on --flag=value argv: %v\nstdout=%s\nstderr=%s", err, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), fmt.Sprintf("(port %d)", port)) {
		t.Errorf("probe did not use the port from --port=N; stdout=%s", out.String())
	}
}

// THE NO-SETSID FALLBACK (codex round 2, P1): setsid is Linux-specific and
// absent from a default macOS environment, where an unconditional call fails
// AFTER the old server has been stopped — the one ordering this script
// exists to avoid.
//
// A mutant that removes the `command -v setsid` guard SURVIVES on Linux by
// construction: setsid is always there. That is a real coverage boundary and
// not a weak test, so the fallback is reached deliberately via PAD_NO_SETSID
// and the assertion is that the restart still detaches and serves. The
// branch is what carries risk; the detection is one builtin.
func TestInstallRefresh_RestartsWithoutSetsid(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	cmd := exec.Command("bash", scriptPath(t), built, installed, commit)
	cmd.Env = append(e.env(), "PAD_NO_SETSID=1")
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("script failed without setsid: %v\nstdout=%s\nstderr=%s", err, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "server restarted and answering") {
		t.Errorf("fallback restart did not come up; stdout=%s", out.String())
	}
}

// HOST AND PORT FROM THE CONFIG FILE (codex round 3, P1): a server whose
// bind comes from ~/.pad/config.toml alone restarts correctly and must be
// probed there. The previous answer was a stated boundary; a permanent
// false failure on every install for that user is not a boundary worth
// stating when the fix is a line match against a flat TOML file.
func TestInstallRefresh_ResolvesHostAndPortFromTheConfigFile(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	if err := os.MkdirAll(filepath.Join(home, ".pad"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := fmt.Sprintf("mode = \"local\"\nhost = \"127.0.0.1\"\nport = %d\n", port)
	if err := os.WriteFile(filepath.Join(home, ".pad", "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	// No running server, so no argv to capture, and PAD_PORT is unset: the
	// config file is the ONLY source of the port. STUB_PORT still tells the
	// stub where to listen, standing in for the config the real server reads.
	env := e.env()
	filtered := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "PAD_PORT=") || strings.HasPrefix(kv, "PAD_HOST=") {
			continue
		}
		filtered = append(filtered, kv)
	}
	cmd := exec.Command("bash", scriptPath(t), built, installed, commit)
	cmd.Env = filtered
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("script ignored the config file's port: %v\nstdout=%s\nstderr=%s", err, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), fmt.Sprintf("(port %d)", port)) {
		t.Errorf("probe did not use the config file's port; stdout=%s", out.String())
	}
}

// IPv6 (codex round 3, P2): `::1` must be bracketed in the probe URL, or
// curl is handed http://::1:PORT/... — not a URL — and a healthy
// IPv6-bound server reads as unreachable.
//
// Skipped rather than failed where the loopback has no IPv6: an environment
// without it cannot discriminate, and a test that passes vacuously there
// while claiming to cover IPv6 is worse than one that says it did not run.
func TestInstallRefresh_ProbesIPv6HostsWithBrackets(t *testing.T) {
	requireScriptDeps(t)
	l, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	const commit = "abc1234"

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	pre := exec.Command(built, "server", "start", "--host", "::1", "--port", strconv.Itoa(port))
	pre.Env = e.env()
	if err := pre.Start(); err != nil {
		t.Fatalf("start pre-server: %v", err)
	}
	waitForListener(t, "::1", port, 8*time.Second)

	// The script probes with curl, so the precondition is whether CURL can
	// reach ::1 — not whether Go can listen on it. The Nix build sandbox
	// satisfies the second and not the first, and the first version of this
	// guard asked the wrong one: the test ran, curl failed, and the failure
	// read as a defect in the bracketing it was written to pin.
	//
	// The v4 leg is the CONTROL. Skipping only when v6 fails while v4
	// succeeds distinguishes "this environment has no usable IPv6" from
	// "curl is broken here", and refuses to skip on the second.
	if err := exec.Command("curl", "-fsS", "-m", "2", "-o", "/dev/null",
		fmt.Sprintf("http://[::1]:%d/api/v1/health", port)).Run(); err != nil {
		if err4 := exec.Command("curl", "-fsS", "-m", "2", "-o", "/dev/null",
			fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", port)).Run(); err4 == nil {
			t.Skipf("curl cannot reach IPv6 loopback here (v4 works): %v", err)
		}
	}

	res := runScript(t, e, built, installed, commit)
	if res.err != nil {
		t.Fatalf("script failed against an IPv6-bound server: %v\nstdout=%s\nstderr=%s",
			res.err, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "server restarted and answering") {
		t.Errorf("IPv6 probe did not succeed; stdout=%s", res.stdout)
	}
}

// THE ORIGINAL DEFECT, and until now the only scenario in this file that was
// never exercised: a WILDCARD bind promises loopback as well as the LAN
// address, and BUG-2897 was a `--host 0.0.0.0` server coming back bound
// elsewhere with 127.0.0.1 dead while every other check read correct.
//
// A mutant dropping 127.0.0.1 from the wildcard probe set SURVIVED the whole
// suite — every other fixture here uses an explicit host, so nothing asked
// what a wildcard promises. Third time in this unit that a fixture drawn
// from my own model of the input tested the model instead: same version
// strings on both sides, one flag spelling, and now one bind shape.
func TestInstallRefresh_WildcardBindMustAnswerOnLoopback(t *testing.T) {
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
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	pre := exec.Command(built, "server", "start", "--host", "0.0.0.0", "--port", strconv.Itoa(port))
	pre.Env = e.env()
	if err := pre.Start(); err != nil {
		t.Fatalf("start pre-server: %v", err)
	}
	waitForListener(t, "127.0.0.1", port, 8*time.Second)

	res := runScript(t, e, built, installed, commit)
	if res.err != nil {
		t.Fatalf("wildcard refresh failed: %v\nstdout=%s\nstderr=%s", res.err, res.stdout, res.stderr)
	}
	// The assertion is on WHAT WAS PROBED, not merely that it succeeded: a
	// script that probed only the LAN address would also exit 0 here, and
	// that is precisely the defect.
	if !strings.Contains(res.stdout, "answering on: 127.0.0.1") {
		t.Errorf("a wildcard bind was accepted without probing loopback — the BUG-2897 defect; stdout=%s", res.stdout)
	}
}

// TOML INLINE COMMENTS (codex round 4, P1): `port = 8080 # local server` is
// valid TOML and the Go parser accepts it. A value regex that swallows the
// comment resolves the port to "8080 # local server", probes nothing, and
// fails every install for that user.
func TestInstallRefresh_ConfigValuesIgnoreInlineComments(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	if err := os.MkdirAll(filepath.Join(home, ".pad"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := fmt.Sprintf("host = \"127.0.0.1\"  # dev box\nport = %d # local server\n", port)
	if err := os.WriteFile(filepath.Join(home, ".pad", "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	env := e.env()[:0]
	for _, kv := range e.env() {
		if strings.HasPrefix(kv, "PAD_PORT=") || strings.HasPrefix(kv, "PAD_HOST=") {
			continue
		}
		env = append(env, kv)
	}
	cmd := exec.Command("bash", scriptPath(t), built, installed, commit)
	cmd.Env = env
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("inline comments broke config resolution: %v\nstdout=%s\nstderr=%s", err, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), fmt.Sprintf("(port %d)", port)) {
		t.Errorf("probe did not use the commented config port; stdout=%s", out.String())
	}
}

// THE ps FALLBACK (codex round 5, P1): macOS has no /proc, so the argv
// capture found nothing there, the restart fell back to defaults, and the
// BUG-2897 fix was silently absent on the platform the setsid fallback had
// just been added for.
//
// A mutant removing the fallback SURVIVES on Linux by construction, so the
// branch is reached deliberately through PAD_NO_PROC — the same disposition
// as the setsid case, and for the same reason: the branch carries the risk,
// the detection is one test.
func TestInstallRefresh_CapturesArgvWithoutProc(t *testing.T) {
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
	argvLog := filepath.Join(home, "argv.log")
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: argvLog, port: port, home: home}

	pre := exec.Command(built, "server", "start", "--host", "127.0.0.1", "--port", strconv.Itoa(port))
	pre.Env = e.env()
	if err := pre.Start(); err != nil {
		t.Fatalf("start pre-server: %v", err)
	}
	waitForListener(t, "127.0.0.1", port, 8*time.Second)

	cmd := exec.Command("bash", scriptPath(t), built, installed, commit)
	cmd.Env = append(e.env(), "PAD_NO_PROC=1")
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("script failed without /proc: %v\nstdout=%s\nstderr=%s", err, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "captured server argv") {
		t.Fatalf("the ps fallback captured nothing; stdout=%s", out.String())
	}
	logged, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(logged)), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "--host 127.0.0.1") {
		t.Errorf("ps fallback lost the argv: last invocation was %q", last)
	}
}

// A NON-NUMERIC PAD_PORT is ignored, as the application ignores it (codex
// round 7): config.go falls through to the next source rather than using a
// bad value, so a mistyped or inherited PAD_PORT must not make every refresh
// fail against a server that is running fine.
func TestInstallRefresh_IgnoresNonNumericPortValues(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	if err := os.MkdirAll(filepath.Join(home, ".pad"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := fmt.Sprintf("host = \"127.0.0.1\"\nport = %d\n", port)
	if err := os.WriteFile(filepath.Join(home, ".pad", "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	env := e.env()[:0]
	for _, kv := range e.env() {
		if strings.HasPrefix(kv, "PAD_PORT=") || strings.HasPrefix(kv, "PAD_HOST=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "PAD_PORT=not-a-number")

	cmd := exec.Command("bash", scriptPath(t), built, installed, commit)
	cmd.Env = env
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("a bad PAD_PORT failed the refresh: %v\nstdout=%s\nstderr=%s", err, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), fmt.Sprintf("(port %d)", port)) {
		t.Errorf("did not fall through to the config port; stdout=%s", out.String())
	}
}

// FAIL CLOSED when a wildcard bind's external address cannot be determined
// (codex rounds 5 and 8). The first version warned and exited 0 — this
// unit's own defect wearing a different hat: a success line covering
// something that was not verified.
//
// PATH is emptied of the address-discovery tools to reach the branch, which
// is otherwise unreachable on a box that has them.
func TestInstallRefresh_FailsClosedWhenTheLANAddressIsUnknown(t *testing.T) {
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
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	pre := exec.Command(built, "server", "start", "--host", "0.0.0.0", "--port", strconv.Itoa(port))
	pre.Env = e.env()
	if err := pre.Start(); err != nil {
		t.Fatalf("start pre-server: %v", err)
	}
	waitForListener(t, "127.0.0.1", port, 8*time.Second)

	// A PATH with only the tools the script genuinely needs, and WITHOUT
	// hostname/ipconfig. Built by symlinking, so the rest of the script
	// still works and only address discovery is impossible.
	stubPath := t.TempDir()
	for _, tool := range []string{"bash", "sed", "awk", "curl", "cp", "mv", "mkdir", "rm", "chmod", "pkill", "pgrep", "ps", "sleep", "mktemp", "dirname", "basename", "git", "head", "setsid", "nohup", "printf", "cat"} {
		full, err := exec.LookPath(tool)
		if err != nil {
			continue
		}
		_ = os.Symlink(full, filepath.Join(stubPath, tool))
	}

	env := e.env()[:0]
	for _, kv := range e.env() {
		if strings.HasPrefix(kv, "PATH=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "PATH="+stubPath)

	cmd := exec.Command("bash", scriptPath(t), built, installed, commit)
	cmd.Env = env
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err == nil {
		t.Fatalf("script reported success without verifying the external bind; stdout=%s", out.String())
	}
	if !strings.Contains(errb.String(), "cannot determine a LAN address") {
		t.Errorf("failure did not name the unverifiable half; stderr=%s", errb.String())
	}
	if strings.Contains(out.String(), "server restarted and answering") {
		t.Errorf("script claimed a verified restart; stdout=%s", out.String())
	}
}

// MORE THAN ONE SERVER is refused rather than guessed (codex round 9). The
// stop is a system-wide `pkill -x` and the restart can only restore one
// argv, so a second server would be killed and left down — or restarted
// with the wrong flags.
func TestInstallRefresh_RefusesWhenSeveralServersAreRunning(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	portA, portB := freePort(t), freePort(t)
	const commit = "abc1234"

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: portA, home: home}

	for _, p := range []int{portA, portB} {
		envp := e.env()[:0]
		for _, kv := range e.env() {
			if strings.HasPrefix(kv, "STUB_PORT=") {
				continue
			}
			envp = append(envp, kv)
		}
		envp = append(envp, fmt.Sprintf("STUB_PORT=%d", p))
		pre := exec.Command(built, "server", "start", "--host", "127.0.0.1", "--port", strconv.Itoa(p))
		pre.Env = envp
		if err := pre.Start(); err != nil {
			t.Fatalf("start server on %d: %v", p, err)
		}
		waitForListener(t, "127.0.0.1", p, 8*time.Second)
	}

	res := runScript(t, e, built, installed, commit)
	if res.err == nil {
		t.Fatalf("script proceeded with two servers running; stdout=%s", res.stdout)
	}
	if !strings.Contains(res.stderr, "more than one") {
		t.Errorf("refusal did not name the ambiguity; stderr=%s", res.stderr)
	}
	// Nothing stopped, nothing installed.
	if _, err := os.Stat(installed); err == nil {
		t.Errorf("installed despite refusing: %s", installed)
	}
}

// AN UNRESOLVABLE ID IS REFUSED, even when it shares a prefix with the
// expected one (codex round 9).
//
// The prefix fallback existed for binaries built outside this checkout, and
// it admitted exactly the collision that resolution was introduced to stop:
// two ids sharing seven characters are indistinguishable to a prefix test
// and distinct to git. Removing the fallback left the suite GREEN because no
// fixture used an id git cannot resolve — the fourth time in this unit that
// a guard's own case was missing from the fixtures.
func TestInstallRefresh_RefusesUnresolvableCommitEvenOnAPrefixMatch(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	// Neither id exists in this repository, and one is a prefix of the other.
	e := stubEnv{version: "pad version dev (b0b0b0b 2026-01-01T00:00:00Z)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	res := runScript(t, e, built, installed, "b0b0b0b0")
	if res.err == nil {
		t.Fatalf("an unresolvable prefix match was accepted; stdout=%s", res.stdout)
	}
	if _, err := os.Stat(installed); err == nil {
		t.Errorf("installed despite refusing: %s", installed)
	}
}

// EVERY test in this file must call requireScriptDeps, and this asserts it by
// reading the file rather than by anyone remembering.
//
// It exists because the scripted edit that added those calls used the pattern
// `TestInstallRefresh_[A-Za-z]+`, which does not match a name containing a
// DIGIT — so `TestInstallRefresh_ProbesIPv6HostsWithBrackets` silently kept
// running in an environment that cannot support it, and CI stayed red for a
// round on a test the fix was supposed to have covered. The set the edit
// touched and the population it was meant to cover were not the same set, and
// nothing said so.
//
// Reading the source is the only instrument that can see this: every runtime
// check passes, because the untouched test runs fine wherever the
// dependencies exist.
func TestEveryScriptTestDeclaresItsDependencies(t *testing.T) {
	src, err := os.ReadFile("install_refresh_test.go")
	if err != nil {
		t.Fatalf("read own source: %v", err)
	}
	re := regexp.MustCompile(`func (TestInstallRefresh_\w+)\(t \*testing\.T\) \{\n(\t[^\n]*)`)
	matches := re.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("the pattern matched no tests at all — this guard is not looking at what it thinks it is")
	}
	var missing []string
	for _, m := range matches {
		if !strings.Contains(m[2], "requireScriptDeps") {
			missing = append(missing, m[1])
		}
	}
	if len(missing) > 0 {
		t.Errorf("these tests drive install-refresh.sh without declaring their dependencies, so they FAIL rather than SKIP where the tools are absent: %v", missing)
	}
}
