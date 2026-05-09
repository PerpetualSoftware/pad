package server

import (
	"log/slog"
	"sync"
	"time"
)

// Default knobs for the periodic Yjs op-log prune sweeper (TASK-1309).
// Operators override via env vars wired in cmd/pad/main.go.
//
//   - Interval: how often the sweep runs. 1 hour balances "responsive
//     cleanup" against "negligible overhead". A long-lived item that
//     hasn't been edited in days won't see its op-log grow further,
//     so a slow sweep is fine.
//   - MinAge: minimum age of an op-log row before it's eligible for
//     pruning. 24 hours covers every realistic disconnect-and-reconnect
//     window (mobile suspend, lock-screen, travel-on-flaky-wifi). Rows
//     younger than this stay so a peer that just dropped can replay
//     normally on reconnect.
const (
	defaultOpLogGCInterval = 1 * time.Hour
	defaultOpLogGCMinAge   = 24 * time.Hour
)

// opLogGCConfig captures the runtime knobs for the periodic loop.
// Stored on Server via SetOpLogGCConfig so cmd/pad and tests can
// override defaults independently. Mirrors the structure of
// orphanGCConfig (TASK-886) — same lifecycle pattern, separate
// goroutine, separate stop channel.
type opLogGCConfig struct {
	mu       sync.Mutex
	interval time.Duration
	minAge   time.Duration
	stop     chan struct{}
	running  bool
}

// SetOpLogGCConfig overrides the default sweep interval (1h) and
// minimum row age (24h). Pass 0 for either to keep the package
// default. Must be called before StartOpLogGC; calling after the
// loop has started silently no-ops since the goroutine captured the
// values at start.
func (s *Server) SetOpLogGCConfig(interval, minAge time.Duration) {
	s.opLogGC.mu.Lock()
	defer s.opLogGC.mu.Unlock()
	if interval > 0 {
		s.opLogGC.interval = interval
	}
	if minAge > 0 {
		s.opLogGC.minAge = minAge
	}
}

// StartOpLogGC kicks off the periodic prune-sweep loop. Idempotent —
// calling twice silently no-ops. Must be called AFTER
// SetCollabRoomManager; the loop checks for a wired room manager
// before each tick and just returns if collab isn't enabled, so a
// no-collab build is safe.
//
// The loop is tracked by Server.bg so Stop() drains it before the
// process exits / SQLite is closed (BUG-842 invariant — same
// reasoning as orphan GC).
func (s *Server) StartOpLogGC() {
	s.opLogGC.mu.Lock()
	if s.opLogGC.running {
		s.opLogGC.mu.Unlock()
		return
	}
	if s.opLogGC.interval == 0 {
		s.opLogGC.interval = defaultOpLogGCInterval
	}
	if s.opLogGC.minAge == 0 {
		s.opLogGC.minAge = defaultOpLogGCMinAge
	}
	s.opLogGC.stop = make(chan struct{})
	s.opLogGC.running = true
	interval := s.opLogGC.interval
	minAge := s.opLogGC.minAge
	stop := s.opLogGC.stop
	s.opLogGC.mu.Unlock()

	slog.Info("op-log GC started",
		"interval", interval.String(),
		"min_age", minAge.String(),
	)

	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.runOpLogGCTick(minAge)
			}
		}
	}()
}

// stopOpLogGC signals the loop to exit. Called from Server.Stop().
// Safe to call when the loop never started.
func (s *Server) stopOpLogGC() {
	s.opLogGC.mu.Lock()
	defer s.opLogGC.mu.Unlock()
	if !s.opLogGC.running {
		return
	}
	close(s.opLogGC.stop)
	s.opLogGC.running = false
}

// runOpLogGCTick is one tick of the periodic loop. No outer timeout
// because PruneSweep itself is bounded by the size of the candidate
// list (one DB query + one DELETE per item, both indexed) — unlike
// orphan GC which can scan the whole filesystem. If a future
// regression makes a sweep slow, that's a signal to add a deadline
// here, not a default.
//
// Logged at info on success, warn on failure. Quiet (no log line)
// when nothing was eligible — operators don't want hourly "0 rows
// pruned" lines on a quiet server.
func (s *Server) runOpLogGCTick(minAge time.Duration) {
	if s.collab == nil {
		return
	}
	res, err := s.collab.PruneSweep(minAge)
	if err != nil {
		slog.Warn("op-log GC sweep failed", "error", err)
		return
	}
	if res.RowsPruned == 0 && res.Errors == 0 && res.ItemsSkipped == 0 {
		return
	}
	slog.Info("op-log GC sweep",
		"items_scanned", res.ItemsScanned,
		"items_pruned", res.ItemsPruned,
		"items_skipped", res.ItemsSkipped,
		"rows_pruned", res.RowsPruned,
		"errors", res.Errors,
	)
}
