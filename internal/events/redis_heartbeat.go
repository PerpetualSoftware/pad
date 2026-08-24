package events

import (
	"log/slog"
	"strings"
	"sync"
	"time"
)

// The heartbeat exists because NOTHING ELSE IN THIS PACKAGE CAN SEE A HALF-OPEN
// CONNECTION (BUG-2738). A route that stops working without closing — a NAT
// table expiring, a firewall dropping an idle flow, a silently rerouted path —
// leaves this instance blocked on a read that will never return, receiving
// nothing, while its replay buffer goes on looking complete. Every resume is
// then answered "caught up" from a coverage window that ended when the route
// did.
//
// go-redis's pub/sub health check does NOT cover it, and that was measured
// rather than reasoned. PubSub.Ping calls writeCmd and returns without ever
// reading a reply (v9.22.0, pubsub.go), so its error stays nil for as long as
// the socket accepts writes — which a half-open socket does until its send
// buffer fills. The channel path sets no read deadline either (Receive calls
// ReceiveTimeout(ctx, 0)). Probed against a TCP proxy that silently stopped
// forwarding: no reconnect in 24 seconds. Do not replace this with a Ping.
//
// WHAT MAKES THE THRESHOLD ANSWERABLE. "Is this workspace quiet, or is the
// route dead?" cannot be answered from traffic, because it depends on the
// deployment's publish rate and no constant is right for every one of them.
// Publishing our OWN traffic replaces it with "did our heartbeat arrive?",
// which is app-controlled and the same on every deployment. That is the whole
// reason the interval is not a tuned number: it is not measuring a workspace,
// it is measuring a socket.
const (
	// DefaultHeartbeatInterval is T: how often an instance publishes one
	// liveness frame per workspace it is subscribed to. Dave's ruling
	// (day-49): 30s.
	DefaultHeartbeatInterval = 30 * time.Second

	// DefaultIdleTimeout is 3T: how long a subscription may receive NOTHING —
	// no message, no heartbeat, no subscription confirmation — before its
	// coverage ends and its connection is cycled.
	//
	// Three intervals rather than two so that a single lost or late heartbeat
	// is not a cycle. The detector runs on the same ticker as the publisher,
	// so the observed latency to detection is in [3T, 4T): the scan granularity
	// adds up to one interval on top of the threshold itself.
	DefaultIdleTimeout = 3 * DefaultHeartbeatInterval
)

// heartbeatPrefix marks a BUS-INTERNAL liveness frame on a workspace's event
// channel.
//
// IT TRAVELS ON THE EVENT CHANNEL ON PURPOSE, and that is the entire cost of
// this design: what needs proving is that THIS channel's connection still
// carries traffic, so a probe on any other channel proves the wrong thing.
// That is also why this is a wire-format change and why it rolls out in two
// phases — see config.EventsHeartbeat.
//
// A PREFIX RATHER THAN AN EXACT PAYLOAD, so a later version of the frame can
// carry fields without needing a third roll: anything after the prefix is
// reserved and ignored. A phase-1 binary already ignores it whatever it says.
//
// It cannot be confused with either event form. decodePayload classifies on
// this prefix BEFORE it splits or unmarshals anything, and no epoch generation
// begins with "hb" — the prefixed event form's first field is parsed as an
// integer, and the bare form is JSON.
const heartbeatPrefix = "hb|"

// heartbeatPayload is what this version emits. The suffix is a format version,
// not a timestamp: a receiver derives arrival time from its own clock, because
// a publisher's clock is not comparable to it (the same reason
// redisEpochGenSuffix is a generation and not a wall clock).
const heartbeatPayload = heartbeatPrefix + "1"

// isHeartbeat reports whether a payload is a bus-internal liveness frame.
func isHeartbeat(payload string) bool { return strings.HasPrefix(payload, heartbeatPrefix) }

// now reads the bus's clock. The seam is nil in every real construction; tests
// set it so an idle threshold can be crossed without sleeping through one.
func (b *RedisBus) now() time.Time {
	if b.nowFunc != nil {
		return b.nowFunc()
	}
	return time.Now()
}

// maintenanceLoop runs the two halves of BUG-2738's machinery.
//
// TWO GOROUTINES, NOT ONE LOOP DOING BOTH, and the separation is the whole
// point rather than tidiness (codex round 1, P1). publishHeartbeats makes N
// SYNCHRONOUS Redis publishes, one per subscribed workspace. Against the
// failure this feature exists to detect — a route that has stopped carrying
// traffic — those publishes are exactly the ones that block, and go-redis
// bounds them by its own Dial/Read/WriteTimeout rather than by any context we
// could pass. Sharing a goroutine would therefore let a stalled publisher
// delay idle detection for as long as those timeouts take, on the very
// instance whose connections have wedged: the detector would sleep through the
// incident it was built to find, and the more workspaces an instance carried
// the longer it would sleep.
//
// A stalled publisher is not otherwise a problem — it produces silence, which
// is precisely what the detector reads. It only had to stop being the
// detector's problem too.
//
// Started by the constructor and ended by Close through b.ctx. Both halves are
// separately callable and the tests drive them directly, which is why the
// wiring has its own test: a direct-call test vouches for the function, not
// for its binding (team CONVE-19).
// A TIMER RE-READ EACH PASS, NOT A TICKER CONSTRUCTED ONCE. A ticker would
// capture heartbeatInterval at goroutine start, which makes the field
// write-once-at-construction in practice while looking like an ordinary
// tunable — and makes any later write to it a data race against this
// goroutine. Re-reading under b.mu each pass costs one uncontended lock per
// interval and makes the field genuinely what its comment says it is.
func (b *RedisBus) maintenanceLoop() {
	defer close(b.maintenanceStopped)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); b.tickForever(b.heartbeatKick, b.publishHeartbeats) }()
	go func() { defer wg.Done(); b.tickForever(b.idleKick, b.cycleIdleSubscriptions) }()
	wg.Wait()
}

// tickForever runs work on the configured interval until the bus closes.
//
// Each loop gets its OWN kick channel: a single shared one would be consumed
// by whichever goroutine happened to be waiting, leaving the other serving out
// a cadence that had already changed.
func (b *RedisBus) tickForever(kick <-chan struct{}, work func()) {
	for {
		b.mu.Lock()
		interval := b.heartbeatInterval
		b.mu.Unlock()

		timer := time.NewTimer(interval)
		select {
		case <-b.ctx.Done():
			timer.Stop()
			return
		case <-kick:
			// The cadence changed under us; drop this wait and re-read it
			// rather than serving out an interval that is no longer the
			// configured one.
			timer.Stop()
			continue
		case <-timer.C:
		}

		work()
	}
}

// setMaintenanceCadence changes T and the idle threshold on a running bus.
//
// IT EXISTS SO THE WIRING CAN BE TESTED AT THE CADENCE, not only the halves at
// the call (team CONVE-19: a direct-call test vouches for the component, not
// its binding). Without the kick, a test that shortens the interval races the
// loop's first read — lose that race and the test waits a full default
// interval — so the only deterministic alternative would be a test-only
// constructor, which would vouch for a construction production never uses.
//
// The kick is buffered and non-blocking: a change that arrives while the loop
// is mid-pass is picked up on its next read, which is the same interval either
// way.
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

// publishHeartbeats emits one liveness frame per workspace this instance is
// currently subscribed to. No-op until phase 2 (see config.EventsHeartbeat).
//
// IT DOES NOT GO THROUGH Publish, and that is load-bearing rather than
// stylistic. Publish mints an ID from the shared Redis counter; a heartbeat
// that consumed one would inflate the ID space that three of this bus's reset
// reasons are derived from — counter_backward, epoch_change and epoch_regressed
// all reason about that counter's values — so a liveness probe would start
// manufacturing the very resets it exists to avoid. A heartbeat carries no ID,
// no epoch, and is never buffered, replayed, fanned out, or counted as an
// event.
//
// COST, WITH THE SENTENCE THAT STOPS SOMEONE OPTIMISING THE WRONG LAYER: the
// heartbeat inherits per-workspace granularity from the existing
// one-PubSub-per-workspace structure — establishSubscription mints a separate
// connection per workspace, so liveness is genuinely per-workspace and there is
// no cheaper shared probe. An instance subscribed to N workspaces publishes N
// frames every T; at N=1000 and T=30s that is ~33 publishes/sec. If fleet
// workspace counts ever make this matter, the fix is CONNECTION CONSOLIDATION,
// not heartbeat thinning: thinning the interval widens the silent window this
// exists to bound, while consolidation reduces the number of sockets that need
// proving at all.
func (b *RedisBus) publishHeartbeats() {
	b.mu.Lock()
	if !b.publishHeartbeat {
		b.mu.Unlock()
		return
	}
	workspaces := make([]string, 0, len(b.wsSubs))
	for ws := range b.wsSubs {
		workspaces = append(workspaces, ws)
	}
	b.mu.Unlock()

	for _, ws := range workspaces {
		channel := b.keys.Name(redisChannelSuffix) + ws
		if err := b.client.Publish(b.ctx, channel, heartbeatPayload).Err(); err != nil {
			// Logged and dropped, never retried. A heartbeat that does not
			// reach Redis is indistinguishable to the receiver from one that
			// was never sent, and the receiver's own threshold is what turns
			// that into a decision — retrying here would only make a wedged
			// publish path look healthier than it is.
			slog.Warn("events: failed to publish a liveness heartbeat; this workspace's subscription will be cycled if the silence reaches the idle timeout",
				"channel", channel, "error", err)
		}
	}
}

// idleCycle is one workspace selected for cycling, with the establishment
// record its selection minted.
type idleCycle struct {
	workspaceID string
	gen         int64
	pending     *pendingSub
}

// cycleIdleSubscriptions ends coverage for every workspace whose subscription
// has received nothing for idleTimeout, and REPLACES that subscription.
//
// DROP-AND-CYCLE, NOT DROP ALONE, and the difference is the whole remedy. A
// half-open route stays half-open: dropping coverage makes the next resume
// honest, but the instance is still attached to a dead socket, so the resync it
// just demanded is served from the same dead subscription and the detector
// fires again on the next tick. That is a resync loop metering the failure at
// 3T intervals, not a recovery. Cycling is what restores delivery.
//
// THE IDLE DETECTOR IS A THIRD ACTOR IN THIS REGION, and every invariant here
// was designed around the other two. Until now only request goroutines mutated
// wsSubs/pendingSubs, plus Close; this one is a background mutator with no
// request behind it. Four hazards, each with the rule that answers it:
//
//  1. CYCLING ACROSS AN IN-FLIGHT ESTABLISHMENT. If pendingSubs holds a record,
//     an establishment is already running and may be about to install over what
//     a cycle just tore down — or the cycle installs a second subscription and
//     the single-establisher wall is breached from a direction it was never
//     guarded against. RULE: the detector takes the same wall. It refuses to
//     cycle while a record exists and waits for the next tick, and when it does
//     cycle it MINTS the record itself, under b.mu, before tearing anything
//     down. Refusing is the cheaper correct answer: an establishment in flight
//     is itself evidence of imminent traffic.
//
//  2. A FRESHLY INSTALLED SUBSCRIPTION LOOKS IDLE. A zero lastSeen reads as
//     1970 and fires the detector on the next tick. RULE: lastSeen is stamped
//     at INSTALL time, not only on inbound frames — see establishSubscription.
//     This matters most in exactly the case BUG-2747 exists for, an unconfirmed
//     admission where no confirmation ever arrives to stamp it, so the naive
//     version would cycle hardest on the workspaces already having a bad time.
//
//  3. CYCLING A WORKSPACE NOBODY WANTS. wsCounts may reach zero between the
//     tick's read and the cycle. RULE: re-check under the SAME lock that
//     performs the teardown — the deregister-before-arbitration ordering
//     BUG-2749 established, applied to a new caller. THESE CHECKS ARE AN
//     OPTIMISATION RATHER THAN A CORRECTNESS GUARD, and the mutation matrix is
//     what says so rather than an argument: removing the whole second read
//     survives every test here, because establishSubscription's own abandon
//     path already refuses to install for an empty workspace and retires the
//     record in the same critical section. What the checks buy is a dial not
//     paid for. See cycleOne for the per-term reading.
//
//  4. NO REQUEST CONTEXT TO ESTABLISH ON. RULE: the re-establishment runs on
//     b.ctx, which establishSubscription's cancellation path already tolerates
//     (never cancelled until Close). That path's comments are all written in
//     terms of "the caller"; here the caller is the bus, and it passes a nil
//     establisher because it has no subscriber registration of its own to
//     unwind.
func (b *RedisBus) cycleIdleSubscriptions() {
	now := b.now()

	var due []idleCycle
	b.mu.Lock()
	// DETECTION IS GATED ON PUBLISHING, and getting this wrong is the defect
	// codex round 1 found in the first draft of this unit (P2, and it is a P1
	// in effect). Idle detection ran on every instance from phase 1, justified
	// in a comment as "detecting off whatever traffic the deployment already
	// carries" — which is true only of a BUSY workspace. On a QUIET one, phase
	// 1 has no traffic to detect off and no heartbeat either, so a perfectly
	// healthy subscription crosses the threshold every 90-120s and is cycled:
	// coverage dropped, every live subscriber told to resync, forever, on the
	// DEFAULT configuration every deployment lands in first. That is the exact
	// load-posture inversion this family keeps having to avoid, shipped as the
	// default.
	//
	// An instance detects off its OWN frames — it publishes to the workspace
	// channels it subscribes to and receives them back — so it never depends
	// on peers having flipped. Publishing and detecting are therefore one
	// capability with one switch, and phase 1 is exactly "recognise the frame
	// so a phase-2 peer costs you nothing".
	if !b.publishHeartbeat {
		b.mu.Unlock()
		return
	}
	idleTimeout := b.idleTimeout
	for ws, sub := range b.wsSubs {
		if _, inFlight := b.pendingSubs[ws]; inFlight {
			continue // rule 1
		}
		if b.wsCounts[ws] == 0 {
			// RULE 3, FIRST READ. Also an optimisation rather than a guard, and
			// for a sharper reason than the second read's: reaching zero takes
			// the subscription down with it (Unsubscribe's count-to-zero branch
			// calls stopRedisSubscription), so a workspace at zero has no
			// wsSubs entry and this loop never sees it. Removing this line
			// survives every test. Kept as a cheap statement of the intended
			// precondition rather than as a load-bearing check.
			continue
		}
		// NO `lastSeen.IsZero()` SKIP HERE, and its absence is deliberate.
		// Treating an unstamped subscription as "not idle" reads as a safe
		// belt-and-braces guard next to rule 2, and is the exact opposite: it
		// would make a subscription that has NEVER received anything
		// permanently uncyclable — which is the BUG-2747 unconfirmed
		// admission, the one case the plan singles out as mattering most.
		// A route that wedges before the acknowledgement arrives would then be
		// undetectable forever, in the population already having the worst
		// time. The install-time stamp (rule 2) is what makes a zero value
		// unreachable for an installed subscription; a guard here would only
		// mask it. Found by the mutation matrix: with the skip present,
		// removing the install stamp survived every test; with it gone, that
		// mutation is caught.
		//
		// RE-ADDING THE SKIP IS ITSELF UNDETECTABLE, and that is the correct
		// reading rather than a coverage gap: rule 2 makes a zero lastSeen
		// unreachable, so the branch would never be taken — until the day rule
		// 2 regressed, which is the day it would hide the regression. An
		// unreachable guard that only acts when a real one has already broken
		// is worse than no guard, because it converts a caught defect into a
		// silent one. This comment is the enforcement; there is no test that
		// can be.
		if now.Sub(sub.lastSeen) < idleTimeout {
			continue
		}
		// Minting the record HERE is what makes rule 1 hold in the other
		// direction too: from this moment a subscriber arriving for this
		// workspace joins the establishment we are about to run instead of
		// finding the doomed subscription live and being admitted into it.
		// subscribeAndReplay checks pendingSubs BEFORE wsSubs precisely so
		// that this overlap is safe.
		pending := &pendingSub{done: make(chan struct{})}
		b.pendingSubs[ws] = pending
		due = append(due, idleCycle{workspaceID: ws, gen: sub.gen, pending: pending})
	}
	b.mu.Unlock()

	for _, c := range due {
		b.cycleOne(c, idleTimeout)
	}
}

// cycleOne ends one workspace's coverage and re-establishes its subscription.
func (b *RedisBus) cycleOne(c idleCycle, idleTimeout time.Duration) {
	slog.Warn("events: no traffic on this workspace's Redis subscription within the idle timeout; ending its replay coverage and replacing the connection, resumes across the silence will report sync_required",
		"workspace", c.workspaceID, "idle_timeout", idleTimeout)

	// BEFORE THE TEARDOWN, because dropWorkspaceCoverage authenticates against
	// the LIVE subscription's generation (see its generation check) and
	// stopRedisSubscription deletes that entry. Reversed, the drop would find
	// no wsSubs entry, return without dropping the buffer, and leave a stale
	// buffer vouching for a span that ended when the route did — which is the
	// exact defect this unit exists to remove.
	b.dropWorkspaceCoverage(c.workspaceID, ResetReasonIdleTimeout, c.gen)

	b.mu.Lock()
	// RULE 3, SECOND READ — under the lock that performs the teardown. Between
	// the scan and here, the last subscriber may have left (taking the
	// subscription down with it), the workspace may have been re-established
	// under a new generation, or the bus may have closed. Any of those means
	// there is nothing of OURS left to cycle, and installing a fresh
	// subscription would be a connection and a receive loop for nobody.
	//
	// WHAT THE MUTATION MATRIX SAYS ABOUT THIS LINE, recorded because the
	// honest reading is not the flattering one. Removing the liveness term,
	// the generation term, the count term, or ALL of them survives every test
	// in this package. That is not a coverage gap to be filled with a cleverer
	// test — it is the correct reading, and each term has a different reason:
	//
	//   - The COUNT and LIVENESS terms are redundant with
	//     establishSubscription, which re-reads wsCounts under its own
	//     deciding lock and abandons — retiring the record in that same
	//     section — when the workspace has emptied (BUG-2749). Dropping them
	//     costs a dial that is immediately thrown away, not a wrong outcome.
	//     They are kept because they do not DEPEND on that coupling: a future
	//     change to the abandon path would otherwise silently make this caller
	//     install for nobody.
	//   - The GENERATION term is unreachable while we hold the establishment
	//     record at all, by rule 1's own mechanism: subscribeAndReplay checks
	//     pendingSubs before wsSubs, so no other caller can establish this
	//     workspace between the scan and here, and nothing else installs. It
	//     is kept for the same reason every other gen check in this file is —
	//     the cost of being wrong is tearing down a subscription that belongs
	//     to someone else.
	//
	// Do not read the survivals as dead code to delete, and do not read them as
	// tested defence in depth. They are a cheap second opinion whose absence is
	// currently harmless.
	sub, live := b.wsSubs[c.workspaceID]
	if !live || sub.gen != c.gen || b.wsCounts[c.workspaceID] == 0 || b.ctx.Err() != nil {
		b.retirePendingLocked(c.workspaceID, c.pending)
		b.mu.Unlock()
		close(c.pending.done)
		return
	}
	b.stopRedisSubscription(c.workspaceID)
	b.mu.Unlock()

	// RULE 4: b.ctx, and a nil establisher. establishSubscription owns the
	// record from here — it installs or abandons, and retires the record in
	// the same critical section either way, so no joiner is stranded by a
	// cycle any more than by a cancelled caller (BUG-2749).
	b.establishSubscription(b.ctx, c.workspaceID, nil, c.pending)

	// REPORTED AFTER ESTABLISHMENT, not before it (codex round 1, P3). While
	// this cycle holds the workspace's establishment record, any caller that
	// reaches Subscribe for the same workspace WAITS on it — including an
	// Observer callback, which this package already treats as allowed to call
	// back into the bus. Reporting from inside that window would let such a
	// callback block on a record only this goroutine can retire.
	//
	// The narrower version of the same hazard is older than this code and is
	// not fixed here: dropWorkspaceCoverage above reports SequenceReset while
	// the record is held, exactly as confirmSubscription's late-acknowledgement
	// path already did. See the Observer interface for the contract that
	// bounds it.
	b.reportSubscriptionCycled()
}
