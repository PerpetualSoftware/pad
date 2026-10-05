package server

import (
	"log/slog"
	"sync"
	"time"
)

// The expired pending-install sweep (SPEC-6 U8a, DOC-3371 §2: "expiry (1
// hour, swept)"; TASK-3397). Expired reservations already stop counting
// against the caps, because every count reads expires_at; this sweep is what
// deletes their staged bytes, so an abandoned preview does not keep them
// until some later install happens to sweep (codex round 1). Same lifecycle
// shape as the reminder tick: its own mutex and stop channel, tracked by
// Server.bg so Stop() drains it, started only from the real bootstrap path.

const defaultAppPendingSweepInterval = 5 * time.Minute

type appPendingSweepConfig struct {
	mu       sync.Mutex
	interval time.Duration
	stop     chan struct{}
	running  bool
}

// StartAppPendingSweep starts the sweep. Idempotent.
func (s *Server) StartAppPendingSweep() {
	s.appPendingSweep.mu.Lock()
	if s.appPendingSweep.running {
		s.appPendingSweep.mu.Unlock()
		return
	}
	if s.appPendingSweep.interval == 0 {
		s.appPendingSweep.interval = defaultAppPendingSweepInterval
	}
	s.appPendingSweep.stop = make(chan struct{})
	s.appPendingSweep.running = true
	interval, stop := s.appPendingSweep.interval, s.appPendingSweep.stop
	s.appPendingSweep.mu.Unlock()

	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		defer s.recoverSweeper("app-pending-sweep")
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.runAppPendingSweep()
			}
		}
	}()
}

func (s *Server) stopAppPendingSweep() {
	s.appPendingSweep.mu.Lock()
	defer s.appPendingSweep.mu.Unlock()
	if !s.appPendingSweep.running {
		return
	}
	close(s.appPendingSweep.stop)
	s.appPendingSweep.running = false
}

// runAppPendingSweep is one pass.
func (s *Server) runAppPendingSweep() {
	n, err := s.store.SweepExpiredPendingInstalls()
	if err != nil {
		slog.Warn("apps: pending install sweep failed", "error", err)
		return
	}
	if n > 0 {
		slog.Info("apps: swept expired pending installs", "count", n)
	}
	// Item-action context codes: single-use, two minutes; nothing reads one
	// that is consumed or expired (TASK-3414 U11).
	if _, err := s.store.PruneContextCodes(); err != nil {
		slog.Warn("apps: context code prune failed", "error", err)
	}
}
