package collab

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Designated-applier protocol (TASK-1257).
//
// The dumb-relay design keeps Y.Doc semantics in the browser. When a
// CLI / API / MCP caller updates an item's content while a co-edit
// session is active, the server can't write items.content directly —
// the connected browser tabs would overwrite it on the next 5s flush
// using their (now stale) Y.Doc state.
//
// The fix is to nominate one connected tab as the "designated
// applier". The server sends it a JSON control message with the new
// markdown; the browser does editor.commands.setContent(markdown),
// which the y-tiptap binding translates into Y.Doc updates that
// propagate via the regular sync path. Once the applier acks, every
// peer is on the new state.
//
// Election: longest-connected wins. Stable choice (the longest
// connection has the most authoritative cumulative Y.Doc state) and
// minimal flicker — newer tabs probably don't have the full history
// yet on a fresh reconnect.
//
// Failure: if the chosen applier doesn't ack within
// applierFirstTimeout, retry with the next-longest-connected client
// (applierRetryTimeout). If no client acks after both attempts,
// caller falls back to direct items.content write — the current
// editors will overwrite it on next flush (data loss vs. graceful
// degradation; the documented contract is "best-effort under
// active co-edit, direct write is the worst case").

const (
	// applierMaxAttempts caps the retry loop. Two is the sweet spot:
	// covers a single slow tab AND a tab that disconnected mid-flow.
	// More attempts mostly just delay the fall-back path.
	applierMaxAttempts = 2

	// controlMessageType values. The browser (TASK-1263) writes a
	// matching applier_ack on completion; any other type the server
	// might add later goes through the same TextMessage envelope.
	ControlMessageApplierRequest = "applier_request"
	ControlMessageApplierAck     = "applier_ack"

	// ControlMessageApplierApplyStart opens the durable persistence bracket for a
	// designated-applier round-trip (BUG-2276 residual 2). The client sends it
	// IMMEDIATELY before setContent (synchronously, no keystroke can interleave), so
	// the server-captured baseline scopes the bracket to exactly the setContent
	// frame — letting the server tell, from durable op-log state, whether that frame
	// persisted or was dropped by a concurrent version-restore freeze. Carries only
	// request_id. Older clients that never send it fall back to a safe "retry on
	// restore" posture (finalization treats an un-bracketed round-trip as
	// not-persisted). See restore_coord.go.
	ControlMessageApplierApplyStart = "applier_apply_start"

	// ControlMessageOpLogCursor advertises the highest item_yjs_updates.id
	// the receiving peer should now consider applied. Sent by the server:
	//   - immediately after a fresh peer's replay completes (cursor =
	//     highest replayed id, or current MAX if no rows existed),
	//   - after every successful AppendYjsUpdate (cursor = the new id),
	//     so each peer's persisted-state knowledge stays current without
	//     a polling round-trip.
	// Clients persist the value per-tab in sessionStorage and announce
	// it on reconnect via `?since=<id>`. Per TASK-1319.
	ControlMessageOpLogCursor = "op_log_cursor"

	// ControlMessageForceRefresh tells the client to discard its local
	// Y.Doc, recreate an empty one, and rely on the lazy-seed path
	// (TASK-1261) to re-encode items.content into ops. Sent when a
	// reconnecting client's `?since=<id>` is below MIN(item_yjs_updates.id)
	// — the rows it expected to replay have been pruned (op-log GC,
	// schema-rebuild, or PruneAndApply), and replaying from the new
	// MIN would be a corrupt patch on a stale Y.Doc state. The server
	// closes the WebSocket immediately after sending the frame; the
	// client is expected to surface a toast and reconnect with
	// `?since=0`. Per TASK-1319.
	ControlMessageForceRefresh = "force_refresh"
)

// applierFirstTimeoutVar / applierRetryTimeoutVar are vars (rather
// than consts) so test helpers can shrink them to a few hundred ms.
// Production code reads them via applierFirstTimeout / applierRetryTimeout
// helpers below, which keep the const-style call sites stable.
var (
	// applierFirstTimeoutVar is the first-attempt budget. Generous
	// so a tab that just regained focus has time to apply + ack.
	applierFirstTimeoutVar = 30 * time.Second

	// applierRetryTimeoutVar is shorter — by retry time we already
	// know the room has at least one slow / stale client; don't
	// double-pay the wait.
	applierRetryTimeoutVar = 15 * time.Second
)

func applierFirstTimeout() time.Duration { return applierFirstTimeoutVar }
func applierRetryTimeout() time.Duration { return applierRetryTimeoutVar }

// ControlMessage is the JSON envelope the server and the browser
// exchange over WebSocket TextMessage frames. Y-protocol traffic
// stays on BinaryMessage so the discriminator at the read-loop level
// is the message type itself, not a payload byte.
type ControlMessage struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id,omitempty"`

	// Markdown carries the new item content for applier_request.
	// Omitted on applier_ack.
	Markdown string `json:"markdown,omitempty"`

	// ExpiresAtMillis is the Unix-millis timestamp after which the
	// browser MUST drop a queued applier_request without applying.
	// Set by the server on every applier_request to "now +
	// applierFirstTimeout"; closes the late-apply hazard where a
	// backgrounded tab eventually wakes up and overwrites newer
	// edits with stale markdown after the server has already
	// retried with a different applier (or fallen back). Client
	// enforcement lives in TASK-1263. Omitted on applier_ack.
	ExpiresAtMillis int64 `json:"expires_at_millis,omitempty"`

	// OpLogID carries the highest item_yjs_updates.id the receiving
	// peer should treat as applied. Set on op_log_cursor frames
	// (TASK-1319). NO omitempty: a legitimate cursor of 0 (empty
	// op-log session) would otherwise serialize as
	// `{"type":"op_log_cursor"}` and the client's strict-type
	// validation would drop it as 'missing op_log_id', leaving
	// the session unanchored. Per Codex round 22 [P1] of TASK-1319.
	// applier_request / applier_ack frames don't read this field;
	// the trailing `,"op_log_id":0` they carry is ignored by the
	// client's discriminated dispatch.
	OpLogID int64 `json:"op_log_id"`
}

// pendingApplierAck tracks one in-flight designated-applier round-trip. The applier
// flow waits on resultCh for the DURABLE outcome — true = the applier's setContent
// landed (success), false = it did not (retry). The outcome is delivered exactly
// once, by whichever of the ack path (resolveApplierAck) or a concurrent restore's
// finalization (freezeAndFinalizePending) reaches it first; `delivered` guards the
// double-send. expectedConn lets the server reject acks/brackets from other peers
// (defence in depth). The bracket baselines are captured at request-send and
// refined at apply_start (BUG-2276 residual 2).
type pendingApplierAck struct {
	expectedConn *websocket.Conn
	resultCh     chan bool

	// startPersistedOpID / startFrozenDropSeq are the applier conn's durable
	// high-water + frozen-drop count at the START of the apply bracket. Seeded at
	// request-send (registerPendingAck) and refined at apply_start so the bracket
	// scopes to exactly the setContent frame.
	startPersistedOpID int64
	startFrozenDropSeq int64
	// applyStarted is set when apply_start was received. Finalization treats an
	// un-bracketed round-trip as not-persisted (safe: retry, never a false success).
	applyStarted bool
	// delivered guards resultCh against a double send (ack vs finalization race).
	delivered bool
}

// deliver sends the round-trip's durable outcome exactly once. MUST be called with
// r.pendingMu held.
func (p *pendingApplierAck) deliver(success bool) {
	if p.delivered {
		return
	}
	p.delivered = true
	select {
	case p.resultCh <- success:
	default:
	}
}

// Sentinel errors for ApplyExternalContent. Callers (the items PATCH
// handler) translate these to the appropriate fall-back action.
var (
	// ErrNoActiveRoom — no Room exists for this itemID. The caller
	// should write items.content directly.
	ErrNoActiveRoom = errors.New("collab: no active room for item")

	// ErrNoApplierAvailable — a Room exists but has no live conns
	// to nominate (e.g. mid-grace-TTL window). Caller falls back to
	// direct write.
	ErrNoApplierAvailable = errors.New("collab: no live conn available to apply")

	// ErrAllAppliersTimedOut — every attempt timed out without an
	// ack. Caller falls back to direct write and (depending on
	// preference) logs a warn so operators can see degraded sessions.
	ErrAllAppliersTimedOut = errors.New("collab: all designated appliers timed out")
)

// ApplyExternalContent routes an external content update through a
// designated applier when an active room exists. Returns nil on
// success; one of the sentinel errors above otherwise so the
// caller can decide whether to fall back to a direct write.
//
// The function blocks until the round-trip resolves OR all attempts time out — the
// PATCH handler stays open during this window and the caller sees the final outcome
// on return.
//
// A concurrent version restore (ForceRefreshRoom) is coordinated via the per-room
// restore gate + durable persistence correlation (BUG-2276 residual 2, see
// restore_coord.go): a NEW round-trip waits out an in-progress restore before
// electing; an IN-FLIGHT round-trip is finalized by the restore's freeze from
// durable op-log state (success iff its setContent frame durably landed). When a
// restore supersedes a round-trip, the election restarts once the restore RESOLVES —
// on rollback against the unfrozen room, on commit against the force-closed room. The
// outer loop caps those restarts.
func (m *RoomManager) ApplyExternalContent(itemID string, markdown string) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed()
	}
	room := m.rooms[itemID]
	m.mu.Unlock()

	if room == nil {
		return ErrNoActiveRoom
	}

	for restart := 0; restart < applierMaxRestartsAfterRestore; restart++ {
		err, superseded := m.electAndApply(room, itemID, markdown)
		if !superseded {
			return err
		}
		// A version restore superseded this election and has now resolved
		// (electAndApply already waited it out). Re-elect from scratch: a fresh
		// request_id + tried set against the post-restore room.
	}
	// Restore storm: fall back to a direct write rather than spin.
	return ErrNoApplierAvailable
}

// electAndApply runs one designated-applier election (first attempt + one retry)
// against the room, gated on the restore coordinator. It returns (err, superseded):
// superseded==true means a version restore interrupted the election and has resolved,
// so ApplyExternalContent should restart; err is meaningless in that case. When
// superseded==false, err is the final outcome (nil on success, or a sentinel).
func (m *RoomManager) electAndApply(room *Room, itemID, markdown string) (error, bool) {
	// A FRESH request_id per election: a late ack from a superseded prior election
	// (same conn, re-picked after a rollback) can't be mistaken for this one's.
	requestID := uuid.NewString()
	timeouts := []time.Duration{applierFirstTimeout(), applierRetryTimeout()}

	tried := make(map[*websocket.Conn]struct{})
	// anyWriteSucceeded tracks whether at least one applier_request
	// reached a peer over the wire. The fallback caller decides
	// whether to prune the op-log based on the error sentinel; only
	// "no applier ever received the request" makes pruning safe (no
	// peer holds an in-memory Y.Doc derived from the now-stale
	// op-log). Without this distinction a sequence of write failures
	// followed by no remaining candidates would surface as
	// ErrAllAppliersTimedOut and skip the safe prune. Per Codex
	// review round 5.
	var anyWriteSucceeded bool
	for attempt := 0; attempt < applierMaxAttempts; attempt++ {
		// Enter the restore gate FIRST so we wait out any in-progress restore
		// before electing. `waited` means a restore held the room and just
		// resolved — restart the whole election (the room's conn set changed).
		if room.enterApplierGate() {
			return nil, true
		}

		applier := room.pickApplier(tried)
		if applier == nil {
			// No more candidates left.
			if !anyWriteSucceeded {
				return ErrNoApplierAvailable, false
			}
			return ErrAllAppliersTimedOut, false
		}
		tried[applier.conn] = struct{}{}

		resultCh, registerErr := room.registerPendingAck(requestID, applier)
		if registerErr != nil {
			// Race: room closed between pickApplier and registration.
			return registerErr, false
		}

		// Send the applier_request as a TextMessage. y-protocol
		// traffic uses BinaryMessage exclusively, so the browser's
		// ws.onmessage handler can branch on event.data type
		// (string vs ArrayBuffer) without an extra prefix byte.
		//
		// ExpiresAt is computed per attempt — the SAME request_id
		// might be sent on a retry (we cancel + re-register so a
		// late ack from the previous attempt isn't accidentally
		// accepted, but the request_id stays stable so log lines
		// thread together).
		msg := ControlMessage{
			Type:            ControlMessageApplierRequest,
			RequestID:       requestID,
			Markdown:        markdown,
			ExpiresAtMillis: time.Now().Add(timeouts[attempt]).UnixMilli(),
		}
		payload, err := json.Marshal(msg)
		if err != nil {
			room.cancelPendingAck(requestID)
			return fmt.Errorf("marshal applier_request: %w", err), false
		}
		// Bounded write (BUG-2276 residual 2, P1a): a dead/slow peer wedged in a
		// deadline-free writeLoop write holds writeMu, which would otherwise block
		// this send indefinitely with a pending-ack entry outstanding. An independent
		// close-timer closes the conn if the write can't complete, forcing the wedged
		// write to error and releasing writeMu — mirroring forceRefreshAll.
		if werr := writeApplierRequest(applier, payload); werr != nil {
			// Conn write failed (slow / dead). Drop the pending
			// ack, evict the broken conn from the room (otherwise
			// it would still count as a "live peer" against
			// PruneAndApply's len(r.conns) > 0 check, blocking the
			// safe op-log prune), force-close the underlying WS
			// so the conn's readLoop wakes up cleanly, and try
			// the next applier. Per Codex review round 6.
			room.cancelPendingAck(requestID)
			room.removeConn(applier)
			_ = applier.conn.Close()
			slog.Warn("collab: applier_request write failed; trying next",
				"item_id", itemID,
				"client_id", applier.id,
				"error", werr,
			)
			continue
		}
		anyWriteSucceeded = true

		// Wait for the DURABLE outcome OR timeout.
		select {
		case success := <-resultCh:
			room.cancelPendingAck(requestID)
			if success {
				// The applier's setContent durably landed (no frame in its apply
				// bracket was frozen-dropped). All peers are on the new state.
				return nil, false
			}
			// Not persisted: a version restore froze this apply, so setContent did
			// NOT land. This is a DURABLE determination (see restore_coord.go), not a
			// timing guess — so we never re-apply over a landed edit. Wait for the
			// restore to resolve, then restart the election: re-apply on rollback
			// (unfrozen room), fall back on commit (force-closed room).
			room.waitRestoreResolved() // bounded to the restore-tx duration
			slog.Info("collab: applier round-trip superseded by version restore; will re-elect",
				"item_id", itemID,
				"client_id", applier.id,
			)
			return nil, true
		case <-time.After(timeouts[attempt]):
			room.cancelPendingAck(requestID)
			slog.Warn("collab: applier_request timed out; will retry next applier",
				"item_id", itemID,
				"client_id", applier.id,
				"attempt", attempt+1,
			)
			// Fall through to next attempt. Any late-arriving ack
			// for this request_id from the timed-out conn is
			// rejected because we just cancelled the entry; the
			// browser is also expected to drop the stale request
			// via its expires_at_millis check (TASK-1263).
		}
	}

	if !anyWriteSucceeded {
		// applierMaxAttempts exhausted without ever putting bytes on
		// the wire. Same recovery profile as no-applier-found.
		return ErrNoApplierAvailable, false
	}
	return ErrAllAppliersTimedOut, false
}

// applierMaxRestartsAfterRestore caps how many times ApplyExternalContent re-elects
// after being superseded by a restore, so a pathological back-to-back restore storm
// can't spin the election forever. On exhaustion the caller falls back to a direct
// write (ErrNoApplierAvailable) — safe graceful degradation.
const applierMaxRestartsAfterRestore = 5

// applierWriteDeadlineVar bounds the applier_request write so a dead/slow peer
// holding writeMu can't wedge the round-trip (BUG-2276 residual 2, P1a). Sized like
// closeFrameDeadline's budget — generous enough that a healthy peer always completes.
// A var so tests can shrink it.
var applierWriteDeadlineVar = 2 * time.Second

func applierWriteDeadline() time.Duration { return applierWriteDeadlineVar }

// writeApplierRequest sends the applier_request with a bounded write. An independent
// timer closes the conn if the write can't complete within applierWriteDeadline —
// gorilla's Close is concurrency-safe with an in-flight WriteMessage, so it forces a
// wedged writeLoop write to error, releasing writeMu and unblocking us. Mirrors the
// per-conn timer pattern in forceRefreshAll (BUG-2264 P1).
func writeApplierRequest(rc *roomConn, payload []byte) error {
	d := applierWriteDeadline()
	deadline := time.Now().Add(d)
	t := time.AfterFunc(d, func() { _ = rc.conn.Close() })
	err := rc.writeMessageWithDeadline(websocket.TextMessage, payload, deadline)
	t.Stop()
	return err
}

// ErrManagerClosed exposes the manager's internal closed-flag error
// to callers via a public sentinel they can wrap. The internal
// errManagerClosed (in manager.go) is unexported so tests can't
// errors.Is against it directly.
func ErrManagerClosed() error { return errManagerClosed }

// pickApplier returns the longest-connected roomConn that hasn't
// already been tried, or nil if there are no candidates left. The
// stable ordering (sort by connectedAt asc) ensures successive
// attempts hit increasingly-younger clients without skipping any.
func (r *Room) pickApplier(tried map[*websocket.Conn]struct{}) *roomConn {
	r.mu.Lock()
	candidates := make([]*roomConn, 0, len(r.conns))
	for _, rc := range r.conns {
		if _, skip := tried[rc.conn]; skip {
			continue
		}
		// Read-only participants (workspace viewers / view-only guests,
		// TASK-265) are never eligible appliers: the applier applies the
		// external markdown by producing Y.Doc sync frames, which the
		// read-only gate drops, so an elected viewer would ack a
		// no-op and make ApplyExternalContent falsely report success —
		// the PATCH handler would then skip its direct-write fallback
		// and the edit would be silently lost. Skip them so election
		// only ever lands on a writer (or returns nil → the caller's
		// direct-write fallback fires).
		//
		// A frozen conn (mid version-restore) is excluded defensively: the enter
		// gate already blocks election while a restore holds the room, but the
		// success/commit path leaves conns frozen until they are force-closed, so a
		// commit-case re-election must skip them → returns nil → the caller's
		// direct-write fallback. (BUG-2276 residual 2.)
		if !rc.canWrite.Load() || rc.frozen.Load() {
			continue
		}
		candidates = append(candidates, rc)
	}
	r.mu.Unlock()

	if len(candidates) == 0 {
		return nil
	}
	// Stable: longest-connected first; tiebreak on conn id (which
	// is monotonic within the process).
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].connectedAt.Equal(candidates[j].connectedAt) {
			return candidates[i].connectedAt.Before(candidates[j].connectedAt)
		}
		return candidates[i].id < candidates[j].id
	})
	return candidates[0]
}

// registerPendingAck creates an entry in the room's pending-acks map keyed on
// requestID, expecting acks/brackets from `applier`. It seeds the bracket baselines
// from the applier conn's current durable high-water + frozen-drop count (refined
// later by apply_start). Returns the result channel the caller selects on (true =
// the applier's content durably landed, false = it did not), or an error if the room
// is no longer accepting requests.
func (r *Room) registerPendingAck(requestID string, applier *roomConn) (<-chan bool, error) {
	r.mu.Lock()
	closing := r.closing
	r.mu.Unlock()
	if closing {
		return nil, errRoomClosing
	}

	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	if _, exists := r.pendingAcks[requestID]; exists {
		// Caller bug — request_ids are UUIDs in production; collision
		// is effectively impossible.
		return nil, fmt.Errorf("collab: duplicate request_id %s", requestID)
	}
	ch := make(chan bool, 1)
	r.pendingAcks[requestID] = &pendingApplierAck{
		expectedConn:       applier.conn,
		resultCh:           ch,
		startPersistedOpID: applier.lastPersistedOpID.Load(),
		startFrozenDropSeq: applier.frozenDropSeq.Load(),
	}
	return ch, nil
}

// cancelPendingAck removes a request from the pending-acks map. Used
// after a timeout / write failure / result delivery to free the slot.
func (r *Room) cancelPendingAck(requestID string) {
	r.pendingMu.Lock()
	delete(r.pendingAcks, requestID)
	r.pendingMu.Unlock()
}

// startApplierApply opens the durable persistence bracket for requestID: it refines
// the bracket baseline to the applier conn's high-water + frozen-drop count RIGHT
// NOW, at apply_start. Since the client sends apply_start immediately before
// setContent (synchronously), the bracket that follows contains exactly the
// setContent frame — so a subsequent lastPersistedOpID advance / frozenDropSeq
// advance is attributable to the applier, not to concurrent typing. Called by
// readLoop; only the expected conn may open the bracket. (BUG-2276 residual 2.)
func (r *Room) startApplierApply(requestID string, rc *roomConn) {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	pending, ok := r.pendingAcks[requestID]
	if !ok || pending.expectedConn != rc.conn {
		return
	}
	pending.startPersistedOpID = rc.lastPersistedOpID.Load()
	pending.startFrozenDropSeq = rc.frozenDropSeq.Load()
	pending.applyStarted = true
}

// resolveApplierAck resolves the round-trip on receipt of the applier_ack (the
// bracket end), from durable state: SUCCESS iff no frame in the apply bracket was
// frozen-dropped (frozenDropSeq unchanged). A no-op-diff setContent, which persists
// nothing, also resolves to success (nothing was dropped → the doc already reflects
// the content). Delivers the outcome once; a concurrent restore finalization may
// have delivered first (idempotent). Only the expected conn may resolve. Called by
// readLoop on an applier_ack control message. (BUG-2276 residual 2.)
func (r *Room) resolveApplierAck(requestID string, rc *roomConn) {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	pending, ok := r.pendingAcks[requestID]
	if !ok {
		return // unknown / already-cancelled request_id
	}
	if pending.expectedConn != rc.conn {
		slog.Warn("collab: applier_ack from unexpected conn; ignoring",
			"request_id", requestID,
		)
		return
	}
	dropped := rc.frozenDropSeq.Load() > pending.startFrozenDropSeq
	pending.deliver(!dropped)
}

// freezeAndFinalizePending is the atomic heart of the version-restore ↔ applier
// coordination (BUG-2276 residual 2). Called by ForceRefreshRoom with appendMu held,
// it (1) marks every conn frozen and (2) FINALIZES every in-flight applier
// round-trip from durable op-log state — success iff the applier conn's high-water
// advanced past the bracket baseline (its setContent frame durably landed BEFORE
// this freeze). Because the freeze and the high-water read share the appendMu
// critical section, a frame still in flight is blocked on appendMu and will be
// dropped once the freeze is visible — so a NOT-advanced entry is a durable "did not
// land", never a race. An un-bracketed round-trip (apply_start not seen) is treated
// as not-persisted (safe: it re-elects; never a false success).
//
// Lock order: caller holds appendMu; we take room.mu then release it before
// pendingMu, so no room.mu↔pendingMu nesting. No socket I/O under any lock.
func (r *Room) freezeAndFinalizePending() {
	r.mu.Lock()
	conns := make(map[*websocket.Conn]*roomConn, len(r.conns))
	for c, rc := range r.conns {
		rc.frozen.Store(true)
		conns[c] = rc
	}
	r.mu.Unlock()

	r.pendingMu.Lock()
	for _, pending := range r.pendingAcks {
		rc := conns[pending.expectedConn]
		persisted := false
		if rc != nil && pending.applyStarted {
			persisted = rc.lastPersistedOpID.Load() > pending.startPersistedOpID
		}
		pending.deliver(persisted)
	}
	r.pendingMu.Unlock()
}

// applierMutexLockOrder is a documentation marker — the mutex order
// for the designated-applier paths is:
//
//	r.mu (room state) > r.pendingMu (ack tracking)
//
// Acquire in that order; release in reverse. registerPendingAck, startApplierApply,
// resolveApplierAck and freezeAndFinalizePending hold ONLY pendingMu (no r.mu
// reentry while pendingMu is held), so no inversion is possible. The closing/conns
// checks that need r.mu run BEFORE the pendingMu acquisition.
var _ = sync.Mutex{}
