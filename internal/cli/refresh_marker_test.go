package cli

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// BUG-3215. install-refresh.sh stops the server, installs, and restarts it with
// the argv it captured. Any pad CLI call in between found nothing answering
// and auto-started a server with default flags, which then held the port
// against the refresh's own restart. The refresh now writes a marker for that
// window and EnsureServer defers to it.
//
// Every test here counts SPAWNS, because the defect is a spawn: an end state of
// "a healthy server on the port" is produced identically by the refresh's
// restart and by the stray, so only the counter tells them apart.

// refreshTestConfig returns a locally-managed config pointed at a port nothing
// listens on, and a spawn counter installed in place of the real spawn.
func refreshTestConfig(t *testing.T) (*config.Config, *int32) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	cfg := &config.Config{
		Host:           "127.0.0.1",
		Port:           port,
		Mode:           config.ModeLocal,
		LoadedFromFile: true,
		DataDir:        t.TempDir(),
	}

	var spawns int32
	origSpawn, origPoll := spawnServer, refreshPoll
	spawnServer = func(*config.Config) error {
		atomic.AddInt32(&spawns, 1)
		return nil
	}
	refreshPoll = 20 * time.Millisecond
	t.Cleanup(func() { spawnServer, refreshPoll = origSpawn, origPoll })
	return cfg, &spawns
}

func writeMarker(t *testing.T, cfg *config.Config, pid, port int, deadline time.Time) {
	t.Helper()
	body := fmt.Sprintf(`{"pid": %d, "port": %d, "deadline": %d}`, pid, port, deadline.Unix())
	if err := os.WriteFile(RefreshMarkerPath(cfg), []byte(body), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

// serveHealthAt starts answering the health probe on the config's port.
func serveHealthAt(t *testing.T, cfg *config.Config) {
	t.Helper()
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	if err != nil {
		t.Fatalf("listen on %d: %v", cfg.Port, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// The control for everything below: with no marker, an unhealthy server is
// auto-started, exactly as before. Without this leg the counter could be
// broken and every "no spawn" assertion would pass.
func TestEnsureServer_SpawnsWithoutAMarker(t *testing.T) {
	cfg, spawns := refreshTestConfig(t)
	if err := EnsureServer(cfg); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	if got := atomic.LoadInt32(spawns); got != 1 {
		t.Fatalf("spawns = %d, want 1: the counter does not observe the ordinary auto-start", got)
	}
}

func TestEnsureServer_WaitsForALiveRefreshInsteadOfSpawning(t *testing.T) {
	cfg, spawns := refreshTestConfig(t)
	writeMarker(t, cfg, os.Getpid(), cfg.Port, time.Now().Add(30*time.Second))

	// The refresh's restart comes up while the CLI is waiting.
	up := time.AfterFunc(300*time.Millisecond, func() { serveHealthAt(t, cfg) })
	defer up.Stop()

	start := time.Now()
	if err := EnsureServer(cfg); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	if got := atomic.LoadInt32(spawns); got != 0 {
		t.Fatalf("spawns = %d, want 0: the CLI auto-started a server over a refresh in progress", got)
	}
	// It waited for the restart, rather than returning early with nothing up.
	if waited := time.Since(start); waited < 250*time.Millisecond {
		t.Fatalf("returned after %v, before the restart was up", waited)
	}
	if !isServerHealthy(cfg.Host, cfg.Port) {
		t.Fatal("returned nil with nothing answering")
	}
}

func TestEnsureServer_RefusesRatherThanSpawnsWhenALiveRefreshMissesItsDeadline(t *testing.T) {
	cfg, spawns := refreshTestConfig(t)
	writeMarker(t, cfg, os.Getpid(), cfg.Port, time.Now().Add(1500*time.Millisecond)) // the deadline is whole seconds

	err := EnsureServer(cfg)
	if err == nil {
		t.Fatal("EnsureServer returned nil with nothing answering and the refresh past its deadline")
	}
	if got := atomic.LoadInt32(spawns); got != 0 {
		t.Fatalf("spawns = %d, want 0: a refresh that is still alive owns the port", got)
	}
	msg := err.Error()
	for _, want := range []string{fmt.Sprintf("pid %d", os.Getpid()), fmt.Sprintf("kill %d", os.Getpid()), RefreshMarkerPath(cfg)} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not name %q: %s", want, msg)
		}
	}
}

// A marker that is not live is not honoured. Each case must spawn, because a
// stale marker that blocked auto-start would turn a crashed refresh into a
// permanently unstartable CLI.
func TestEnsureServer_IgnoresAMarkerThatIsNotLive(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, cfg *config.Config)
	}{
		{"writer is dead", func(t *testing.T, cfg *config.Config) {
			writeMarker(t, cfg, os.Getpid(), cfg.Port, time.Now().Add(30*time.Second))
			orig := refreshOwnerGone
			refreshOwnerGone = func(int) bool { return true }
			t.Cleanup(func() { refreshOwnerGone = orig })
		}},
		{"deadline already passed", func(t *testing.T, cfg *config.Config) {
			writeMarker(t, cfg, os.Getpid(), cfg.Port, time.Now().Add(-time.Second))
		}},
		// A seat's private server, or an e2e server, on another port is not in
		// the refresh's window.
		{"another port", func(t *testing.T, cfg *config.Config) {
			writeMarker(t, cfg, os.Getpid(), cfg.Port+1, time.Now().Add(30*time.Second))
		}},
		{"unparseable", func(t *testing.T, cfg *config.Config) {
			if err := os.WriteFile(RefreshMarkerPath(cfg), []byte(`{"pid": `), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, spawns := refreshTestConfig(t)
			tc.setup(t, cfg)
			if err := EnsureServer(cfg); err != nil {
				t.Fatalf("EnsureServer: %v", err)
			}
			if got := atomic.LoadInt32(spawns); got != 1 {
				t.Fatalf("spawns = %d, want 1: a marker that is not live blocked the auto-start", got)
			}
		})
	}
}

// The refresh ended (its trap removed the marker) without anything answering:
// the auto-start is legitimate again, and the waiting call makes it.
func TestEnsureServer_SpawnsWhenTheMarkerIsRemovedMidWait(t *testing.T) {
	cfg, spawns := refreshTestConfig(t)
	writeMarker(t, cfg, os.Getpid(), cfg.Port, time.Now().Add(30*time.Second))
	rm := time.AfterFunc(200*time.Millisecond, func() { _ = os.Remove(RefreshMarkerPath(cfg)) })
	defer rm.Stop()

	if err := EnsureServer(cfg); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	if got := atomic.LoadInt32(spawns); got != 1 {
		t.Fatalf("spawns = %d, want 1", got)
	}
}

// The marker is found by the SAME DataDir rule the script uses, from any cwd:
// PAD_DB_PATH's directory, else PAD_DATA_DIR, else ~/.pad. A CLI started in
// another directory, or linked to another workspace, must find the one file.
func TestRefreshMarkerPath_FollowsTheDataDirRuleFromAnyCwd(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	dbDir := t.TempDir()
	elsewhere := t.TempDir()

	t.Setenv("HOME", home)
	t.Setenv("PAD_DATA_DIR", "")
	t.Setenv("PAD_DB_PATH", "")
	t.Chdir(elsewhere)

	load := func() string {
		t.Helper()
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("config.Load: %v", err)
		}
		return RefreshMarkerPath(cfg)
	}

	if got, want := load(), filepath.Join(home, ".pad", RefreshMarkerName); got != want {
		t.Errorf("default: %s, want %s", got, want)
	}
	t.Setenv("PAD_DATA_DIR", dataDir)
	if got, want := load(), filepath.Join(dataDir, RefreshMarkerName); got != want {
		t.Errorf("PAD_DATA_DIR: %s, want %s", got, want)
	}
	// PAD_DB_PATH wins over PAD_DATA_DIR, as config.Load applies it second.
	t.Setenv("PAD_DB_PATH", filepath.Join(dbDir, "pad.db"))
	if got, want := load(), filepath.Join(dbDir, RefreshMarkerName); got != want {
		t.Errorf("PAD_DB_PATH: %s, want %s", got, want)
	}
}

// The marker is WRITTEN by bash and READ here, so the two sides share a format
// only if a test holds them to one. This reads the script's own printf format
// string, renders it the way printf does, and requires the reader to take the
// result as a live marker for this port. A renamed key or a changed shape on
// either side fails here instead of silently turning the marker off.
func TestReadRefreshMarker_ParsesTheScriptsOwnFormat(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install-refresh.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	re := regexp.MustCompile(`printf '(\{"pid"[^']*)' "\$\$" "\$PORT"`)
	m := re.FindAllSubmatch(src, -1)
	if len(m) != 1 {
		t.Fatalf("found %d marker printf lines in the script, want exactly 1", len(m))
	}
	format := strings.ReplaceAll(string(m[0][1]), `\n`, "\n")

	cfg, _ := refreshTestConfig(t)
	body := fmt.Sprintf(format, os.Getpid(), cfg.Port, time.Now().Add(30*time.Second).Unix())
	if err := os.WriteFile(RefreshMarkerPath(cfg), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, st := readRefreshMarker(cfg)
	if st != refreshLive {
		t.Fatalf("the script's marker %q reads as state %d, want live", body, st)
	}
	if got.PID != os.Getpid() || got.Port != cfg.Port {
		t.Fatalf("parsed %+v from %q", got, body)
	}
}
