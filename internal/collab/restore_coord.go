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
// freezes the room. It marks a restore in progress so new applier round-trips block
// in enterApplierGate. It does NOT drain: in-flight round-trips are finalized by the
// freeze itself (see (*Room).freezeAndFinalizePending), so there is nothing to wait
// on here — beginRestore never blocks. Paired with resolveRestore (deferred by the
// caller so it runs on every return path).
func (r *Room) beginRestore() {
	r.restoreMu.Lock()
	r.restoreActive = true
	if r.restoreResolved == nil {
		r.restoreResolved = make(chan struct{})
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
// ApplyExternalContent from scratch against the now-resolved room. The common path
// (no restore) returns immediately with waited=false.
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
	r.restoreMu.Unlock()
	return waited
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
