package watchevents

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// This is BUG-2738's mechanism, ported to the watch bus — and the port is
// SMALLER than the original by design rather than by omission.
//
// The defect is identical and inherited from the same library: a Redis
// connection can stop carrying traffic without closing, and go-redis cannot see
// it, because PubSub.Ping writes the command and never reads a reply (v9.22.0,
// pubsub.go). The instance then blocks on a read forever while its replay
// buffer goes on looking complete.
//
// THIS PACKAGE ALREADY HAS AN ARM FOR "RECEIVING NOTHING", AND IT CANNOT SEE
// THIS. receiveMessages handles the message channel CLOSING, which go-redis
// does only on pool.ErrClosed — the client or PubSub was closed underneath a
// running bus. That case logs an ERROR and moves a counter, for exactly the
// state "publishes fine, receives nothing at all". A half-open route produces
// the same observable state and never trips it, because nothing closes. A
// reader who finds that arm first will reasonably assume this is covered; it is
// not, and this file is the part that covers it.
//
// WHAT DOES NOT PORT, and why each is meaningless here rather than skipped:
//
//   - The per-workspace subscription map. This bus holds ONE process-wide
//     subscription on one channel, so liveness is a property of the instance
//     and lives in two fields rather than a map.
//   - The establishment record, the single-establisher wall, and joint rules 1
//     and 3. Those exist because internal/events establishes on the REQUEST
//     path with concurrent first subscribers; this bus subscribes once, in its
//     constructor, off any request.
//   - The concurrency cap and bounded-parallel recovery. Those bound N
//     simultaneous re-dials; here N is one.
//   - Per-workspace cycle scoping. dropCoverage already announces to every
//     subscriber on the instance, because there is one buffer and one flat
//     subscriber map.
//   - The dedicated cycle counter. internal/events needs one because its
//     dropWorkspaceCoverage RETURNS EARLY when a workspace has no buffer, so
//     the reset reason under-reports the early-wedge case. dropCoverage here
//     has no such branch: it replaces the buffer and reports unconditionally,
//     so the reason is complete on its own and a second metric would be a
//     number needing to be explained against its neighbour for no signal.
//
// WHAT DOES PORT, each having earned its place there:
//
//   - Stamp at install and on EVERY inbound frame, not only decodable ones.
//   - Validate the frame by SHAPE, not by prefix alone, so a foreign payload
//     wearing "hb|" still ends coverage loudly instead of being swallowed.
//   - Suspend detection when the probe itself fails, classified on the ERROR
//     rather than on the caller's context state.
//   - A deadline-based cadence, so a slow instance cannot manufacture its own
//     incident by drifting its own heartbeats apart.
//   - "Replacement attempted, not guaranteed" in whatever the operator reads.
const (
	// DefaultWatchHeartbeatInterval is T. Dave's day-49 ruling for the
	// activity bus, applied here for the same reason: it makes silence
	// diagnostic without asking anyone to guess a deployment's publish rate.
	DefaultWatchHeartbeatInterval = 30 * time.Second

	// DefaultWatchIdleTimeout is 3T — three intervals so a single lost or late
	// frame is not a cycle.
	DefaultWatchIdleTimeout = 3 * DefaultWatchHeartbeatInterval
)

// watchHeartbeatPrefix marks a bus-internal liveness frame on the watch
// channel. It travels on the SAME channel as notifications because that is the
// connection whose liveness is in question; a probe anywhere else proves the
// wrong thing. That is also why it is a wire-format change and rolls out in two
// phases — see config.WatchHeartbeat.
const watchHeartbeatPrefix = "hb|"

// watchHeartbeatPayload is what this version emits; the suffix is a format
// version, not a timestamp.
const watchHeartbeatPayload = watchHeartbeatPrefix + "1"

// watchHeartbeatMaxLen bounds what this package will accept as its own frame.
const watchHeartbeatMaxLen = 64

// isWatchHeartbeat reports whether a payload is a liveness frame.
//
// VALIDATED BY SHAPE, NOT BY PREFIX ALONE. Accepting any "hb|…" would create a
// silently-ignored class on the watch channel, where every unreadable payload
// otherwise ends coverage loudly and moves undecodable_message — the counter
// whose documented job is "suspect a namespace collision". A foreign publisher
// whose bytes happen to start with the prefix must not slip through it.
//
// What is NOT a problem, and was considered: a forged frame cannot fake
// liveness. Liveness means "this socket carried traffic", and a frame that
// ARRIVES demonstrates that whoever sent it.
func isWatchHeartbeat(payload string) bool {
	if len(payload) > watchHeartbeatMaxLen || !strings.HasPrefix(payload, watchHeartbeatPrefix) {
		return false
	}
	fields := strings.Split(payload[len(watchHeartbeatPrefix):], "|")
	if fields[0] == "" {
		return false
	}
	for _, r := range fields[0] {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, f := range fields[1:] {
		for _, r := range f {
			switch {
			case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			case r == '.' || r == '_' || r == '-' || r == ':':
			default:
				return false
			}
		}
	}
	return true
}

// now reads the bus's clock. The seam is nil in production; tests set it so a
// 90s threshold can be crossed without sleeping through one.
func (b *RedisBus) now() time.Time {
	if b.nowFunc != nil {
		return b.nowFunc()
	}
	return time.Now()
}

// stampLastSeen records that the subscription just received something.
//
// Called for EVERY inbound frame, ahead of any decode: what is measured is
// whether the SOCKET carries traffic, so a frame that turns out to be
// undecodable is still proof the route works. Ending coverage is the right
// answer to an unreadable message; cycling the connection is not.
func (b *RedisBus) stampLastSeen() {
	b.mu.Lock()
	b.lastSeen = b.now()
	b.mu.Unlock()
}

// maintenanceLoop publishes heartbeats and cycles a wedged subscription.
//
// TWO GOROUTINES, NOT ONE. publishHeartbeats makes a synchronous Redis publish,
// and against the very failure this exists to detect that call is the one that
// blocks — bounded by go-redis's own timeouts rather than by anything we pass.
// Sharing a goroutine would let a stalled publisher delay detection on exactly
// the instance whose connection has wedged.
func (b *RedisBus) maintenanceLoop() {
	defer b.wg.Done()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); b.tickForever(b.heartbeatKick, b.publishHeartbeats) }()
	go func() { defer wg.Done(); b.tickForever(b.idleKick, b.cycleIfIdle) }()
	wg.Wait()
}

// tickForever runs work on the configured interval until the bus closes.
//
// SCHEDULED FROM A DEADLINE, not from the end of the last pass: restarting the
// timer afterwards makes the real period T plus however long the pass took, and
// for the publisher that is self-defeating — an instance whose publishes are
// slow emits heartbeats further apart, sees them further apart, and can cross
// its own 3T threshold and cycle a connection that was never wedged.
func (b *RedisBus) tickForever(kick <-chan struct{}, work func()) {
	next := time.Now()
	for {
		b.mu.Lock()
		interval := b.heartbeatInterval
		b.mu.Unlock()

		next = nextWatchTick(next, interval, time.Now())
		if wait := time.Until(next); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-b.ctx.Done():
				timer.Stop()
				return
			case <-kick:
				timer.Stop()
				next = time.Now()
				continue
			case <-timer.C:
			}
		}
		if b.ctx.Err() != nil {
			return
		}
		work()
	}
}

// nextWatchTick returns the deadline for the pass after one scheduled for prev.
// Separated out so the arithmetic is testable without a clock — the only way to
// observe drift through the loop is to time it, and a timing test is a flaky
// test. An overrun beyond one interval resets rather than replaying the missed
// ticks: a burst of heartbeats buys nothing and a burst of scans would hammer a
// Redis that is already struggling.
func nextWatchTick(prev time.Time, interval time.Duration, now time.Time) time.Time {
	next := prev.Add(interval)
	if now.Sub(next) > interval {
		return now.Add(interval)
	}
	return next
}

// setMaintenanceCadence changes T and the idle threshold on a running bus, so
// the WIRING can be tested at the cadence rather than only the halves at the
// call (CONVE-19).
func (b *RedisBus) setMaintenanceCadence(interval, idleTimeout time.Duration) {
	b.mu.Lock()
	b.heartbeatInterval = interval
	b.idleTimeout = idleTimeout
	b.mu.Unlock()
	for _, kick := range []chan struct{}{b.heartbeatKick, b.idleKick} {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
}

// publishHeartbeats emits one liveness frame. No-op until phase 2.
//
// IT DOES NOT GO THROUGH Publish. That path assigns an id from the shared watch
// counter and carries epoch semantics; a heartbeat that consumed an id would
// inflate the sequence that counter_backward and epoch_change reason about, so
// a liveness probe would manufacture the resets it exists to avoid. A heartbeat
// carries no id, is never buffered, never replayed, never reaches a subscriber
// and is never counted as a notification.
//
// COST: one publish per instance per interval, regardless of how many
// workspaces or clients exist — there is one channel. The activity bus pays N
// per interval for N workspaces; this is the structural saving that makes the
// port cheap.
func (b *RedisBus) publishHeartbeats() {
	b.mu.Lock()
	on := b.publishHeartbeat
	b.mu.Unlock()
	if !on || b.client == nil {
		return
	}

	channel := b.keys.Name(redisWatchChannelSuffix)
	if err := b.client.Publish(b.ctx, channel, watchHeartbeatPayload).Err(); err != nil {
		// Logged and dropped, never retried, and DELIBERATELY NOT STAMPED:
		// lastProbeOK stays where it was, so the detector stops treating this
		// instance's silence as evidence. A failure to PROBE is not a finding
		// about the peer.
		slog.Warn("watchevents: failed to publish a liveness heartbeat; idle detection is suspended until a probe "+
			"succeeds, because silence cannot be read as a finding when we could not ask",
			"channel", channel, "error", err)
		b.reportHeartbeatPublishFailed()
		return
	}

	b.mu.Lock()
	b.lastProbeOK = b.now()
	b.mu.Unlock()
}

// cycleIfIdle ends this instance's coverage and replaces its subscription when
// nothing has arrived for the idle timeout.
//
// DROP-AND-CYCLE, NOT DROP ALONE. A half-open route stays half-open: dropping
// coverage makes the next resume honest, but the resync it demands is served
// from the same dead socket and the detector fires again on the next pass — a
// loop metering the failure rather than recovering from it.
func (b *RedisBus) cycleIfIdle() {
	b.mu.Lock()
	switch {
	case !b.publishHeartbeat, b.closed, b.client == nil:
		b.mu.Unlock()
		return
	case b.now().Sub(b.lastSeen) < b.idleTimeout:
		b.mu.Unlock()
		return
	case !b.lastProbeOK.After(b.lastSeen):
		// THE PREMISE HAS TO HOLD BEFORE THE CONCLUSION IS DRAWN. Silence means
		// "the receive path is dead" only if we managed to send something into
		// it since the last thing came out. Expressed as an ordering rather
		// than an age: a fresh subscription has the two equal, so it is never
		// cycled before its first successful probe.
		b.mu.Unlock()
		return
	}
	oldCancel, oldPubsub := b.subCancel, b.pubsub
	idleTimeout := b.idleTimeout
	b.mu.Unlock()

	slog.Warn("watchevents: no traffic on this instance's Redis subscription within the idle timeout; ending its "+
		"replay coverage and attempting to replace the connection, resumes across the silence will report sync_required",
		"idle_timeout", idleTimeout)

	// Coverage first, and through the ordinary path: dropCoverage resets the
	// buffer, reports the reason and announces to every live subscriber, which
	// is what this bus already does for a resubscription.
	b.dropCoverage(ResetReasonIdleTimeout)

	// The replaced loop must leave by the quiet door BEFORE its subscription is
	// closed, or it reports the instance deaf on a closure we caused.
	if oldCancel != nil {
		oldCancel()
	}
	if oldPubsub != nil {
		_ = oldPubsub.Close()
	}

	if err := b.resubscribe(); err != nil {
		slog.Error("watchevents: could not re-establish the watch subscription after an idle cycle; this instance "+
			"will receive no further notifications until it is restarted", "error", err)
		return
	}
}

// resubscribe installs a fresh subscription and receive loop.
func (b *RedisBus) resubscribe() error {
	pubsub := b.client.Subscribe(b.ctx, b.keys.Name(redisWatchChannelSuffix))

	confirmCtx, cancelConfirm := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancelConfirm()
	if _, err := pubsub.Receive(confirmCtx); err != nil {
		_ = pubsub.Close()
		return err
	}

	subCtx, subCancel := context.WithCancel(b.ctx)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		subCancel()
		_ = pubsub.Close()
		return errors.New("bus closed during resubscribe")
	}
	b.pubsub = pubsub
	b.subCancel = subCancel
	// STAMPED AT INSTALL, both of them. A zero lastSeen reads as 1970 and the
	// detector would cycle the replacement on its next pass; and lastProbeOK
	// equal to lastSeen is what stops it being cycled before its first
	// successful probe.
	b.lastSeen = b.now()
	b.lastProbeOK = b.now()
	b.mu.Unlock()

	b.wg.Add(1)
	go b.receiveMessages(subCtx, pubsub)
	return nil
}

// NOTE: callerIsGone already exists in this package, added by BUG-2751 for the
// resume settle path, and is reused here rather than duplicated. Its reasoning
// carries unchanged: classify on the ERROR, never on the caller's context
// state, because asking the context cannot distinguish "we stopped" from "Redis
// failed while we happened to be stopping".
