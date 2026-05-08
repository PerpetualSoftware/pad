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
}

// pendingApplierAck pairs the channel a PATCH handler is waiting on
// with the conn the ack is expected from. Storing the expected conn
// lets the readLoop reject acks from other peers in the same room
// (defence in depth — a hostile peer shouldn't be able to forge an
// ack on someone else's request).
type pendingApplierAck struct {
	expectedConn *websocket.Conn
	ch           chan struct{}
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
// successful ack; one of the sentinel errors above otherwise so the
// caller can decide whether to fall back to a direct write.
//
// The function blocks until ack OR all attempts time out — the PATCH
// handler stays open during this window and the caller sees the
// final outcome on return.
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
		applier := room.pickApplier(tried)
		if applier == nil {
			// No more candidates left.
			if !anyWriteSucceeded {
				return ErrNoApplierAvailable
			}
			return ErrAllAppliersTimedOut
		}
		tried[applier.conn] = struct{}{}

		ackCh, registerErr := room.registerPendingAck(requestID, applier.conn)
		if registerErr != nil {
			// Race: room closed between pickApplier and registration.
			return registerErr
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
			return fmt.Errorf("marshal applier_request: %w", err)
		}
		if err := applier.writeMessage(websocket.TextMessage, payload); err != nil {
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
				"error", err,
			)
			continue
		}
		anyWriteSucceeded = true

		// Wait for the ack OR timeout.
		select {
		case <-ackCh:
			// Success: the applier propagated Y.Doc updates via the
			// regular sync path before sending the ack, so all peers
			// are now on the new state. Drop the pending-ack entry
			// so the room's pendingAcks map doesn't grow unbounded
			// across the lifetime of the room.
			room.cancelPendingAck(requestID)
			return nil
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
		return ErrNoApplierAvailable
	}
	return ErrAllAppliersTimedOut
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

// registerPendingAck creates an entry in the room's pending-acks map
// keyed on requestID, with the expected ack source set to
// expectedConn. Returns the channel the caller should select on, or
// an error if the room is no longer accepting requests.
func (r *Room) registerPendingAck(requestID string, expectedConn *websocket.Conn) (<-chan struct{}, error) {
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
	ch := make(chan struct{}, 1)
	r.pendingAcks[requestID] = &pendingApplierAck{
		expectedConn: expectedConn,
		ch:           ch,
	}
	return ch, nil
}

// cancelPendingAck removes a request from the pending-acks map. Used
// after a timeout / write failure to free the slot before retrying.
func (r *Room) cancelPendingAck(requestID string) {
	r.pendingMu.Lock()
	delete(r.pendingAcks, requestID)
	r.pendingMu.Unlock()
}

// routeApplierAck signals the channel waiting on requestID, but
// only if the ack came from the expected conn (defence in depth).
// Called by readLoop on receipt of an applier_ack control message.
func (r *Room) routeApplierAck(requestID string, fromConn *websocket.Conn) {
	r.pendingMu.Lock()
	pending, ok := r.pendingAcks[requestID]
	r.pendingMu.Unlock()
	if !ok {
		return // unknown / already-cancelled request_id
	}
	if pending.expectedConn != fromConn {
		slog.Warn("collab: applier_ack from unexpected conn; ignoring",
			"request_id", requestID,
		)
		return
	}
	select {
	case pending.ch <- struct{}{}:
	default:
		// Already signalled; this is a duplicate ack — common when
		// the applier retries after a transient send failure.
	}
}

// applierMutexLockOrder is a documentation marker — the mutex order
// for the designated-applier paths is:
//
//	r.mu (room state) > r.pendingMu (ack tracking)
//
// Acquire in that order; release in reverse. registerPendingAck and
// routeApplierAck hold ONLY pendingMu (no r.mu reentry), so no
// inversion is possible there. The closing/conns checks that need
// r.mu run BEFORE the pendingMu acquisition.
var _ = sync.Mutex{}
