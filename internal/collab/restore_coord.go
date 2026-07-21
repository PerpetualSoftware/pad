package collab

import "time"

// Version-restore ↔ designated-applier coordination (BUG-2276 residual 2).
//
// Problem. A version restore (ForceRefreshRoom) freezes every connected conn for
// the duration of its prune+reseed transaction. The designated-applier round-trip
// (ApplyExternalContent) routes an external content update through a connected tab,
// which applies the markdown to its Y.Doc (producing persisted sync frames) and
// then sends an applier_ack. If a restore's transient frozen=true window lands
// between the applier's frames and its ack, readLoop drops the ack. On a restore
// ROLLBACK (op-log NOT pruned) that ack loss is ambiguous:
//   - (A) the applier's frames persisted BEFORE the freeze → the external content
//     DID land → the round-trip should be treated as SUCCESS, no retry.
//   - (B) the applier's frames were dropped DURING the freeze → the content did
//     NOT land → the round-trip must retry (re-apply after unfreeze).
// The dropped ack can't distinguish (A) from (B), so the applier flow retries and
// can double-apply markdown, clobbering peer edits made in the retry window.
//
// Fix — release-on-resolve. Rather than resolve the ambiguity after the fact, we
// PREVENT it: an applier round-trip and a restore's freeze window are made mutually
// exclusive per item.
//
//   - A restore, before it freezes (beginRestore), preempts any ALREADY in-flight
//     applier round-trip and BLOCKS until every one has drained. Preempted
//     round-trips get a bounded grace during which — because the room is still
//     UNFROZEN — a genuinely delivered ack still resolves them as SUCCESS
//     (sub-case A); otherwise they abandon their attempt and park (sub-case B).
//     So when frozen=true is finally set, there is NO pending applier ack to drop:
//     the residual-2 race is gone at the source.
//
//   - A new applier round-trip WAITS for an in-progress restore to resolve
//     (enterApplierGate) before it elects an applier. After resolve it re-elects
//     from scratch: on ROLLBACK the conns are unfrozen (a live applier exists → it
//     applies fresh, no ambiguity); on COMMIT the conns are force-closed (no applier
//     → ErrNoApplierAvailable → the caller's direct-write fallback, unchanged).
//
// The common path — an applier round-trip with NO restore in progress — pays only a
// single leaf-mutex lock/unlock + counter bump at gate entry/exit; its ack-wait
// select watches an open channel that never fires, so its latency and behavior are
// byte-for-byte unchanged (hard constraint 1). Every serialization wait is bounded
// to the restore-tx duration (plus, for a preempted in-flight round-trip, the short
// applierPreemptGrace), never to the 30s applier timeout (hard constraint 2).

// applierPreemptGraceVar is how long a preempted in-flight applier round-trip keeps
// watching for its ack after a restore signals it to yield, BEFORE it abandons the
// attempt. The preempt fires while the room is still UNFROZEN, so an ack the browser
// already sent (trailing its now-persisted sync frames on the same ordered conn) is
// still delivered normally within this window — letting a round-trip whose content
// already landed resolve as SUCCESS (sub-case A) instead of parking and re-applying.
// It only needs to cover socket→readLoop processing latency for an ack that TCP
// ordering guarantees is already received-or-imminent; it is NOT the 30s applier
// timeout, and it is paid ONLY when a restore actively preempts a genuinely
// in-flight round-trip (never on the common no-restore path). A var so tests can
// shrink/stretch it.
var applierPreemptGraceVar = 500 * time.Millisecond

func applierPreemptGrace() time.Duration { return applierPreemptGraceVar }

// applierMaxRestartsAfterRestore caps how many times ApplyExternalContent re-elects
// after being preempted/blocked by a restore, so a pathological back-to-back restore
// storm can't spin the election forever. On exhaustion the caller falls back to a
// direct write (ErrNoApplierAvailable) — safe graceful degradation.
const applierMaxRestartsAfterRestore = 5

// beginRestore is called by ForceRefreshRoom under the per-item lock, BEFORE it
// freezes the room's conns. It marks a restore in progress (so new applier
// round-trips block in enterApplierGate) and preempts any ALREADY in-flight
// round-trip (by closing the preempt channel they captured), then BLOCKS until
// every in-flight round-trip has drained to zero. On return the room is safe to
// freeze: no applier ack can race the frozen window, because there is no in-flight
// applier round-trip left. Paired with resolveRestore (deferred by the caller).
//
// The drain is bounded: each preempted round-trip yields within applierPreemptGrace,
// so this never blocks for the 30s applier timeout. A room with no in-flight
// round-trips returns immediately.
//
// Precondition (guaranteed by ForceRefreshRoom holding itemLock): no other restore
// is in progress for this room, so restorePreempt is open and closing it is safe.
func (r *Room) beginRestore() {
	r.restoreMu.Lock()
	r.restoreActive = true
	if r.restoreResolved == nil {
		r.restoreResolved = make(chan struct{})
	}
	// Wake every in-flight round-trip that captured this preempt channel.
	close(r.restorePreempt)
	// Drain: wait for all in-flight round-trips to yield. restoreCond is broadcast
	// by exitApplierGate when the count hits 0.
	for r.applierInFlight > 0 {
		r.restoreCond.Wait()
	}
	r.restoreMu.Unlock()
}

// resolveRestore releases the gate when the restore RESOLVES (commit, rollback, or
// uncertain). It clears restoreActive, closes restoreResolved (waking every parked /
// gate-blocked applier round-trip so they re-elect against the post-restore room),
// and installs a fresh preempt channel for the next restore cycle. ForceRefreshRoom
// defers this immediately after beginRestore so it runs on every return path.
func (r *Room) resolveRestore() {
	r.restoreMu.Lock()
	r.restoreActive = false
	if r.restoreResolved != nil {
		close(r.restoreResolved)
		r.restoreResolved = nil
	}
	// The old preempt channel is closed; hand out a fresh open one so future
	// round-trips capture a channel that only fires on the NEXT restore.
	r.restorePreempt = make(chan struct{})
	r.restoreMu.Unlock()
}

// enterApplierGate is called at the start of each applier election attempt. It
// blocks while a version restore holds the room, then registers this round-trip as
// in-flight and returns the preempt channel the caller's ack-wait select must watch.
//
// `waited` reports whether it had to block for an in-progress restore: when true the
// caller must NOT proceed with a stale election — it exits the gate and restarts
// ApplyExternalContent from scratch against the now-resolved room. The common path
// (no restore) returns immediately with waited=false and the room's current open
// preempt channel (which never fires unless a restore begins mid-round-trip).
func (r *Room) enterApplierGate() (preempt <-chan struct{}, waited bool) {
	r.restoreMu.Lock()
	defer r.restoreMu.Unlock()
	for r.restoreActive {
		waited = true
		resolved := r.restoreResolved
		r.restoreMu.Unlock()
		if resolved != nil {
			<-resolved // bounded to the restore-tx duration
		}
		r.restoreMu.Lock()
	}
	r.applierInFlight++
	return r.restorePreempt, waited
}

// exitApplierGate deregisters an in-flight applier round-trip and, when the count
// reaches zero, wakes a restore draining in beginRestore. Must be paired with each
// successful enterApplierGate.
func (r *Room) exitApplierGate() {
	r.restoreMu.Lock()
	if r.applierInFlight > 0 {
		r.applierInFlight--
	}
	if r.applierInFlight == 0 {
		r.restoreCond.Broadcast()
	}
	r.restoreMu.Unlock()
}

// waitRestoreResolved blocks until the in-progress restore (if any) resolves. Called
// by a preempted round-trip after it has exited the gate, so it doesn't re-elect
// until the restore's effects (unfreeze on rollback / force-close on commit) are in
// place. Bounded to the restore-tx duration.
func (r *Room) waitRestoreResolved() {
	r.restoreMu.Lock()
	resolved := r.restoreResolved
	active := r.restoreActive
	r.restoreMu.Unlock()
	if active && resolved != nil {
		<-resolved
	}
}
