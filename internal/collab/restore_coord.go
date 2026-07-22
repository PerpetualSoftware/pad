package collab

// Version-restore ↔ designated-applier coordination (BUG-2276 residual 2).
//
// Problem. A version restore (ForceRefreshRoom) freezes every connected conn for
// the duration of its prune+reseed transaction. The designated-applier round-trip
// (ApplyExternalContent) routes an external content update through a connected tab,
// which applies the markdown to its Y.Doc (producing persisted sync frames) and
// then acks. If a restore's transient frozen=true window landed between the
// applier's frames and its ack, the ack was dropped; on a restore ROLLBACK that
// loss was ambiguous — did the content land (frames persisted before the freeze)
// or not (frames dropped during the freeze)? — and the applier flow could
// double-apply markdown, clobbering peer edits.
//
// Fix — durable persistence correlation, decided atomic with the freeze (NOT a
// timer). The applier round-trip's fate is read from DURABLE op-log state:
//
//   - The client brackets its setContent with an applier_apply_start{request_id}
//     control frame sent IMMEDIATELY before setContent (synchronously, so no
//     concurrent keystroke frame can interleave) and the applier_ack{request_id}
//     right after. The server captures the applier conn's (lastPersistedOpID,
//     frozenDropSeq) at apply_start — the bracket baseline — so the bracket
//     contains exactly the setContent frame. Attribution is airtight.
//
//   - On the ack (all bracket frames processed by readLoop), the round-trip
//     resolves to SUCCESS iff no frame in the bracket was frozen-dropped
//     (frozenDropSeq unchanged) — i.e. setContent wasn't lost to a freeze (a
//     no-op-diff setContent, which persists nothing, also resolves to success).
//
//   - When a restore freezes, ForceRefreshRoom FINALIZES every in-flight round-trip
//     UNDER appendMu, atomic with setting frozen: the round-trip persisted iff its
//     applier conn's lastPersistedOpID advanced past the bracket baseline. Because
//     the read and the freeze share the appendMu critical section, any frame still
//     in flight is blocked on appendMu and will be dropped once the freeze is
//     visible — so lastPersistedOpID-not-advanced is a DURABLE "did not land". There
//     is no window in which a persisted edit is seen as not-persisted, and (via the
//     apply_start bracket) none in which typing is mistaken for the applier's frame.
//
//   - A NEW round-trip that starts while a restore holds the room WAITS for it to
//     resolve (enterApplierGate) before electing, then re-elects: on ROLLBACK the
//     conns are unfrozen (apply fresh), on COMMIT they are force-closed (no applier
//     → ErrNoApplierAvailable → the caller's direct-write fallback, unchanged).
//
// The common no-restore path pays only a single leaf-mutex check at gate entry
// (restoreActive==false → proceed) plus the two extra durable-signal atomics on the
// persist path; its ack resolves to success with no added latency (hard constraint
// 1). Every wait is bounded to the restore-tx duration, never the 30s applier
// timeout (hard constraint 2). restoreMu is a strict leaf; lock order is unchanged
// (hard constraint 5).

// beginRestore is called by ForceRefreshRoom under the per-item lock, BEFORE it
// freezes the room. It (1) marks a restore in progress so new applier round-trips
// block in enterApplierGate, and (2) DRAINS the admission gap — waits for every
// round-trip that already passed the gate but hasn't finished registering its
// pending-ack entry to register (admittedUnregistered → 0). Without this drain a
// round-trip admitted just before beginRestore could register AFTER
// freezeAndFinalizePending's scan and be missed (BUG-2276 residual 2, P1): the
// nonresponsive case would hang the applier timeout and a no-op setContent could
// falsely ack.
//
// The drain is bounded to the enter→register window, which is mutex-only (pickApplier
// + registerPendingAck, no I/O) — microseconds, NEVER the applier round-trip. It
// cannot stall: it waits only for registration to complete, and a round-trip that is
// forced to restart (enterApplierGate returned waited) was never counted as admitted.
// Paired with resolveRestore (deferred by the caller so it runs on every return path).
func (r *Room) beginRestore() {
	r.restoreMu.Lock()
	r.restoreActive = true
	if r.restoreResolved == nil {
		r.restoreResolved = make(chan struct{})
	}
	for r.admittedUnregistered > 0 {
		r.restoreCond.Wait()
	}
	r.restoreMu.Unlock()
}

// resolveRestore releases the gate when the restore RESOLVES (commit, rollback, or
// uncertain): it clears restoreActive and closes restoreResolved, waking every
// gate-blocked or finalized-and-parked round-trip so they re-elect against the
// post-restore room. ForceRefreshRoom defers it immediately after beginRestore.
func (r *Room) resolveRestore() {
	r.restoreMu.Lock()
	r.restoreActive = false
	if r.restoreResolved != nil {
		close(r.restoreResolved)
		r.restoreResolved = nil
	}
	r.restoreMu.Unlock()
}

// enterApplierGate is called at the start of each applier election. It blocks while
// a version restore holds the room and reports whether it had to wait. When it
// waited, the caller must NOT proceed with a stale election — it restarts
// ApplyExternalContent from scratch against the now-resolved room (and is NOT counted
// as admitted). The common path (no restore) returns immediately with waited=false
// and ADMITS the round-trip: it is counted in admittedUnregistered until the caller
// calls finishAdmission (after registering, or on any early bail before registering),
// so a concurrent beginRestore drains it before the freeze scan (P1). MUST be paired
// with exactly one finishAdmission on every !waited path.
func (r *Room) enterApplierGate() (waited bool) {
	r.restoreMu.Lock()
	for r.restoreActive {
		waited = true
		resolved := r.restoreResolved
		r.restoreMu.Unlock()
		if resolved != nil {
			<-resolved // bounded to the restore-tx duration
		}
		r.restoreMu.Lock()
	}
	if !waited {
		r.admittedUnregistered++
	}
	r.restoreMu.Unlock()
	return waited
}

// finishAdmission ends the admission span opened by a !waited enterApplierGate — call
// it exactly once after registerPendingAck returns (success or error) or on any early
// bail before registration. When the count hits zero it wakes a beginRestore draining
// the admission gap. (BUG-2276 residual 2, P1.)
func (r *Room) finishAdmission() {
	r.restoreMu.Lock()
	if r.admittedUnregistered > 0 {
		r.admittedUnregistered--
	}
	if r.admittedUnregistered == 0 {
		r.restoreCond.Broadcast()
	}
	r.restoreMu.Unlock()
}

// waitRestoreResolved blocks until the in-progress restore (if any) resolves. Called
// by a round-trip the restore finalized to not-persisted, so it doesn't re-elect
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
