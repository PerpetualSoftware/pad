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
// and no job row is ever written. The loop starts the first time a provider
// is configured — at boot, or later when an admin enables one
// (ConfigureDecisions, TASK-3121). It is not stopped when a later change
// disables decisions: it keeps ticking, reads a nil runner, and does nothing.

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

	// stopped latches once stopDecisionTick runs. Reconfiguration can start
	// the tick at any time (TASK-3121), so without it a settings write racing
	// Stop() could start a loop after Stop() had drained s.bg — a goroutine
	// using a store that is about to close (codex round 1).
	stopped bool

	// passCancel cancels the pass in flight, if any. SetDecisionRunner calls
	// it when the runner changes, so disabling or replacing the provider
	// stops the OLD one being called for the rest of that pass — up to
	// `limit` more jobs of item data sent to a provider the admin turned off
	// (codex round 1). A cancelled job's claim is released without counting
	// an attempt, so the new runner picks it up.
	passCancel context.CancelFunc
}

// SetDecisionRunner attaches the decision runner and wires its registry into
// the store's write doors. A nil runner (no provider configured) removes any
// resolver, so the doors enqueue nothing.
//
// The write doors' resolver is installed BEFORE the runner is published
// (codex round 2). Disabling therefore stops new enqueues before the runner
// goes nil, rather than leaving a window after the disable in which a write
// still owes a job. A write whose transaction read the old resolver before
// that point can still commit its job afterwards: it was concurrent with the
// disable, and its row stays owed, un-run, until a provider is configured.
func (s *Server) SetDecisionRunner(r *decision.Runner) {
	if s.store != nil {
		r.Install(s.store)
	}
	s.decisionTick.mu.Lock()
	if s.decisionTick.runner != r && s.decisionTick.passCancel != nil {
		s.decisionTick.passCancel()
		s.decisionTick.passCancel = nil
	}
	s.decisionTick.runner = r
	s.decisionTick.mu.Unlock()
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

// StartDecisionTick starts the evaluation loop. Idempotent, a no-op with no
// runner attached, and a no-op once Stop() has run.
func (s *Server) StartDecisionTick() {
	s.decisionTick.mu.Lock()
	if s.decisionTick.running || s.decisionTick.stopped || s.decisionTick.runner == nil {
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
	s.decisionTick.stopped = true
	if !s.decisionTick.running {
		return
	}
	close(s.decisionTick.stop)
	s.decisionTick.cancel()
	s.decisionTick.running = false
}

func (s *Server) runDecisionTick(ctx context.Context) {
	// The runner and the pass's cancel are taken under ONE lock, so a swap
	// either lands before (this pass uses the new runner) or after (the swap
	// cancels this pass). There is no window in which the old runner runs
	// uncancellable.
	passCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.decisionTick.mu.Lock()
	r, id, limit, lease := s.decisionTick.runner, s.decisionTick.runnerID, s.decisionTick.limit, s.decisionTick.lease
	s.decisionTick.passCancel = cancel
	s.decisionTick.mu.Unlock()
	defer func() {
		s.decisionTick.mu.Lock()
		s.decisionTick.passCancel = nil
		s.decisionTick.mu.Unlock()
	}()

	n, err := r.RunOnce(passCtx, id, limit, lease)
	if err != nil {
		slog.Error("decision tick: claim failed", "error", err, "claimed", n)
	}
}
