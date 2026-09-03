package server

import (
	"log/slog"
	"sync"
	"time"
)

// The reminder tick: the half of IDEA-2641 that ACTS at a target time.
//
// Everything else in Pad's date handling is reactive — a due_date makes an
// item show up as overdue once somebody asks. Nothing fired on its own, which
// is why GitHub #1010 had to keep "revisit this on the 1st" in an external
// cron. This loop is the engine; the store owns the arbitration and the
// event, and this file owns only the schedule.
//
// It is the sixth instance of a settled shape (outbox drain, token reaper,
// workspace purge, oplog GC, orphan GC, MCP audit sweep): config struct with
// its own mutex and stop channel, tracked by Server.bg so Stop() drains it
// before the DB closes (the BUG-842 invariant), recoverSweeper on the
// goroutine, and an injectable tick channel so a test can pin assertions to
// one specific pass instead of racing a free-running loop.

// defaultReminderTickInterval is how often armed reminders are checked.
//
// THE RECEIPT, because a bare number invites someone to "tune" it: this bounds
// LATENESS, not throughput. A reminder fires at most one interval after its
// instant, so the interval is the promise — 30s means "within half a minute of
// when you asked", which is the resolution a human-set reminder is stated at
// in the first place (nobody arms one for 14:32:07). The cost side is a single
// indexed range scan over a PARTIAL index holding only armed rows, which is
// empty on the overwhelming majority of instances; a tick that finds nothing
// does one query and returns. Going faster buys precision nobody asked for at
// a cost that scales with instance count; going much slower makes "remind me
// at 9" mean something a user would call broken.
const defaultReminderTickInterval = 30 * time.Second

type reminderTickConfig struct {
	mu       sync.Mutex
	interval time.Duration
	limit    int
	stop     chan struct{}
	running  bool
	// tick, when non-nil, replaces the interval ticker so a test can drive
	// exactly one pass. Same affordance as outboxDrainConfig.tick.
	tick <-chan time.Time
}

// SetReminderTickConfig overrides the tick's timings. Zero values keep the
// defaults, so a caller can set one knob without restating the rest. Must be
// called before StartReminderTick; the goroutine captures the interval at
// start.
func (s *Server) SetReminderTickConfig(interval time.Duration, limit int) {
	s.reminderTick.mu.Lock()
	defer s.reminderTick.mu.Unlock()
	if interval > 0 {
		s.reminderTick.interval = interval
	}
	if limit > 0 {
		s.reminderTick.limit = limit
	}
}

// SetReminderTickChannel replaces the interval ticker with a caller-driven
// channel. Test affordance only.
func (s *Server) SetReminderTickChannel(c <-chan time.Time) {
	s.reminderTick.mu.Lock()
	defer s.reminderTick.mu.Unlock()
	s.reminderTick.tick = c
}

// StartReminderTick starts the periodic reminder sweep. Idempotent.
//
// Started from the real server bootstrap path, not Server.New, so unit tests
// that construct a Server don't spawn a background goroutine unless they opt
// in — the same rule every sweeper here follows.
func (s *Server) StartReminderTick() {
	s.reminderTick.mu.Lock()
	if s.reminderTick.running {
		s.reminderTick.mu.Unlock()
		return
	}
	if s.reminderTick.interval == 0 {
		s.reminderTick.interval = defaultReminderTickInterval
	}
	s.reminderTick.stop = make(chan struct{})
	s.reminderTick.running = true
	interval := s.reminderTick.interval
	stop := s.reminderTick.stop
	tick := s.reminderTick.tick
	s.reminderTick.mu.Unlock()

	slog.Info("reminder tick started", "interval", interval.String())

	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		defer s.recoverSweeper("reminder-tick")
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
				s.runReminderTick()
			}
		}
	}()
}

// stopReminderTick signals the loop to exit. Safe when it never started.
func (s *Server) stopReminderTick() {
	s.reminderTick.mu.Lock()
	defer s.reminderTick.mu.Unlock()
	if !s.reminderTick.running {
		return
	}
	close(s.reminderTick.stop)
	s.reminderTick.running = false
}

// runReminderTick is one pass: fire every reminder whose instant has arrived.
//
// The store does the arbitration and writes each event in the same transaction
// as the fired_at that retires the reminder, so this function deliberately has
// no delivery logic of its own — the outbox drain picks the events up on its
// own schedule, and on an instance with no webhook dispatcher the poll surface
// serves them instead.
//
// NOW IS TAKEN ONCE per pass and passed down, rather than each row reading the
// clock: a pass that computed "now" per row could fire a reminder whose
// instant fell between two rows of the same scan, making the batch's contents
// depend on how long the batch took.
func (s *Server) runReminderTick() {
	if s.store == nil {
		return
	}
	s.reminderTick.mu.Lock()
	limit := s.reminderTick.limit
	s.reminderTick.mu.Unlock()

	nowTS := time.Now().UTC().Format(time.RFC3339)
	// A pass can BOTH fire and fail: the store continues past a broken row
	// rather than letting it block newer reminders, so it returns the
	// reminders it fired alongside the joined errors. Both halves are
	// reported — logging only the error would hide work that happened, and
	// logging only the count would hide work that did not.
	fired, err := s.store.FireDueReminders(nowTS, limit)
	if err != nil {
		// LOUD ON FAILURE, and never silent: a tick that fails quietly looks
		// exactly like a tick that found nothing, which is the shape that let
		// a broken watcher sit for thirty minutes reading as "still running".
		slog.Error("reminder tick: some reminders could not be fired", "error", err, "fired", len(fired))
	}
	if len(fired) > 0 {
		slog.Info("reminder tick: fired reminders", "count", len(fired))
	}
}
