package server

import "time"

// midStreamGapCooldown is the minimum interval between two mid-stream
// sync_required signals on ONE connection.
//
// WHY A COOLDOWN AT ALL. The signal's remedy is a delta sync, and the
// subscriber most likely to be signalled is a slow one — so answering "you
// could not keep up" with "now also fetch a delta" can feed back: more sync
// work, slower draining, more drops, more signals. The bus's gap channel
// coalesces (capacity 1), which bounds the QUEUE but not the LOOP: once the
// handler consumes a signal, the next drop re-arms it immediately.
//
// WHY THIS LENGTH. It is a round trip, not a tuning knob pulled out of the
// air: a second signal is only useful once the client has had time to act on
// the first, and telling it again mid-delta is pure waste — the web client
// already defers a sync_required that arrives while one is in flight
// (BUG-2508). Five seconds is comfortably longer than a delta sync against a
// busy workspace and far shorter than the 30s keepalive, so a genuinely new
// hole is never sat on for a noticeable time.
//
// WHY A LATCH RATHER THAN A DROP. Suppressing a gap that arrives inside the
// window would be the same dishonesty this whole fix exists to remove, one
// layer up. A gap inside the window is REMEMBERED and announced when the
// window closes, so the client always learns — at most once per window.
const midStreamGapCooldown = 5 * time.Second

// gapCooldown is the interval a handler should use — midStreamGapCooldown
// unless a test has narrowed it.
//
// A field rather than the bare constant because the rate limit's BINDING to
// the handlers is a separate claim from the limiter working (team CONVE-19),
// and an integration test that had to wait out five real seconds per assertion
// would not be written.
func (s *Server) gapCooldown() time.Duration {
	if s.midStreamGapCooldownOverride > 0 {
		return s.midStreamGapCooldownOverride
	}
	return midStreamGapCooldown
}

// gapReadyToAnnounce is the SSE handlers' ordering barrier, as a predicate.
//
// latched says a gap is waiting; queued is the subscriber channel's current
// depth; budget is the depth captured when the gap was latched, decremented
// once per event taken off the channel since.
//
// The announcement waits for the events that were already queued when the gap
// arrived, so a client is not told its position is untrustworthy and then
// immediately handed IDs that re-establish one, below the hole. It waits for
// them BY COUNT rather than for the channel to empty: an emptiness-only
// condition never fires under continuous refill, and the subscriber this
// signal exists for — a slow one on a busy workspace — is exactly the one
// whose channel never empties.
//
// A function rather than an expression inlined in two select loops because the
// starvation case is not reliably reproducible end-to-end (the channel does
// briefly empty under most schedulings), so the bound needs somewhere it can
// be asserted directly.
func gapReadyToAnnounce(latched bool, queued, budget int) bool {
	return latched && (queued == 0 || budget <= 0)
}

// gapAnnouncer rate-limits one connection's mid-stream gap announcements
// without losing any.
//
// Used by both SSE handlers, which have separate select loops that would
// otherwise each grow their own copy of this logic — and a rate limiter
// implemented twice is a rate limiter that behaves two ways.
//
// Not safe for concurrent use: it belongs to one handler goroutine.
type gapAnnouncer struct {
	cooldown time.Duration
	timer    *time.Timer
	// cooling is the timer's channel while a window is open, and nil
	// otherwise. A nil channel blocks forever in a select, which is how the
	// case switches itself off without a second flag.
	cooling <-chan time.Time
	pending bool
}

func newGapAnnouncer(cooldown time.Duration) *gapAnnouncer {
	t := time.NewTimer(cooldown)
	if !t.Stop() {
		<-t.C
	}
	return &gapAnnouncer{cooldown: cooldown, timer: t}
}

// stop releases the timer. Safe to call more than once.
func (g *gapAnnouncer) stop() { g.timer.Stop() }

// cool is the channel a handler's select must watch alongside the bus's gap
// channel. It fires when a window closes; the handler then calls flush.
func (g *gapAnnouncer) cool() <-chan time.Time { return g.cooling }

// observe records a gap the bus just reported and says whether to announce it
// NOW. False means the announcement is latched and will come back through
// cool()/flush() when the window closes.
func (g *gapAnnouncer) observe() bool {
	if g.cooling != nil {
		g.pending = true
		return false
	}
	g.open()
	return true
}

// flush is called when cool() fires. It reports whether a latched gap is owed
// an announcement now, and reopens the window if so.
func (g *gapAnnouncer) flush() bool {
	g.cooling = nil
	if !g.pending {
		return false
	}
	g.pending = false
	g.open()
	return true
}

func (g *gapAnnouncer) open() {
	g.timer.Reset(g.cooldown)
	g.cooling = g.timer.C
}
