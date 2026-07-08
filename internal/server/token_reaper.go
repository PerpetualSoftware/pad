package server

import (
	"log/slog"
	"sync"
	"time"
)

// defaultTokenReaperInterval is how often the background reaper sweeps expired
// short-lived credentials. Hourly balances "bounded row growth" against
// "negligible overhead" — every table it touches is small and every DELETE is
// range-indexed on expires_at. PLAN-1933 DR-5 / TASK-1936.
const defaultTokenReaperInterval = 1 * time.Hour

// tokenReaperConfig captures the runtime knobs + lifecycle for the periodic
// token-reaper loop. Mirrors orphanGCConfig / opLogGCConfig (same lifecycle
// pattern, separate goroutine, separate stop channel). The CleanExpired*
// store methods existed but were never called before this — they leak rows
// without a caller (DR-5).
type tokenReaperConfig struct {
	mu       sync.Mutex
	interval time.Duration
	stop     chan struct{}
	running  bool
}

// SetTokenReaperConfig overrides the default sweep interval (1h). Pass 0 to
// keep the package default. Must be called before StartTokenReaper; calling
// after the loop has started silently no-ops since the goroutine captured the
// value at start.
func (s *Server) SetTokenReaperConfig(interval time.Duration) {
	s.tokenReaper.mu.Lock()
	defer s.tokenReaper.mu.Unlock()
	if interval > 0 {
		s.tokenReaper.interval = interval
	}
}

// StartTokenReaper kicks off the periodic sweep that deletes expired/used
// email-verification tokens, password-reset tokens, sessions, and CLI-auth
// sessions. Idempotent — calling twice silently no-ops.
//
// Started from the real server bootstrap path (cmd/pad/main.go), NOT from
// Server.New, so unit tests that construct a Server don't spawn a background
// goroutine unless they opt in (mirrors StartOrphanGC / StartOpLogGC). The
// loop is tracked by Server.bg so Stop() drains it before the process exits /
// the DB is closed (BUG-842 invariant).
func (s *Server) StartTokenReaper() {
	s.tokenReaper.mu.Lock()
	if s.tokenReaper.running {
		s.tokenReaper.mu.Unlock()
		return
	}
	if s.tokenReaper.interval == 0 {
		s.tokenReaper.interval = defaultTokenReaperInterval
	}
	s.tokenReaper.stop = make(chan struct{})
	s.tokenReaper.running = true
	interval := s.tokenReaper.interval
	stop := s.tokenReaper.stop
	s.tokenReaper.mu.Unlock()

	slog.Info("token reaper started", "interval", interval.String())

	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		defer s.recoverSweeper("token-reaper") // BUG-2071
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.runTokenReaperTick()
			}
		}
	}()
}

// stopTokenReaper signals the loop to exit. Called from Server.Stop().
// Safe to call when the loop never started.
func (s *Server) stopTokenReaper() {
	s.tokenReaper.mu.Lock()
	defer s.tokenReaper.mu.Unlock()
	if !s.tokenReaper.running {
		return
	}
	close(s.tokenReaper.stop)
	s.tokenReaper.running = false
}

// runTokenReaperTick runs one sweep. Each cleaner is independent — a failure
// in one is logged and does not skip the others. Quiet on success (operators
// don't want hourly no-op log lines).
func (s *Server) runTokenReaperTick() {
	if err := s.store.CleanExpiredEmailVerifications(); err != nil {
		slog.Warn("token reaper: clean expired email verifications failed", "error", err)
	}
	if err := s.store.CleanExpiredPasswordResets(); err != nil {
		slog.Warn("token reaper: clean expired password resets failed", "error", err)
	}
	if err := s.store.CleanExpiredSessions(); err != nil {
		slog.Warn("token reaper: clean expired sessions failed", "error", err)
	}
	if err := s.store.CleanExpiredCLIAuthSessions(); err != nil {
		slog.Warn("token reaper: clean expired CLI auth sessions failed", "error", err)
	}
}
