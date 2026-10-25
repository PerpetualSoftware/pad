package buildtools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// BUG-4066 (lead ruling 4b4cb4ff): with a relative data dir and a server whose
// cwd is not the caller's, the refresh marker used to be written under the
// CALLER's cwd and looked for under the server's after the cd, so it leaked.
// It is now anchored where the log is (${SERVER_CWD:-$PWD}): written and
// cleaned in one place, and nothing lands under the caller's cwd.
func TestBUG4066_RefreshMarkerIsAnchoredAndCleanedUp(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	serverDir, callerDir := realDir(t, t.TempDir()), realDir(t, t.TempDir())
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home, dataDir: "data"}
	pre := exec.Command(built, "server", "start", "--host", "127.0.0.1")
	pre.Env = e.env()
	pre.Dir = serverDir
	startPreServer(t, pre, "127.0.0.1", port)

	res := runScriptIn(t, e, callerDir, built, installed, commit)
	if res.err != nil {
		t.Fatalf("refresh failed: %v\nstdout=%s\nstderr=%s", res.err, res.stdout, res.stderr)
	}
	for _, p := range []string{filepath.Join(callerDir, "data", "refresh.lock"), filepath.Join(serverDir, "data", "refresh.lock")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("the refresh marker was left behind at %s (stat err=%v)", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(callerDir, "data")); !os.IsNotExist(err) {
		t.Errorf("the refresh wrote under the caller's cwd: %s exists (stat err=%v)", filepath.Join(callerDir, "data"), err)
	}
}

// The script warns about a relative data dir BEFORE anything is stopped: a
// refusal that happens before the stop (the unwritable-log check) still
// carries the warning.
func TestBUG4066_ScriptWarnsAboutARelativeDataDirBeforeStopping(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	serverDir := realDir(t, t.TempDir())
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home, dataDir: "data"}
	// A directory where the log file goes, under the server's anchored data dir.
	if err := os.MkdirAll(filepath.Join(serverDir, "data", "logs", "server.log"), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := exec.Command(built, "server", "start", "--host", "127.0.0.1")
	pre.Env = e.env()
	pre.Dir = serverDir
	startPreServer(t, pre, "127.0.0.1", port)
	prePid := strconv.Itoa(pre.Process.Pid)

	res := runScriptIn(t, e, serverDir, built, installed, commit)
	if res.err == nil {
		t.Fatalf("refresh succeeded with an unwritable log\nstdout=%s\nstderr=%s", res.stdout, res.stderr)
	}
	want := "PAD_DATA_DIR is relative (data); this process resolves it to " + filepath.Join(serverDir, "data") + "."
	if !strings.Contains(res.stderr, want) {
		t.Errorf("no relative-data-dir warning before the refusal; want %q in stderr=%s", want, res.stderr)
	}
	if got, err := pidAnswersAt(port); err != nil || got != prePid {
		t.Fatalf("the running server did not survive: %q, %v; want %s", got, err, prePid)
	}
}

// config_value reads config.toml from the ANCHORED data dir (BUG-4066).
//
// Function-level ON PURPOSE: an end-to-end version (PAD_PORT unset, the port
// only in <server cwd>/data/config.toml) made the script's pre-capture target
// discovery fall back to the default port 7777 and stop the box's real
// shared server, twice (BUG-4066 trail). This extracts data_dir,
// anchored_data_dir and config_value from the script and calls config_value
// with SERVER_CWD set, so no server is discovered, stopped or started.
func TestBUG4066_ConfigValueReadsTheAnchoredConfig(t *testing.T) {
	requireScriptDeps(t)
	serverDir, callerDir := realDir(t, t.TempDir()), realDir(t, t.TempDir())
	for _, d := range []struct{ root, port string }{{serverDir, "4242"}, {callerDir, "1111"}} {
		if err := os.MkdirAll(filepath.Join(d.root, "data"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d.root, "data", "config.toml"), []byte("port = "+d.port+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "install-refresh.sh"))
	if err != nil {
		t.Fatal(err)
	}
	extract := `sed -n -e '/^data_dir() {/,/^}/p' -e '/^anchored_data_dir() {/,/^}/p' -e '/^config_value() {/,/^}/p' "$1"`
	for _, tc := range []struct {
		name, serverCwd, want string
	}{
		{"anchored to the server's cwd", serverDir, "4242"},
		{"no server cwd: here", "", "1111"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", `eval "$(`+extract+`)"; config_value port`, "x", script)
			cmd.Dir = callerDir
			cmd.Env = append(os.Environ(), "PAD_DATA_DIR=data", "PAD_DB_PATH=", "SERVER_CWD="+tc.serverCwd)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("config_value: %v", err)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Fatalf("config_value port = %q, want %q (read from the wrong data dir)", got, tc.want)
			}
		})
	}
}
