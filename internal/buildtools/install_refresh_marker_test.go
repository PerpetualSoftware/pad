package buildtools

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// BUG-3215. Between the stop and the restart nothing serves the port, and a
// pad CLI call in that window used to auto-start a server of its own (the
// caller's binary, cwd and default flags) that then held the port against the
// refresh. The script now claims the window with a marker the CLI's
// auto-start defers to, and checks WHO holds the port both before it spawns
// the restart and after the probe.

// markerRun runs the script and returns its result plus the pid of the bash
// process that ran it: the pid the marker must name, because the CLI's
// liveness check probes it.
func markerRun(t *testing.T, e stubEnv, built, installed, commit string, extraEnv ...string) (runResult, int) {
	t.Helper()
	cmd := exec.Command("bash", scriptPath(t), built, installed, commit)
	cmd.Env = append(e.env(), extraEnv...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatalf("start script: %v", err)
	}
	pid := cmd.Process.Pid
	err := cmd.Wait()
	return runResult{stdout: out.String(), stderr: errb.String(), err: err}, pid
}

func countServerStarts(t *testing.T, argvLog string) int {
	t.Helper()
	b, err := os.ReadFile(argvLog)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read argv log: %v", err)
	}
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.Contains(l, "server start") {
			n++
		}
	}
	return n
}

// The marker exists for the restart, names this script, this port and a
// short deadline, lives where the CLI will look (the DataDir rule, not the
// cwd), and is gone when the script exits. Run once with the default
// location and once with PAD_DATA_DIR and PAD_DB_PATH, because a marker the
// CLI resolves somewhere else is a marker nobody honours.
func TestInstallRefresh_HoldsTheRefreshMarkerForTheRestartWindow(t *testing.T) {
	requireScriptDeps(t)
	cases := []struct {
		name string
		// env returns extra environment and the marker path it implies.
		env func(home string) ([]string, string)
	}{
		{"default data dir", func(home string) ([]string, string) {
			return nil, filepath.Join(home, ".pad", "refresh.lock")
		}},
		{"PAD_DATA_DIR", func(home string) ([]string, string) {
			d := filepath.Join(home, "elsewhere")
			return []string{"PAD_DATA_DIR=" + d}, filepath.Join(d, "refresh.lock")
		}},
		{"PAD_DB_PATH wins over PAD_DATA_DIR", func(home string) ([]string, string) {
			d := filepath.Join(home, "dbdir")
			return []string{"PAD_DATA_DIR=" + filepath.Join(home, "loser"), "PAD_DB_PATH=" + filepath.Join(d, "pad.db")},
				filepath.Join(d, "refresh.lock")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, dir := t.TempDir(), t.TempDir()
			name := uniqueName(t)
			defer killStub(t, name)
			port := freePort(t)
			const commit = "abc1234"

			built := installStub(t, dir, name)
			installed := filepath.Join(home, "bin", name)
			markerLog := filepath.Join(home, "marker.log")
			extra, markerPath := tc.env(home)
			extra = append(extra, "STUB_MARKER_LOG="+markerLog, "STUB_MARKER_PATH="+markerPath)
			e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
				argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

			pre := exec.Command(built, "server", "start")
			pre.Env = append(e.env(), extra...)
			startPreServer(t, pre, "127.0.0.1", port)

			before := time.Now().Unix()
			res, scriptPID := markerRun(t, e, built, installed, commit, extra...)
			if res.err != nil {
				t.Fatalf("refresh failed: %v\nstdout=%s\nstderr=%s", res.err, res.stdout, res.stderr)
			}

			logged, err := os.ReadFile(markerLog)
			if err != nil {
				t.Fatalf("read marker log: %v", err)
			}
			lines := strings.Split(strings.TrimSpace(string(logged)), "\n")
			if len(lines) != 2 {
				t.Fatalf("want 2 server starts logged (the pre-server and the restart), got %d: %q", len(lines), lines)
			}
			// PREMISE: before the refresh there is no marker, so the one the
			// restart sees was written by the refresh.
			if lines[0] != "absent" {
				t.Fatalf("premise: a marker existed before the refresh: %q", lines[0])
			}
			var m struct {
				PID      int   `json:"pid"`
				Port     int   `json:"port"`
				Deadline int64 `json:"deadline"`
			}
			if err := json.Unmarshal([]byte(lines[1]), &m); err != nil {
				t.Fatalf("the restart did not find a readable marker at %s: %q (%v)", markerPath, lines[1], err)
			}
			if m.PID != scriptPID {
				t.Errorf("marker pid = %d, want the script's pid %d", m.PID, scriptPID)
			}
			if m.Port != port {
				t.Errorf("marker port = %d, want %d", m.Port, port)
			}
			// PAD_PROBE_TIMEOUT is 8 in these tests, so the deadline is
			// now + 18s: short, and not already past.
			if m.Deadline <= before || m.Deadline > before+19+5 {
				t.Errorf("marker deadline %d is not a short one after %d", m.Deadline, before)
			}
			if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
				t.Errorf("the marker outlived the refresh (stat err=%v)", err)
			}
		})
	}
}

// Day 79, in order: the refresh stops the server, and before it restarts it a
// pad CLI call auto-starts a stray on the port (a binary that predates the
// marker, or a start by hand). The restart cannot bind, so it is not spawned,
// and the holder is named.
func TestInstallRefresh_NamesAStrayThatTookThePortBeforeTheRestart(t *testing.T) {
	requireScriptDeps(t)
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	argvLog := filepath.Join(home, "argv.log")
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: argvLog, port: port, home: home}

	// The pre-server hands its port to a stray when the refresh stops it.
	pre := exec.Command(built, "server", "start")
	pre.Env = append(e.env(), "STUB_STRAY_ON_TERM=1")
	startPreServer(t, pre, "127.0.0.1", port)

	res := runScript(t, e, built, installed, commit)
	if res.err == nil {
		t.Fatalf("refresh reported success with a stray on the port; stdout=%s", res.stdout)
	}
	if !strings.Contains(res.stderr, "was taken after the old server stopped and before the restart") {
		t.Errorf("the refusal does not say the port was taken in the window; stderr=%s", res.stderr)
	}
	if !strings.Contains(res.stderr, name+" server start") {
		t.Errorf("the refusal does not name the holder's argv; stderr=%s", res.stderr)
	}
	// Only the pre-server ever started: no restart was spawned into a port
	// it could not bind.
	if got := countServerStarts(t, argvLog); got != 1 {
		t.Errorf("server starts logged = %d, want 1 (the pre-server alone)", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".pad", "refresh.lock")); !os.IsNotExist(err) {
		t.Errorf("the marker outlived a refusal (stat err=%v)", err)
	}
}

// The probe answers for whoever holds the address. Here a stray beats the
// restart to the port after it is spawned, so the probe PASSES, and the only
// thing that can tell this from a good refresh is who is listening. Before
// this check the script printed success over the stray.
func TestInstallRefresh_RefusesSuccessWhenTheListenerIsNotTheRestart(t *testing.T) {
	requireScriptDeps(t, "ss")
	home, dir := t.TempDir(), t.TempDir()
	name := uniqueName(t)
	defer killStub(t, name)
	port := freePort(t)
	const commit = "abc1234"

	built := installStub(t, dir, name)
	installed := filepath.Join(home, "bin", name)
	e := stubEnv{version: "pad version dev (" + commit + " x)", healthy: "1",
		argvLog: filepath.Join(home, "argv.log"), port: port, home: home}

	// No server running: the refresh starts one with defaults, and that
	// start is beaten to the port.
	res, _ := markerRun(t, e, built, installed, commit, "STUB_STRAY_ON_START=1")
	if res.err == nil {
		t.Fatalf("refresh reported success while a stray served the port; stdout=%s", res.stdout)
	}
	if strings.Contains(res.stdout, "server restarted and answering") {
		t.Errorf("the script claimed the restart answered; stdout=%s", res.stdout)
	}
	if !strings.Contains(res.stderr, "not by this refresh's restart") {
		t.Errorf("the refusal does not say the listener is not the restart; stderr=%s", res.stderr)
	}
	// PREMISE: the probe itself passed. Otherwise this is the ordinary
	// "did not answer" failure and the ownership check was never reached.
	if strings.Contains(res.stderr, "did not answer on") {
		t.Fatalf("premise: the probe failed, so the stray was not answering; stderr=%s", res.stderr)
	}
}
