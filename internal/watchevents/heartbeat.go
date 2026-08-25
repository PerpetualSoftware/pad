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
func (b *RedisBus) stampLastSeen(gen int64) {
	b.mu.Lock()
	if b.subGen == gen {
		b.lastSeen = b.now()
	}
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

	b.mu.Lock()
	gen := b.subGen
	b.mu.Unlock()

	channel := b.keys.Name(redisWatchChannelSuffix)
	if err := b.client.Publish(b.ctx, channel, watchHeartbeatPayload).Err(); err != nil {
		// Logged and dropped without a retry OF OURS — go-redis retries the
		// command internally before returning this error, so what reaches here
		// has already been tried more than once — and DELIBERATELY NOT
		// STAMPED:
		// lastProbeOK stays where it was, so the detector stops treating this
		// instance's silence as evidence. A failure to PROBE is not a finding
		// about the peer.
		slog.Warn("watchevents: failed to publish a liveness heartbeat; idle detection is suspended until a probe "+
			"succeeds, because silence cannot be read as a finding when we could not ask",
			"channel", channel, "error", err)
		b.reportHeartbeatPublishFailed()
		return
	}

	if b.afterProbePublish != nil {
		b.afterProbePublish()
	}

	// STAMPED ONLY IF IT IS STILL THE SAME SUBSCRIPTION. The publish happens off
	// the lock and can take as long as go-redis's timeouts allow, during which a
	// cycle may have replaced the subscription this probe was sent for.
	// Crediting the replacement would say a probe reached a socket that never
	// saw one.
	b.mu.Lock()
	if b.subGen == gen {
		b.lastProbeOK = b.now()
	}
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
	// NO SUBSCRIPTION AT ALL: a previous pass dropped coverage and its
	// resubscribe failed. THE RE-DIAL IS ALL THAT IS OWED HERE (codex round
	// 5). Falling through would re-run the whole cycle, and the cycle's
	// precondition stays true for the entire outage — the probe fails so
	// nothing stamps lastProbeOK, nothing arrives so nothing stamps lastSeen,
	// and both are frozen either side of the threshold. Coverage would be
	// dropped once per cadence, each time on an already-empty buffer, each
	// time re-announcing a hole every subscriber has been told about, and each
	// time moving the counter an operator is told to read as an incident
	// count. Retrying is genuinely owed; re-deciding is not.
	if b.pubsub == nil && !b.closed && b.client != nil {
		b.mu.Unlock()
		if err := b.resubscribe(); err != nil {
			// "this attempt failed", not "this instance is deaf": with a
			// second caller the dial can fail here while that caller has
			// already installed one, so the stronger claim would be stale.
			slog.Warn("watchevents: a retry to re-establish the watch subscription failed; this instance receives "+
				"no notifications unless another attempt has already succeeded", "error", err)
		}
		return
	}
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
	decidedGen := b.subGen
	b.mu.Unlock()

	slog.Warn("watchevents: no traffic on this instance's Redis subscription within the idle timeout; ending its "+
		"replay coverage and attempting to replace the connection, resumes across the silence will report sync_required",
		"idle_timeout", idleTimeout)

	if b.beforeDropHook != nil {
		b.beforeDropHook()
	}

	// RE-VALIDATED IMMEDIATELY BEFORE THE DROP (codex round 1). The decision
	// above was taken under a lock this function then released, and a heartbeat
	// or notification can arrive in that window — at which point the
	// subscription is demonstrably alive and dropping it would resync every
	// client on the instance for nothing. False positives are the property this
	// design cares about most, and BUG-2738 had to learn the same thing at its
	// round 11: "idle when we looked" is not "idle now".
	// RE-VALIDATED AND DROPPED UNDER ONE LOCK (codex round 2). Checking and
	// then dropping in two acquisitions leaves an interval in which a frame can
	// arrive and be discarded by a drop already decided on — which is the same
	// class of defect as the stale decision this re-check exists to fix, one
	// level down.
	if !b.dropCoverageIfStillIdle(decidedGen) {
		slog.Info("watchevents: the subscription started receiving again before its cycle ran; leaving it alone")
		return
	}

	// The generation was retired inside dropCoverageIfStillIdle, atomically with
	// the buffer reset. From here until resubscribe installs a new one, NO
	// generation is current, so a late frame from the old subscription is
	// ignored at all four places it could otherwise mutate shared state: the
	// epoch bookkeeping and the buffer append (redis_bus.go), the coverage drop
	// (dropCoverageChecked), and the liveness stamp (stampLastSeen).

	// The replaced loop must leave by the quiet door BEFORE its subscription is
	// closed, or it reports the instance deaf on a closure we caused.
	if oldCancel != nil {
		oldCancel()
	}
	if oldPubsub != nil {
		_ = oldPubsub.Close()
	}

	// FORGOTTEN AS WELL AS CLOSED. resubscribe overwrites both on success, so
	// this only matters when it fails — and then the fields would otherwise go
	// on naming a PubSub that is already closed, which Close closes a second
	// time and which makes "this instance has no subscription" unrepresentable.
	// The retry arm at the top of this function reads exactly that state.
	b.mu.Lock()
	b.pubsub, b.subCancel = nil, nil
	b.mu.Unlock()

	if err := b.resubscribe(); err != nil {
		// NOT "until restarted" — that was wrong (codex round 2). No generation
		// is current after the teardown above, so the next idle tick finds the
		// instance idle and tries again. It is degraded until one succeeds, and
		// every attempt is reported, which is the honest claim.
		slog.Error("watchevents: could not re-establish the watch subscription after an idle cycle; this instance "+
			"receives no notifications until a later attempt succeeds", "error", err)
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

	// A TEST SEAM AT THE DIAL/INSTALL BOUNDARY. The window the install guard
	// below closes opens here: the connection exists and the lock has not been
	// taken. A test cannot open it from outside — starting two goroutines only
	// makes overlap likely, and one can finish before the other begins (codex
	// round 8) — so this is where a barrier has to go for the guard to be
	// exercised rather than assumed. nil in production.
	if b.beforeInstallLock != nil {
		b.beforeInstallLock()
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		subCancel()
		_ = pubsub.Close()
		return errors.New("bus closed during resubscribe")
	}
	// SOMEONE ELSE GOT THERE FIRST (codex round 6). Both callers dial with the
	// lock released — deliberately, since a Redis round trip under the bus's
	// hot mutex would stall every fan-out on the instance — so two passes can
	// each find no subscription and each dial one. Installing both would leave
	// two receive loops on the SAME generation, so both accept frames and every
	// notification is processed twice, and the loser's PubSub would be
	// untracked: nothing closes it, including Close.
	//
	// The install is what needs serialising, not the dial, so the loser
	// discards its own connection here rather than the two racing to overwrite
	// b.pubsub. In production one goroutine runs this — the idle scanner — and
	// that is exactly the sort of invariant a later caller breaks without
	// noticing, which is why this does not rely on it.
	if b.pubsub != nil {
		b.mu.Unlock()
		subCancel()
		_ = pubsub.Close()
		return nil
	}
	b.pubsub = pubsub
	b.subCancel = subCancel
	// Already retired by the caller when it tore the old one down; on the
	// constructor path this is the first generation. Either way the loop below
	// reads the CURRENT value rather than minting another.
	gen := b.subGen
	// STAMPED AT INSTALL, both of them. A zero lastSeen reads as 1970 and the
	// detector would cycle the replacement on its next pass; and lastProbeOK
	// equal to lastSeen is what stops it being cycled before its first
	// successful probe.
	b.lastSeen = b.now()
	b.lastProbeOK = b.now()
	// ADDED UNDER THE SAME LOCK AS THE b.closed CHECK ABOVE, deliberately.
	// Close sets closed, then Waits; a WaitGroup forbids an Add that takes the
	// counter off zero from racing a Wait, and between an Unlock here and an
	// Add outside it, Close can run in full. Holding the lock across the Add
	// makes "not closed" and "counted" one step, so Close either sees the
	// count or has already sent this call down the closed-bus return above.
	b.wg.Add(1)
	b.mu.Unlock()

	// A TEST SEAM AT THE ONLY POINT THAT MATTERS. The hazard this ordering
	// guards is Close running between the b.closed check above and the Add,
	// and it is unreachable from outside: both sit under one lock, so nothing
	// a test can call from another goroutine lands between them. Firing here,
	// with the lock released and the Add already made, lets a test hold the
	// window open and observe that Close survives it. nil in production.
	if b.afterResubscribeInstall != nil {
		b.afterResubscribeInstall()
	}

	go b.receiveMessages(subCtx, pubsub, gen)
	return nil
}

// NOTE: callerIsGone already exists in this package, added by BUG-2751 for the
// resume settle path, and is reused here rather than duplicated. Its reasoning
// carries unchanged: classify on the ERROR, never on the caller's context
// state, because asking the context cannot distinguish "we stopped" from "Redis
// failed while we happened to be stopping".

// currentGen reports the live subscription's generation. Test helper in spirit
// but unexported and used by production code's own checks; a direct caller that
// is standing in for the receive loop passes this.
func (b *RedisBus) currentGen() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.subGen
}

// dropCoverageIfStillIdle re-validates and ends coverage without releasing the
// lock in between, returning the reason to report once it is released.
//
// The report has to happen off the lock — observer callbacks may call back into
// the bus — but the DECISION and the DROP must not be separable, or a frame
// arriving between them is silently discarded by a drop that was already
// decided.
func (b *RedisBus) dropCoverageIfStillIdle(decidedGen int64) bool {
	var pending pendingReports
	b.mu.Lock()
	stillIdle := b.subGen == decidedGen &&
		b.now().Sub(b.lastSeen) >= b.idleTimeout &&
		b.lastProbeOK.After(b.lastSeen)
	if !stillIdle {
		b.mu.Unlock()
		return false
	}
	// RETIRED IN THE SAME CRITICAL SECTION AS THE RESET (codex round 3).
	// Retiring it afterwards left a window between the fresh buffer and the
	// generation bump in which a straggler from the old subscription still
	// passed every fence and landed in the buffer we had just emptied — which
	// is precisely the stale coverage this cycle exists to end.
	b.subGen++
	b.replay = newReplayBuffer(b.replaySize)
	b.lastAppendedID = 0
	b.knownFrom = 0
	pending.reset(ResetReasonIdleTimeout)
	b.signalAllLocked()
	b.mu.Unlock()

	b.flush(&pending)
	return true
}
