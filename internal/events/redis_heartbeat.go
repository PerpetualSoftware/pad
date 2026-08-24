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
// WHAT THIS DETECTS AND WHAT IT DOES NOT, stated as precisely as the mechanism
// actually supports (codex round 10, which was asked to refute the claim rather
// than to look for defects, and partly succeeded). All three limits below were
// checked against go-redis v9.22.0 rather than reasoned about:
//
//   - IT IS A RECEIVE-SIDE DETECTOR, not a round-trip health check. What it
//     measures is whether frames ARRIVE on this workspace's subscription. A
//     subscription that receives fine but whose outbound direction is broken
//     looks healthy here — correctly, since nothing is being lost.
//
//   - IT DOES NOT COVER THE PUBLISH PATH, and cannot: PUBLISH travels on the
//     client's connPool while a subscription holds a connection from the
//     separate pubSubPool (redis.go:363, :1956). Those are different sockets
//     with different fates, so a wedged publish path is invisible to this, and
//     a reconnect of one repairs nothing about the other. An instance whose
//     publishes fail loses ITS OWN events for everyone; that is a different
//     failure needing a different signal.
//
//   - REPLACEMENT IS ATTEMPTED, NOT GUARANTEED. If the network path is still
//     blackholed when the cycle re-dials, the replacement cannot receive
//     either, and the detector fires again on the next pass. That is the
//     honest behaviour — coverage stays ended, so nothing is claimed falsely —
//     but "delivery resumes" is a statement about the network, not about this
//     code. See BUG-2764 for a case where the replacement can fail silently
//     even on a healthy path.
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
	// is not a cycle.
	//
	// THE LATENCY ARITHMETIC, corrected after the loops were split (codex
	// round 8; the earlier wording described a shared ticker that no longer
	// exists). Measured FROM lastSeen, detection lands in [3T, 4T) — the scan
	// runs on its own T-cadence, so it adds up to one interval on top of the
	// threshold. Measured from FAULT ONSET it is wider and less tidy, roughly
	// [2T, 4T): the publisher has its own independent phase, so the last frame
	// to get through may have been sent anywhere in the interval before the
	// route died. A pass that overruns widens both ends further. Quote the
	// from-lastSeen figure when reasoning about the code and the from-onset
	// one when telling an operator how long an incident hides.
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
// carry fields without needing a third roll: a phase-1 binary already ignores
// a v2 frame it knows nothing about.
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

// heartbeatMaxLen bounds a frame this package will accept as one of its own.
// A liveness frame carries a version and, at most, a few short tokens; anything
// larger is something else wearing the prefix.
const heartbeatMaxLen = 64

// isHeartbeat reports whether a payload is a bus-internal liveness frame.
//
// THE SHAPE IS VALIDATED, NOT JUST THE PREFIX (codex round 5, P2), and the
// first draft got this wrong in a way worth recording. Accepting any "hb|…"
// created a silently-ignored class on the workspace event channel where
// previously EVERY unreadable payload ended coverage loudly and moved
// undecodable_message — the counter whose documented job is "suspect a
// namespace collision". A foreign or buggy publisher whose payload happened to
// start with "hb|" would have slipped through that signal without a trace.
//
// What is NOT a problem, and was considered: a forged frame cannot fake
// liveness. Liveness here means "this socket carried traffic", and a frame
// that ARRIVES demonstrates exactly that whoever sent it — which is why
// stampLastSeen fires for undecodable frames too. There is no claim about
// event coverage in a heartbeat to forge.
//
// So the rule is conservative in the direction that keeps the loud path loud:
// "hb|" then a decimal version, then optional "|"-separated tokens from a
// narrow charset, under a length cap. A disciplined future frame still needs
// no third roll; arbitrary bytes wearing the prefix go back to being a
// coverage-ending decode failure.
func isHeartbeat(payload string) bool {
	if len(payload) > heartbeatMaxLen || !strings.HasPrefix(payload, heartbeatPrefix) {
		return false
	}
	fields := strings.Split(payload[len(heartbeatPrefix):], "|")
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
// SCHEDULED FROM A DEADLINE, NOT FROM THE END OF THE LAST PASS (codex round 5,
// P2). Restarting the timer after work() makes the real period T plus however
// long the pass took, and for the PUBLISHER that is self-defeating: an instance
// whose publishes are slow emits heartbeats further apart, its own subscription
// sees them further apart, and it can cross its own 3T threshold and cycle
// connections that were never wedged. The slowness would manufacture the
// incident.
//
// When a pass overruns badly the schedule is reset to now rather than firing
// the missed ticks back-to-back: there is no value in a burst of heartbeats,
// and a burst of idle scans would hammer a Redis that is already struggling.
func (b *RedisBus) tickForever(kick <-chan struct{}, work func()) {
	next := time.Now()
	for {
		b.mu.Lock()
		interval := b.heartbeatInterval
		b.mu.Unlock()

		next = nextTick(next, interval, time.Now())
		if wait := time.Until(next); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-b.ctx.Done():
				timer.Stop()
				return
			case <-kick:
				// The cadence changed under us; drop this wait, re-read it, and
				// re-base the schedule so the new interval starts now rather
				// than from a deadline computed under the old one.
				timer.Stop()
				next = time.Now()
				continue
			case <-timer.C:
			}
		}

		// CHECKED AGAIN AFTER THE WAIT, and this is NOT redundant with the
		// ctx.Done arm above even though removing either one alone survives
		// every test (mutation matrix; team lesson: a pair that only dies
		// together is a question, not a clearance). They cover disjoint
		// moments and each is independently right:
		//
		//   - The select arm is the exit while WAITING, which is where this
		//     goroutine spends essentially all of its life. Without it a closed
		//     bus leaves both loops sleeping out a full interval before
		//     noticing, every interval, forever.
		//   - This check is the exit after the timer has already fired, so a
		//     bus that closed DURING the previous pass does not start another
		//     one. Without it, work() runs once more against a cancelled
		//     context: the publish half writes to Redis on a dead ctx and the
		//     idle half takes b.mu after Close has drained it.
		//
		// Removing BOTH is detected, by TestClosingTheBusStopsTheMaintenanceLoop.
		if b.ctx.Err() != nil {
			return
		}
		work()
	}
}

// PublishHeartbeatsForTest runs one publish pass, for tests in OTHER packages.
//
// cmd/pad's wiring test needs to know whether the config flip reached this
// bus's constructor, and driving the pass DIRECTLY is what makes that test
// deterministic (codex round 7). Shortening the cadence and waiting instead
// makes the negative arm — a phase-1 bus must publish nothing — a race against
// the scheduler: under -race or a loaded CI box the goroutine may simply not
// have run yet, which is indistinguishable from a bus that is correctly
// silent. The loop's own wiring is covered inside this package, where the
// unexported cadence setter is available.
//
// Named so no production caller reaches for it.
func (b *RedisBus) PublishHeartbeatsForTest() { b.publishHeartbeats() }

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
	// THE GENERATION IS PART OF THE SNAPSHOT (codex round 14). The publish
	// below happens off the lock and can take as long as go-redis's timeouts
	// allow, during which this workspace's subscription may be torn down and
	// replaced — by a cycle, or by its last subscriber leaving and a new one
	// arriving. Stamping "whatever occupies this workspace now" would then
	// credit a probe to a subscription that never received one, and the next
	// pass's failures could cycle it while it looked recently probed. Same
	// hazard stampLastSeen already guards against, on the same map.
	probes := make([]idleProbe, 0, len(b.wsSubs))
	for ws, sub := range b.wsSubs {
		probes = append(probes, idleProbe{workspaceID: ws, gen: sub.gen})
	}
	b.mu.Unlock()

	for _, p := range probes {
		ws := p.workspaceID
		channel := b.keys.Name(redisChannelSuffix) + ws
		if err := b.client.Publish(b.ctx, channel, heartbeatPayload).Err(); err != nil {
			// Logged and dropped, never retried: retrying here would only make
			// a wedged publish path look healthier than it is.
			//
			// AND DELIBERATELY NOT STAMPED. lastProbeOK stays where it was, so
			// the detector stops treating this workspace's silence as evidence
			// — see that field. A failure to PROBE is not a finding about the
			// peer, and counting it as one tears down healthy connections
			// whenever this instance's outbound path is the broken one.
			slog.Warn("events: failed to publish a liveness heartbeat; this workspace's idle detection is suspended until a probe succeeds, because silence cannot be read as a finding when we could not ask",
				"channel", channel, "error", err)
			b.reportHeartbeatPublishFailed()
			continue
		}
		if b.afterProbePublish != nil {
			b.afterProbePublish(ws)
		}

		b.mu.Lock()
		if sub, ok := b.wsSubs[ws]; ok && sub.gen == p.gen {
			sub.lastProbeOK = b.now()
		}
		b.mu.Unlock()
	}
}

// liveGen reports the generation of the workspace's installed subscription, and
// whether there is one at all.
//
// Distinct from currentSubGen, which answers zero for both "no subscription"
// and a genuine zero — a distinction the cycle needs, because "nothing was
// installed" and "something was installed" are the two outcomes it reports on.
func (b *RedisBus) liveGen(workspaceID string) (int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sub, ok := b.wsSubs[workspaceID]
	if !ok {
		return 0, false
	}
	return sub.gen, true
}

// idleProbe is one workspace selected for a heartbeat, with the generation of
// the subscription the probe is FOR. The generation travels with it so a slow
// publish cannot credit a subscription that replaced the one it was sent for.
type idleProbe struct {
	workspaceID string
	gen         int64
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
		// THE PREMISE HAS TO HOLD BEFORE THE CONCLUSION IS DRAWN. Silence only
		// means "the receive path is dead" if we actually managed to send
		// something into it AFTER the silence began; see redisSub.lastProbeOK.
		//
		// EXPRESSED AS AN ORDERING, not as an age, and the honest reason is
		// weaker than the one this comment first gave. Codex round 16 argued
		// an age-based form ("has a probe succeeded within the threshold")
		// failed to suspend detection where this one would; the mutation
		// matrix then declined to confirm it — reverting to the age form, and
		// even removing cycleOne's copy too, breaks no test, and no case could
		// be constructed that separates them. On any healthy path the two
		// stamps advance TOGETHER, because a probe whose frame arrives sets
		// both; they diverge only on the wedge, where both forms cycle.
		//
		// It is kept because it says exactly what the rule means — we have
		// sent something into this subscription more recently than anything
		// came out of it — and is never weaker. Not because it was shown to
		// fix a reachable defect. A fresh subscription has the two equal, so it
		// is never cycled before its first successful probe.
		//
		// CHECKED HERE AND AGAIN IN cycleOne. Removing either alone leaves the
		// tests green, and so does removing both, for the reason above; the
		// pair is justified by what it expresses, not by the matrix.
		//
		// The two placements still cover different moments: this one keeps a
		// workspace off the due list at all, so no establishment record is
		// minted and no joiner is made to wait, while cycleOne's covers the
		// probe failing AFTER selection — a window the concurrency cap makes
		// real. Neither subsumes the other.
		if !sub.lastProbeOK.After(sub.lastSeen) {
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

	// BOUNDED-PARALLEL, NOT SERIAL (codex round 5, P2). Each cycle re-dials,
	// and a dial against a struggling Redis is bounded by go-redis's own
	// timeouts rather than by anything here — so a serial pass makes recovery
	// take N x that timeout, and the workspaces at the end of the map wait the
	// longest while still reporting themselves uncovered. The failure that puts
	// many workspaces on this list at once is precisely a Redis failover, so
	// the serial case is the common one, not the exotic one.
	//
	// Each entry already owns its own establishment record, minted under the
	// lock above, so they are independent by construction: rule 1 keeps any
	// other caller off a workspace being cycled, and two entries never name the
	// same one.
	//
	// The cap is a deliberate middle: unbounded goroutines would answer a Redis
	// outage by opening one dial per workspace at once, which is the shape that
	// turns a slow dependency into an outage of our own.
	if b.afterIdleScan != nil {
		b.afterIdleScan()
	}

	sem := make(chan struct{}, maxConcurrentCycles)
	var wg sync.WaitGroup
	for _, c := range due {
		wg.Add(1)
		sem <- struct{}{}
		go func(c idleCycle) {
			defer wg.Done()
			defer func() { <-sem }()
			b.cycleOne(c, idleTimeout)
		}(c)
	}
	// WAITED ON, so one pass cannot overlap the next and so a direct caller —
	// every test here — observes a finished pass rather than a started one.
	wg.Wait()
}

// nextTick returns the deadline for the pass after one that was scheduled for
// prev, given the configured interval and the current time.
//
// SEPARATED OUT SO THE ARITHMETIC CAN BE TESTED WITHOUT A CLOCK (mutation
// matrix: restoring the drift survived every test, because the only way to
// observe it in the loop is to time it, and a timing test is a flaky test).
//
// The schedule is deadline-based rather than sleep-after-work, because the
// latter makes the real period T plus however long the pass took. For the
// publisher that is self-defeating: an instance whose publishes are slow emits
// heartbeats further apart, its own subscription sees them further apart, and
// it can cross its own 3T threshold and cycle connections that were never
// wedged — the slowness manufacturing the incident.
//
// When a pass overruns by more than a whole interval the schedule is RESET to
// now rather than firing the missed ticks back to back. A burst of heartbeats
// buys nothing, and a burst of idle scans would hammer a Redis that is already
// struggling — which is precisely the condition that made the pass overrun.
func nextTick(prev time.Time, interval time.Duration, now time.Time) time.Time {
	next := prev.Add(interval)
	if now.Sub(next) > interval {
		return now.Add(interval)
	}
	return next
}

// maxConcurrentCycles bounds how many replacement dials one idle pass has in
// flight. Eight because the work is entirely network-bound and the point is to
// stop N sequential dial timeouts from serialising recovery, not to saturate
// anything: at the 30s cadence this is eight concurrent connects at most once
// per interval, against a Redis that is by definition already in trouble when
// the number is large.
const maxConcurrentCycles = 8

// cycleOne ends one workspace's coverage and re-establishes its subscription.
//
// EVERY VALIDATION, THE COVERAGE DROP AND THE TEARDOWN HAPPEN UNDER ONE LOCK,
// and that is the fix for a false positive codex round 11 found — the property
// this whole design cares about most, because a false positive costs a
// coverage drop and a resync for every subscriber of a healthy workspace.
//
// The scan selects victims and releases the lock; this runs afterwards, and
// "afterwards" can be a long time. The concurrency cap means a workspace can
// wait behind several batches of slow replacement dials, and a GC or CPU pause
// can leave a backlog of heartbeats undrained in the receive loop. In that
// window the subscription can start receiving again — and the earlier version
// cycled it anyway, because its re-checks covered generation, subscriber count
// and bus liveness but never re-asked the question the scan had asked.
//
// The staleness re-check below is therefore not defensive tidying: it is the
// difference between "idle when we looked" and "idle now", and the gap between
// those two was widened by this unit's own concurrency cap.
func (b *RedisBus) cycleOne(c idleCycle, idleTimeout time.Duration) {
	b.mu.Lock()
	sub, live := b.wsSubs[c.workspaceID]

	// RULE 3, SECOND READ, plus the freshness re-check. Between the scan and
	// here, the last subscriber may have left (taking the subscription down
	// with it), the workspace may have been re-established under a new
	// generation, the bus may have closed, or the connection may simply have
	// started working again.
	//
	// WHAT THE MUTATION MATRIX SAYS ABOUT THE FIRST THREE TERMS, recorded
	// because the honest reading is not the flattering one. Removing the
	// liveness term, the generation term, the count term, or all of them
	// survives every test in this package — establishSubscription re-reads
	// wsCounts under its own deciding lock and abandons, retiring the record in
	// that same section (BUG-2749), so dropping them costs a dial that is
	// immediately thrown away rather than a wrong outcome. They are kept
	// because they do not DEPEND on that coupling. The generation term is
	// additionally unreachable while we hold the establishment record, by rule
	// 1's own mechanism. Do not read those survivals as dead code to delete,
	// and do not read them as tested defence in depth.
	//
	// The FRESHNESS term is different in kind: it is load-bearing, it has its
	// own test, and removing it is detected.
	switch {
	case !live || sub.gen != c.gen || b.wsCounts[c.workspaceID] == 0 || b.ctx.Err() != nil:
		b.retirePendingLocked(c.workspaceID, c.pending)
		b.mu.Unlock()
		close(c.pending.done)
		return
	case !sub.lastProbeOK.After(sub.lastSeen):
		// The probe started failing, or something arrived, while this cycle sat
		// in the queue. Same ordering rule as the scan's check: with no
		// successful probe SINCE the last thing we received, we have no
		// evidence about the receive path, so tearing it down would be a
		// guess.
		b.retirePendingLocked(c.workspaceID, c.pending)
		b.mu.Unlock()
		close(c.pending.done)
		return
	case b.now().Sub(sub.lastSeen) < idleTimeout:
		// It recovered while this cycle sat in the queue. Nothing to end and
		// nothing to replace: leaving it alone is the whole point.
		b.retirePendingLocked(c.workspaceID, c.pending)
		b.mu.Unlock()
		close(c.pending.done)
		slog.Info("events: a workspace queued for an idle cycle started receiving again before its turn; leaving its subscription alone",
			"workspace", c.workspaceID)
		return
	}

	// The drop must precede the teardown, because it authenticates against the
	// LIVE subscription's generation and stopRedisSubscription deletes that
	// entry. Both now happen without releasing the lock in between, so there is
	// no window in which coverage is ended for a workspace this function then
	// decides not to cycle.
	report := b.dropWorkspaceCoverageLocked(c.workspaceID, ResetReasonIdleTimeout, c.gen)
	b.stopRedisSubscription(c.workspaceID)
	b.mu.Unlock()

	// LOGGED AFTER THE UNLOCK, and after the decision is final (codex rounds 6
	// and 12). Two separate reasons, both learned the hard way:
	//
	//   - After the DECISION, so the log cannot describe a cycle that then
	//     abandons. It still says ATTEMPTING to replace, because
	//     establishSubscription can install nothing if the bus closes or the
	//     workspace empties while it dials — an operator correlating this line
	//     with pad_event_subscription_cycled_total would otherwise find the log
	//     without the counter and go hunting a bug that is not there.
	//   - After the UNLOCK, because slog runs the installed handler
	//     synchronously and b.mu is the lock every fan-out and every Subscribe
	//     on this instance contends for. A slow or custom handler would stall
	//     all of them, and one that called back into the bus would deadlock.
	slog.Warn("events: no traffic on this workspace's Redis subscription within the idle timeout; ending its replay coverage and attempting to replace the connection, resumes across the silence will report sync_required",
		"workspace", c.workspaceID, "idle_timeout", idleTimeout)

	// Reported with the lock released: an Observer callback may call back into
	// the bus (see the Observer interface for the one thing it may not do).
	if report != "" {
		b.reportReset(report)
	}

	// RULE 4: b.ctx, and a nil establisher. establishSubscription owns the
	// record from here — it installs or abandons, and retires the record in the
	// same critical section either way, so no joiner is stranded by a cycle any
	// more than by a cancelled caller (BUG-2749).
	installed := b.establishSubscription(b.ctx, c.workspaceID, nil, c.pending)
	if !installed {
		// THE OUTCOME IS LOGGED, not only the attempt (codex round 16). The
		// line above says "attempting"; without this an on-call correlating it
		// with pad_event_subscription_cycled_total finds a log with no counter
		// and no explanation, on the one path where that is expected.
		slog.Warn("events: the idle cycle installed no replacement subscription; the workspace was left uncovered because the bus is closing or it lost its last subscriber",
			"workspace", c.workspaceID)
	}

	if b.afterCycleEstablish != nil {
		b.afterCycleEstablish(c.workspaceID)
	}

	// AND ONLY IF A REPLACEMENT ACTUALLY LANDED (codex round 3). The counter's
	// documented meaning is "torn down AND replaced", and establishSubscription
	// has two reasons to install nothing: the bus closed under us, or the
	// workspace emptied while we dialled. Reporting unconditionally would count
	// those as cycles, which is wrong in the direction that matters — an
	// operator reading a non-zero rate concludes connections are being
	// blackholed, and a shutdown would manufacture that signal. The teardown is
	// still visible through the idle_timeout reset reason when a buffer existed
	// to drop.
	//
	// TAKEN FROM THE ESTABLISHMENT ITSELF, not inferred from the live
	// generation afterwards (codex round 13). Inference is wrong in both
	// directions: if this cycle installed nothing and an unrelated caller
	// established the workspace before the check, that caller's subscription
	// was counted as this cycle's replacement; and a real replacement that
	// immediately lost its last subscriber was missed.
	if installed {
		b.reportSubscriptionCycled()
	}
}
