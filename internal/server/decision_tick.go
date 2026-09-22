package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/decision"
)

// The decision tick: the half of TASK-3117 that does the provider calls.
//
// Writes only mark an evaluation owed (a decision_jobs row, in the write's own
// transaction — the day-73 async-only ruling). This loop claims owed jobs and
// runs them. Same settled sweeper shape as reminder_tick.go: config struct
// with its own mutex and stop channel, tracked by Server.bg so Stop() drains
// it before the DB closes, recoverSweeper on the goroutine, and an injectable
// tick channel for tests.
//
// With no provider configured the runner is nil, the store has no resolver,
// no job row is ever written, and this loop is never started.

const (
	// defaultDecisionTickInterval bounds how stale a decision is after a
	// write: at most one interval plus the provider's latency. Decisions are
	// advisory reads, not alarms, so a few seconds is well inside what a
	// reader would notice, and an idle tick costs one indexed scan of an
	// empty table.
	defaultDecisionTickInterval = 5 * time.Second

	// defaultDecisionTickLimit caps jobs per pass. Each job is a provider
	// round trip, evaluated sequentially, so this bounds one pass's wall
	// time rather than throughput.
	defaultDecisionTickLimit = 20

	// defaultDecisionClaimLease must outlast ONE JOB, not one pass: the
	// runner claims jobs one at a time (codex round 4), so a lease starts
	// when its own evaluation does. A lease that lapses mid-call hands the
	// job to another instance and buys a duplicate provider call.
	//
	// THE RECEIPT: typesafe.go's retry policy is maxAttempts=4 at a 60s
	// requestTimeout, with up to maxRetryAfter=30s between attempts — a
	// worst case of 4*60 + 3*30 = 330s per job. 10 minutes is ~1.8x that.
	// A provider with a longer retry budget needs this raised with it.
	defaultDecisionClaimLease = 10 * time.Minute
)

type decisionTickConfig struct {
	mu       sync.Mutex
	runner   *decision.Runner
	interval time.Duration
	limit    int
	lease    time.Duration
	runnerID string
	stop     chan struct{}
	cancel   context.CancelFunc
	running  bool
	tick     <-chan time.Time
}

// SetDecisionRunner attaches the decision runner and wires its registry into
// the store's write doors. A nil runner (no provider configured) removes any
// resolver, so the doors enqueue nothing.
func (s *Server) SetDecisionRunner(r *decision.Runner) {
	s.decisionTick.mu.Lock()
	s.decisionTick.runner = r
	s.decisionTick.mu.Unlock()
	if s.store != nil {
		r.Install(s.store)
	}
}

func (s *Server) decisionRunner() *decision.Runner {
	s.decisionTick.mu.Lock()
	defer s.decisionTick.mu.Unlock()
	return s.decisionTick.runner
}

// SetDecisionTickChannel replaces the interval ticker with a caller-driven
// channel. Test affordance only.
func (s *Server) SetDecisionTickChannel(c <-chan time.Time) {
	s.decisionTick.mu.Lock()
	defer s.decisionTick.mu.Unlock()
	s.decisionTick.tick = c
}

// StartDecisionTick starts the evaluation loop. Idempotent, and a no-op with
// no runner attached.
func (s *Server) StartDecisionTick() {
	s.decisionTick.mu.Lock()
	if s.decisionTick.running || s.decisionTick.runner == nil {
		s.decisionTick.mu.Unlock()
		return
	}
	if s.decisionTick.interval == 0 {
		s.decisionTick.interval = defaultDecisionTickInterval
	}
	if s.decisionTick.limit == 0 {
		s.decisionTick.limit = defaultDecisionTickLimit
	}
	if s.decisionTick.lease == 0 {
		s.decisionTick.lease = defaultDecisionClaimLease
	}
	if s.decisionTick.runnerID == "" {
		s.decisionTick.runnerID = "decision-" + uuid.NewString()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.decisionTick.stop = make(chan struct{})
	s.decisionTick.cancel = cancel
	s.decisionTick.running = true
	interval := s.decisionTick.interval
	stop := s.decisionTick.stop
	tick := s.decisionTick.tick
	s.decisionTick.mu.Unlock()

	slog.Info("decision tick started", "interval", interval.String())

	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		defer s.recoverSweeper("decision-tick")
		var c <-chan time.Time
		if tick != nil {
			c = tick
		} else {
			t := time.NewTicker(interval)
			defer t.Stop()
			c = t.C
		}
		for {
			select {
			case <-stop:
				return
			case <-c:
				s.runDecisionTick(ctx)
			}
		}
	}()
}

// stopDecisionTick signals the loop to exit and cancels an in-flight provider
// call, so Stop() does not wait out a network round trip before closing the
// database. The runner releases a cancelled job's claim without counting an
// attempt, so it is claimable again at once.
func (s *Server) stopDecisionTick() {
	s.decisionTick.mu.Lock()
	defer s.decisionTick.mu.Unlock()
	if !s.decisionTick.running {
		return
	}
	close(s.decisionTick.stop)
	s.decisionTick.cancel()
	s.decisionTick.running = false
}

func (s *Server) runDecisionTick(ctx context.Context) {
	s.decisionTick.mu.Lock()
	r, id, limit, lease := s.decisionTick.runner, s.decisionTick.runnerID, s.decisionTick.limit, s.decisionTick.lease
	s.decisionTick.mu.Unlock()

	n, err := r.RunOnce(ctx, id, limit, lease)
	if err != nil {
		slog.Error("decision tick: claim failed", "error", err, "claimed", n)
	}
}
