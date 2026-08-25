package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// sessionRegistryTestEnv: fresh HOME (empty registry), a temp cwd with NO
// .pad.toml (so ResolveAgentName falls to PAD_AGENT / runtime detection),
// every session/agent variable cleared, and --format json. Not parallel:
// it mutates env, cwd, and the formatFlag global.
func sessionRegistryTestEnv(t *testing.T) (home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on windows
	for _, k := range []string{"PAD_SESSION_PID", "CLAUDE_PID", "CLAUDE_CODE_MESSAGING_SOCKET", "PAD_SESSION_ID", "CLAUDE_CODE_SESSION_ID", "PAD_AGENT", "CLAUDECODE"} {
		t.Setenv(k, "")
	}
	repo := t.TempDir()
	orig, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	prev := formatFlag
	formatFlag = "json"
	t.Cleanup(func() { formatFlag = prev })
	return home
}

func runSessionCmd(t *testing.T, args ...string) string {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() {
		cmd := sessionCmd()
		cmd.SetArgs(args)
		runErr = cmd.Execute()
	})
	if runErr != nil {
		t.Fatalf("pad session %s: %v", strings.Join(args, " "), runErr)
	}
	return out
}

func decodeRecord(t *testing.T, out string) cli.SessionRecord {
	t.Helper()
	var rec cli.SessionRecord
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("parse record %q: %v", out, err)
	}
	return rec
}

func decodeRecords(t *testing.T, out string) []cli.SessionRecord {
	t.Helper()
	var recs []cli.SessionRecord
	if err := json.Unmarshal([]byte(out), &recs); err != nil {
		t.Fatalf("parse records %q: %v", out, err)
	}
	return recs
}

func TestSessionRegister_AgentResolution(t *testing.T) {
	tests := []struct {
		name      string
		padAgent  string
		args      []string
		wantAgent string
	}{
		{"defaults to PAD_AGENT", "wren", nil, "wren"},
		{"--agent overrides PAD_AGENT", "wren", []string{"--agent", "kestrel"}, "kestrel"},
		{"explicit empty --agent is anonymous, not the default", "wren", []string{"--agent", ""}, ""},
		{"no name anywhere is anonymous", "", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionRegistryTestEnv(t)
			t.Setenv("PAD_AGENT", tc.padAgent)
			rec := decodeRecord(t, runSessionCmd(t, append([]string{"register"}, tc.args...)...))
			if rec.Agent != tc.wantAgent {
				t.Fatalf("agent = %q, want %q", rec.Agent, tc.wantAgent)
			}
			if rec.SessionPID != os.Getpid() || rec.SessionPIDSource != "self" || rec.Liveness != cli.LivenessAlive {
				t.Fatalf("with no harness pid the record keys on self and is alive: %+v", rec)
			}
			cwd, _ := os.Getwd()
			if rec.Cwd != cwd {
				t.Fatalf("cwd = %q, want %q", rec.Cwd, cwd)
			}
		})
	}
}

func TestSessionRegister_HarnessPidAndSessionID(t *testing.T) {
	home := sessionRegistryTestEnv(t)
	parent := os.Getppid()
	t.Setenv("CLAUDE_PID", strconv.Itoa(parent))
	t.Setenv("CLAUDE_CODE_SESSION_ID", "01ABC")
	rec := decodeRecord(t, runSessionCmd(t, "register", "--agent", "rook"))
	if rec.SessionPID != parent || rec.SessionPIDSource != "CLAUDE_PID" || rec.SessionID != "01ABC" {
		t.Fatalf("harness identity not recorded: %+v", rec)
	}
	want := filepath.Join(home, ".pad", "sessions", strconv.Itoa(parent)+".json")
	if rec.Path != want {
		t.Fatalf("path = %q, want %q", rec.Path, want)
	}
}

func TestSessionRegister_RejectsBadHarnessPid(t *testing.T) {
	sessionRegistryTestEnv(t)
	t.Setenv("CLAUDE_PID", "nope")
	cmd := sessionCmd()
	cmd.SetArgs([]string{"register"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "CLAUDE_PID") {
		t.Fatalf("expected an error naming CLAUDE_PID, got %v", err)
	}
}

// exitedPID starts and reaps a trivial process, returning its now-dead pid.
func exitedPID(t *testing.T) int {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no shell to spawn a dead process from")
	}
	c := exec.Command(sh, "-c", "exit 0")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	pid := c.Process.Pid
	_ = c.Wait()
	return pid
}

// registerAs writes a record for the given owner pid via the real verb,
// so the list/prune tests exercise the same file the writer produces.
func registerAs(t *testing.T, pid int, agent, cwd string) {
	t.Helper()
	t.Setenv("PAD_SESSION_PID", strconv.Itoa(pid))
	orig, _ := os.Getwd()
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(orig) }()
	runSessionCmd(t, "register", "--agent", agent)
	t.Setenv("PAD_SESSION_PID", "")
}

// backdateRecord rewrites a record's registered_at so ordering assertions
// do not rest on sub-second luck.
func backdateRecord(t *testing.T, path, registeredAt string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["registered_at"] = registeredAt
	out, _ := json.Marshal(raw)
	if err := os.WriteFile(path, out, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionList_FiltersOrderAndAll(t *testing.T) {
	sessionRegistryTestEnv(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	// Register the dead one FIRST: a later register prunes dead records,
	// and this one must be written by hand after the live ones instead.
	registerAs(t, os.Getpid(), "self-agent", dirA)
	registerAs(t, os.Getppid(), "parent-agent", dirB)
	// Make the order STRICT: both registered within one RFC3339 second,
	// which would leave "newest first" to a tie broken by directory order
	// (a coin flip that passes on its own). Backdate the first record.
	backdateRecord(t, filepath.Join(os.Getenv("HOME"), ".pad", "sessions", strconv.Itoa(os.Getpid())+".json"), "2026-08-01T00:00:00Z")
	// A dead session, written directly (register would prune it on the
	// next call — and the point of --all is to show what a prune would
	// take).
	dead := exitedPID(t)
	dir := filepath.Join(os.Getenv("HOME"), ".pad", "sessions")
	deadRec := map[string]any{"session_pid": dead, "session_pid_source": "CLAUDE_PID", "agent": "ghost", "cwd": dirA, "pid": 1, "registered_at": "2026-08-01T00:00:00Z"}
	data, _ := json.Marshal(deadRec)
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(dead)+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	// Default: dead hidden; newest first (parent registered after self).
	recs := decodeRecords(t, runSessionCmd(t, "list"))
	if len(recs) != 2 || recs[0].Agent != "parent-agent" || recs[1].Agent != "self-agent" {
		t.Fatalf("default list wrong (want 2 live rows, newest first): %+v", recs)
	}
	for _, r := range recs {
		if r.Liveness != cli.LivenessAlive {
			t.Fatalf("live rows must read alive: %+v", r)
		}
	}
	// --all adds the dead row.
	recs = decodeRecords(t, runSessionCmd(t, "list", "--all"))
	if len(recs) != 3 {
		t.Fatalf("--all must include the dead row: %+v", recs)
	}
	var sawDead bool
	for _, r := range recs {
		if r.Agent == "ghost" {
			sawDead = r.Liveness == cli.LivenessDead
		}
	}
	if !sawDead {
		t.Fatalf("dead row missing or not dead under --all: %+v", recs)
	}
	// --agent filters exactly; --cwd filters by directory.
	recs = decodeRecords(t, runSessionCmd(t, "list", "--agent", "self-agent"))
	if len(recs) != 1 || recs[0].Agent != "self-agent" {
		t.Fatalf("--agent filter wrong: %+v", recs)
	}
	recs = decodeRecords(t, runSessionCmd(t, "list", "--cwd", dirB))
	if len(recs) != 1 || recs[0].Cwd != dirB {
		t.Fatalf("--cwd filter wrong: %+v", recs)
	}
	// An empty JSON list is `[]`, not null — consumers index it.
	out := runSessionCmd(t, "list", "--agent", "nobody")
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty list must print [], got %q", out)
	}
}

func TestSessionList_TableMarksLegacyAndAnonymous(t *testing.T) {
	sessionRegistryTestEnv(t)
	formatFlag = "table"
	dir := filepath.Join(os.Getenv("HOME"), ".pad", "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// A v1 (legacy) file whose socket basename is our own live pid.
	legacy := map[string]any{"pid": 1, "cwd": "/legacy", "messaging_socket_path": "/run/x/" + strconv.Itoa(os.Getpid()) + ".sock", "registered_at": "2026-08-20T00:00:00Z"}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(dir, "1.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	out := runSessionCmd(t, "list")
	if !strings.Contains(out, "alive (legacy)") || !strings.Contains(out, "\n-\t") && !strings.Contains(out, "\n-  ") {
		t.Fatalf("table must mark the legacy row and show '-' for its missing agent:\n%s", out)
	}
	// The PID column is the socket-derived owner (ours), not the v1 file's
	// registrar pid (1) — the row must name the session that exists.
	if !strings.Contains(out, strconv.Itoa(os.Getpid())) || strings.Contains(out, "\t1\t") || strings.Contains(out, "  1  ") {
		t.Fatalf("legacy row must show the socket-derived pid %d, not the registrar pid:\n%s", os.Getpid(), out)
	}
	if !strings.Contains(out, "AGENT") || !strings.Contains(out, "STATE") {
		t.Fatalf("table header missing:\n%s", out)
	}
}

func TestSessionPrune_Verb(t *testing.T) {
	sessionRegistryTestEnv(t)
	registerAs(t, os.Getpid(), "live", t.TempDir())
	dir := filepath.Join(os.Getenv("HOME"), ".pad", "sessions")
	dead := exitedPID(t)
	deadRec := map[string]any{"session_pid": dead, "agent": "ghost", "cwd": "/x", "pid": 1, "registered_at": "2026-08-01T00:00:00Z"}
	data, _ := json.Marshal(deadRec)
	deadPath := filepath.Join(dir, strconv.Itoa(dead)+".json")
	if err := os.WriteFile(deadPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(dir, "999999.json")
	if err := os.WriteFile(badPath, []byte("nope"), 0600); err != nil {
		t.Fatal(err)
	}

	var rep cli.PruneReport
	if err := json.Unmarshal([]byte(runSessionCmd(t, "prune")), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.DeadRemoved != 1 || rep.UnknownRemoved != 0 || rep.Kept != 2 {
		t.Fatalf("prune without a bound: %+v", rep)
	}
	if _, err := os.Stat(deadPath); !os.IsNotExist(err) {
		t.Fatalf("dead record must be gone: %v", err)
	}
	if _, err := os.Stat(badPath); err != nil {
		t.Fatalf("unknown record must survive an unbounded prune: %v", err)
	}
	// Bound the malformed (unknown) file is older than: its mtime is now,
	// so a 1ns bound covers it.
	if err := json.Unmarshal([]byte(runSessionCmd(t, "prune", "--older-than", "1ns")), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.UnknownRemoved != 1 || rep.Kept != 1 {
		t.Fatalf("prune with a bound: %+v", rep)
	}
	if _, err := os.Stat(badPath); !os.IsNotExist(err) {
		t.Fatalf("old unknown record must be gone under a bound: %v", err)
	}
}

// TestSessionList_EmptyAgentFilterExcludesLegacyAndMalformed: a legacy or
// malformed row has NO agent, not an empty one, so --agent "" returns only
// genuinely anonymous registrations.
func TestSessionList_EmptyAgentFilterExcludesLegacyAndMalformed(t *testing.T) {
	sessionRegistryTestEnv(t)
	registerAs(t, os.Getpid(), "", t.TempDir()) // anonymous v2
	dir := filepath.Join(os.Getenv("HOME"), ".pad", "sessions")
	legacy := map[string]any{"pid": 1, "cwd": "/legacy", "messaging_socket_path": "/run/x/" + strconv.Itoa(os.Getppid()) + ".sock", "registered_at": "2026-08-20T00:00:00Z"}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(dir, "1.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "424242.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	all := decodeRecords(t, runSessionCmd(t, "list"))
	if len(all) != 3 {
		t.Fatalf("unfiltered list must show all three rows: %+v", all)
	}
	recs := decodeRecords(t, runSessionCmd(t, "list", "--agent", ""))
	if len(recs) != 1 || recs[0].Legacy || recs[0].Malformed || recs[0].SessionPID != os.Getpid() {
		t.Fatalf("--agent \"\" must return only the anonymous v2 row: %+v", recs)
	}
}

// TestSessionList_TableNeutralizesControlCharacters: a self-declared
// agent name carrying a newline and an ANSI escape must not forge a row
// or drive the terminal; each such rune renders as '?'.
func TestSessionList_TableNeutralizesControlCharacters(t *testing.T) {
	sessionRegistryTestEnv(t)
	hostile := "ev\x1b[31mil\nghost  1  alive"
	registerAs(t, os.Getpid(), hostile, t.TempDir())
	formatFlag = "table"
	out := runSessionCmd(t, "list")
	if strings.Contains(out, "\x1b") || strings.Count(out, "\n") != 2 { // header + one row
		t.Fatalf("table must neutralize control characters (got ESC or an extra line):\n%q", out)
	}
	if !strings.Contains(out, "ev?[31mil?ghost") {
		t.Fatalf("control runes must render as '?':\n%q", out)
	}
	regOut := runSessionCmd(t, "register", "--agent", hostile)
	if strings.Contains(regOut, "\x1b") || strings.Count(regOut, "\n") != 1 {
		t.Fatalf("register's confirmation line must neutralize control characters:\n%q", regOut)
	}
}

// TestSessionList_CwdMatchesDirectoryIdentity: a session registered from a
// directory reached through a symlink matches --cwd given as the real
// path AND as the link — identity, not spelling — and the stored cwd is
// the real path.
func TestSessionList_CwdMatchesDirectoryIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	sessionRegistryTestEnv(t)
	real := t.TempDir()
	realResolved, err := filepath.EvalSymlinks(real) // t.TempDir may itself sit under a symlink (macOS /var → /private/var)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "via-link")
	if err := os.Symlink(realResolved, link); err != nil {
		t.Fatal(err)
	}
	registerAs(t, os.Getpid(), "a", link)
	recs := decodeRecords(t, runSessionCmd(t, "list"))
	if len(recs) != 1 || recs[0].Cwd != realResolved {
		t.Fatalf("registration must store the real path, got %+v", recs)
	}
	for _, q := range []string{realResolved, link, link + "/"} {
		got := decodeRecords(t, runSessionCmd(t, "list", "--cwd", q))
		if len(got) != 1 {
			t.Fatalf("--cwd %q must match the session registered via the link: %+v", q, got)
		}
	}
	if got := decodeRecords(t, runSessionCmd(t, "list", "--cwd", t.TempDir())); len(got) != 0 {
		t.Fatalf("an unrelated directory must not match: %+v", got)
	}
}

// TestSessionList_TableMarksUnverifiedPid: a harness pid claim that is not
// in the registering process's ancestry shows as "(unverified pid)" in
// the table, so a human reading it sees the same caveat the JSON carries.
func TestSessionList_TableMarksUnverifiedPid(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ancestry verification is linux-only")
	}
	sessionRegistryTestEnv(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no shell")
	}
	c := exec.Command(sh, "-c", "sleep 60")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	t.Setenv("CLAUDE_PID", strconv.Itoa(c.Process.Pid))
	rec := decodeRecord(t, runSessionCmd(t, "register", "--agent", "claimant"))
	if rec.SessionPIDVerified {
		t.Fatalf("a non-ancestor pid must be unverified: %+v", rec)
	}
	t.Setenv("CLAUDE_PID", "")
	formatFlag = "table"
	out := runSessionCmd(t, "list")
	if !strings.Contains(out, "alive (unverified pid)") {
		t.Fatalf("table must mark the unverified pid:\n%s", out)
	}
}
