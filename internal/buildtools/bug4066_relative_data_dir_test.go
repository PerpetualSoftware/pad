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

	// The restarted stub records whether the marker exists where the SERVER
	// looks, at the moment it starts inside the stop-to-restart window.
	probeOut := filepath.Join(home, "probe.out")
	res := runScriptIn(t, e, callerDir, built, installed, commit,
		"STUB_MARKER_PROBE="+filepath.Join(serverDir, "data", "refresh.lock"), "STUB_PROBE_OUT="+probeOut)
	if res.err != nil {
		t.Fatalf("refresh failed: %v\nstdout=%s\nstderr=%s", res.err, res.stdout, res.stderr)
	}
	if b, err := os.ReadFile(probeOut); err != nil || strings.TrimSpace(string(b)) != "present" {
		t.Errorf("the marker was not present at <server cwd>/data/refresh.lock when the restart started: %q (err=%v)", b, err)
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
// carries the warning. It names the SERVER's anchored, cleaned path, not the
// caller's (BUG-4066 r1: the caller's cwd here differs from the server's).
func TestBUG4066_ScriptWarnsAboutARelativeDataDirBeforeStopping(t *testing.T) {
	for _, tc := range []struct{ name, dataDir, dbPath, serverRel, want string }{
		{"data", "data", "", "data", "PAD_DATA_DIR is relative (data); this process resolves it to %s (the server's directory; from this shell's directory it would be %s)."},
		{"./data", "./data", "", "data", "PAD_DATA_DIR is relative (./data); this process resolves it to %s (the server's directory; from this shell's directory it would be %s)."},
		{"./x/../data", "./x/../data", "", "data", "PAD_DATA_DIR is relative (./x/../data); this process resolves it to %s (the server's directory; from this shell's directory it would be %s)."},
		{"PAD_DB_PATH names the dir", "", "x/pad.db", "x", "PAD_DB_PATH is relative (x/pad.db); this process resolves the data dir to %s (the server's directory; from this shell's directory it would be %s)."},
		{"PAD_DB_PATH with dots", "", "./y/../x/pad.db", "x", "PAD_DB_PATH is relative (./y/../x/pad.db); this process resolves the data dir to %s (the server's directory; from this shell's directory it would be %s)."},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
				argvLog: filepath.Join(home, "argv.log"), port: port, home: home, dataDir: tc.dataDir}
			var extra []string
			if tc.dbPath != "" {
				extra = []string{"PAD_DB_PATH=" + tc.dbPath}
			}
			// A directory where the log file goes, under the server's anchored data dir.
			if err := os.MkdirAll(filepath.Join(serverDir, tc.serverRel, "logs", "server.log"), 0o755); err != nil {
				t.Fatal(err)
			}
			pre := exec.Command(built, "server", "start", "--host", "127.0.0.1")
			pre.Env = append(e.env(), extra...)
			pre.Dir = serverDir
			startPreServer(t, pre, "127.0.0.1", port)
			prePid := strconv.Itoa(pre.Process.Pid)

			res := runScriptIn(t, e, callerDir, built, installed, commit, extra...)
			if res.err == nil {
				t.Fatalf("refresh succeeded with an unwritable log\nstdout=%s\nstderr=%s", res.stdout, res.stderr)
			}
			// Both paths in ONE line, and that line once: phase 1's contract is
			// one warning per process (BUG-4066 r4 item 1).
			want := fmt.Sprintf(tc.want, filepath.Join(serverDir, tc.serverRel), filepath.Join(callerDir, tc.serverRel))
			line := ""
			for _, l := range strings.Split(res.stderr, "\n") {
				if strings.Contains(l, "is relative (") {
					if line != "" {
						t.Errorf("the relative data-dir warning printed twice; stderr=%s", res.stderr)
					}
					line = l
				}
			}
			if !strings.Contains(line, want) {
				t.Errorf("the warning line does not name the server's anchored path and the caller's reading; want %q in %q", want, line)
			}
			if n := strings.Count(res.stderr, "directory it would be"); n != 1 {
				t.Errorf("the shell's-directory note printed %d times, want once, inside the warning; stderr=%s", n, res.stderr)
			}
			if got, err := pidAnswersAt(port); err != nil || got != prePid {
				t.Fatalf("the running server did not survive: %q, %v; want %s", got, err, prePid)
			}
		})
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
