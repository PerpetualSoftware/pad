package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// RefreshMarkerName is the file scripts/install-refresh.sh writes, beside the
// PID file, for the window in which it has stopped the server and not yet seen
// its replacement answer (BUG-3215).
//
// That window used to belong to whichever process asked first. Any pad CLI
// call landing in it found no server and auto-started one with DEFAULT flags,
// running the CALLER's binary (a worktree build, or the old inode before the
// install's rename) from the caller's cwd. The refresh's own restart then
// could not bind, and the box was left serving the wrong argv and possibly the
// wrong commit. The marker hands the window back to the refresh: EnsureServer
// waits for it instead of spawning.
const RefreshMarkerName = "refresh.lock"

// refreshMarker is the marker's content. The script writes it with printf, so
// the shape is kept flat and every member is a plain integer.
type refreshMarker struct {
	// PID of the refresh script. A marker whose writer is gone describes
	// nothing: crash-only, so no one else removes it.
	PID int `json:"pid"`
	// Port the refresh will restart the server on. A CLI pointed at another
	// port (a seat's private server, an e2e server) is not in this window.
	Port int `json:"port"`
	// Deadline, unix seconds. A pid alone can be reused, so liveness is the
	// pid AND the deadline; a marker past it is not honoured by a new call.
	Deadline int64 `json:"deadline"`
}

// RefreshMarkerPath is where the marker lives for this config: the DataDir
// the PID file is in. The script resolves the same directory by the same
// rule (PAD_DB_PATH's directory, else PAD_DATA_DIR, else ~/.pad); neither
// the cwd nor the linked workspace takes part, so every CLI on the box finds
// the one marker.
func RefreshMarkerPath(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, RefreshMarkerName)
}

// refreshState is what a read of the marker says about THIS config's server.
type refreshState int

const (
	// refreshNone: no marker, an unreadable one, another port's, a dead
	// writer's, or one past its deadline. Nothing to wait for.
	refreshNone refreshState = iota
	// refreshLive: a refresh of this port is in progress.
	refreshLive
	// refreshExpired: the marker's writer is alive but its deadline has
	// passed. Only meaningful to a caller that was already waiting.
	refreshExpired
)

// Package-level so the tests can drive the clock and the pid probe. Neither
// is swapped outside tests.
var (
	refreshNow       = time.Now
	refreshOwnerGone = processIsGone
)

func readRefreshMarker(cfg *config.Config) (refreshMarker, refreshState) {
	data, err := os.ReadFile(RefreshMarkerPath(cfg))
	if err != nil {
		return refreshMarker{}, refreshNone
	}
	var m refreshMarker
	// An unparseable marker is read as absent, not as live: it cannot name a
	// writer whose liveness bounds it, so honouring it could block auto-start
	// for ever. The script writes it by rename, so a half-written file is
	// never observed.
	if err := json.Unmarshal(data, &m); err != nil || m.PID <= 0 || m.Port <= 0 {
		return refreshMarker{}, refreshNone
	}
	if m.Port != cfg.Port {
		return m, refreshNone
	}
	if refreshOwnerGone(m.PID) {
		return m, refreshNone
	}
	if refreshNow().Unix() >= m.Deadline {
		return m, refreshExpired
	}
	return m, refreshLive
}

// refreshPoll is how often a waiting call re-reads the marker and the health
// probe.
var refreshPoll = 200 * time.Millisecond

// awaitRefresh defers to a refresh in progress on this config's port. It
// returns nil when there is nothing to wait for, or when the server answered
// during the wait; the caller re-probes health and spawns only if it still
// has to. It returns an error, and the caller must NOT spawn, when the
// refresh's deadline passes while its writer is still alive: the refresh is
// still acting on the port, and an auto-start there is the defect.
func awaitRefresh(cfg *config.Config) error {
	if _, st := readRefreshMarker(cfg); st != refreshLive {
		// An expired marker found by a call that was not waiting is not
		// honoured: pid AND deadline is the liveness rule.
		return nil
	}
	for {
		if isServerHealthy(cfg.Host, cfg.Port) {
			return nil
		}
		time.Sleep(refreshPoll)
		cur, st := readRefreshMarker(cfg)
		switch st {
		case refreshNone:
			// Removed, or its writer died: the refresh is over, and if
			// nothing answers now an auto-start is legitimate again.
			return nil
		case refreshExpired:
			return fmt.Errorf(
				"a server refresh (pid %d) is in progress on port %d and the server has not answered by its deadline; "+
					"not auto-starting over it. Check that refresh's output; if it is stuck, stop it (kill %d) and retry. "+
					"Marker: %s",
				cur.PID, cur.Port, cur.PID, RefreshMarkerPath(cfg))
		}
	}
}
