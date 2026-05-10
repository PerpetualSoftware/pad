package collab

import (
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/gorilla/websocket"
)

// Y-protocol top-level message types. The first byte of every WebSocket
// message a Yjs client sends discriminates these. The dumb-relay server
// only needs the coarse split: sync bytes are persisted to the op-log
// and broadcast; awareness bytes are broadcast only (presence is
// ephemeral and meaningless after the originating peer disconnects).
//
// Subtypes within a sync message (state-vector negotiation vs.
// document update) are NOT distinguished server-side — the spike
// verified that persisting whole sync frames yields a correct replay
// when fed back to a fresh Yjs peer in order. This is the y-websocket
// reference protocol's "dumb relay" mode.
const (
	yMessageSync      = 0
	yMessageAwareness = 1
)

// DefaultGraceTTL is how long a Room lingers in memory after its
// last subscriber disconnects. Within this window a reconnect
// (transient network blip, mobile-tab background suspension)
// cancels the grace timer and the Room continues. After the window
// the Room is removed from the manager and any cached state is
// reclaimed.
//
// 60 seconds tracks the value the Plan body locks in (PLAN-1248)
// and roughly matches a generous mobile-foreground-restore window.
// Tests can override per-manager via NewRoomManagerWithConfig.
const DefaultGraceTTL = 60 * time.Second

// errRoomClosing is returned by Room.addConn when the grace timer
// fired between the manager's getOrCreate and addConn calls. The
// caller (RoomManager.Join) re-fetches via the manager — the next
// getOrCreate will see the room missing and mint a fresh one.
var errRoomClosing = errors.New("collab: room is closing")

// roomConn pairs a single WebSocket connection with the bookkeeping
// the Room needs around it: a unique server-assigned id (so a peer's
// own ops aren't echoed back to itself), the OpBus subscription
// channel that feeds outbound writes, a write mutex (gorilla's
// rule — at most one writer goroutine at a time per conn), and the
// connect timestamp used by the designated-applier election to pick
// the longest-connected peer.
type roomConn struct {
	id          uint64
	conn        *websocket.Conn
	bus         chan OpEvent
	writeMu     sync.Mutex
	connectedAt time.Time
	// replayDone is set after replayTo + the initial post-replay
	// op_log_cursor frame have been written. writeLoop suppresses
	// op_log_cursor frames before this flips so live ops broadcast
	// during the replay window don't advance the client's cursor
	// past replay rows it hasn't yet seen — disconnecting mid-replay
	// after a single live cursor would otherwise leave the client's
	// resume cursor pointing past unreplayed rows. Per Codex round
	// 5 [P1] of TASK-1319.
	replayDone atomic.Bool
	// maxLiveOpLogIDDuringReplay records the highest persisted op-log
	// id whose binary frame this conn's writeLoop fanned out before
	// `replayDone` flipped true. The post-replay initial cursor uses
	// max(highestReplayed, since, this) so a live op that was applied
	// to the client's Y.Doc during the replay window doesn't leave
	// the client cursor pointing below the highest applied op — that
	// mismatch would trip the client's `cursor=0 + remoteSyncApplied`
	// force_refresh path on empty-replay sessions and discard
	// buffered pre-anchor edits. Per Codex round 21 [P1] of TASK-1319.
	maxLiveOpLogIDDuringReplay atomic.Int64
}

// writeMessage is a tiny helper that holds writeMu while writing one
// frame. All writes (replay during addConn, writeLoop fan-out) go
// through here so we never hit "concurrent write to websocket
// connection" panics.
func (rc *roomConn) writeMessage(messageType int, data []byte) error {
	rc.writeMu.Lock()
	defer rc.writeMu.Unlock()
	return rc.conn.WriteMessage(messageType, data)
}

// Room is the per-item collab fan-out point. One Room per `itemID`
// at a time; created lazily by the RoomManager on first Join and
// reclaimed `graceTTL` after the last subscriber leaves.
type Room struct {
	itemID        string
	store         opLogStore
	bus           OpBus
	schemaVersion string
	graceTTL      time.Duration
	onIdle        func(string) // RoomManager.markRoomGone

	mu         sync.Mutex
	conns      map[*websocket.Conn]*roomConn
	graceTimer *time.Timer
	closing    bool // set after the grace timer reclaims this Room

	// appendMu serialises the persist+publish path for sync frames.
	// Each peer's readLoop runs in its own goroutine — without this,
	// concurrent AppendYjsUpdate calls would violate TASK-1252's
	// "single writer per item" contract and risk a Postgres
	// allocation-vs-commit-order cursor gap. Held only across the
	// AppendYjsUpdate + bus.Publish sequence, so it does NOT
	// serialise reads, awareness frames, or other rooms.
	appendMu sync.Mutex

	// pendingMu + pendingAcks track in-flight designated-applier
	// requests (TASK-1257). Each entry is a request_id → expected
	// applier conn + ack channel. Server-side PATCH handlers create
	// an entry, send the applier_request control message to the
	// chosen conn, and Wait on the channel; the readLoop on that
	// conn receives an applier_ack JSON frame and signals the
	// channel. The expectedConn check prevents an unrelated peer
	// from spoofing acks for someone else's request.
	pendingMu   sync.Mutex
	pendingAcks map[string]*pendingApplierAck
}

// opLogStore is the store surface a Room needs. Pulling it into a
// narrow interface lets manager_test stub op-log behaviour without
// dragging in the entire *store.Store API surface.
type opLogStore interface {
	AppendYjsUpdate(itemID string, data []byte, schemaVersion string) (int64, error)
	LoadYjsUpdatesSince(itemID string, sinceID int64) ([]models.YjsUpdate, error)
	// LatestYjsUpdateSchemaVersion + PruneYjsUpdatesBefore power the
	// schema-mismatch rebuild flow (TASK-1268). The room manager
	// checks the most recent op-log row's stamp on Join and prunes
	// the entire item's op-log when the persisted version no longer
	// matches the server's current SCHEMA_VERSION. The latest row's
	// id is returned alongside the version so the rebuild can detect
	// when it's about to drop unflushed edits and log a warning.
	LatestYjsUpdateSchemaVersion(itemID string) (string, int64, bool, error)
	PruneYjsUpdatesBefore(itemID string, before time.Time) (int64, error)
	// GetItemContentFlushedOpLogID returns the per-item flush
	// watermark (TASK-1309). Returns (0, false) for items with NULL
	// watermark or no row.
	GetItemContentFlushedOpLogID(itemID string) (int64, bool, error)
	// ListDormantOpLogItemsBefore + PruneItemOpLogIfDormantBefore
	// power the periodic prune sweep (TASK-1309). Selects items
	// whose ENTIRE op-log is older than cutoff AND whose
	// items.content has captured every op-log row (id watermark);
	// conditional DELETE re-checks dormancy atomically. See
	// yjs_updates.go for why prefix-pruning is unsafe (Yjs causal
	// references).
	ListDormantOpLogItemsBefore(before time.Time) ([]string, error)
	PruneItemOpLogIfDormantBefore(itemID string, before time.Time) (int64, error)
	// MinOpLogID powers the resume-cursor / force-refresh check
	// (TASK-1319): when a reconnecting client announces `?since=<id>`
	// and that id is below MIN, rows it expected to replay have
	// been pruned and the server sends ControlMessageForceRefresh.
	// MaxOpLogID is intentionally NOT on this interface — earlier
	// versions used it to populate the initial cursor when replay
	// produced no rows, but Codex round 6 [P1] caught the race
	// between MaxOpLogID and an in-flight broadcast: the cursor
	// could advertise an id whose binary frame hadn't been
	// delivered through this conn's writeLoop yet. The initial
	// cursor now strictly uses `max(highestReplayed, since)`.
	MinOpLogID(itemID string) (int64, bool, error)
}

// addConn registers a freshly-built roomConn with the room. Cancels
// any pending grace timer (a reconnect within the window keeps the
// Room alive) and returns errRoomClosing if the grace timer already
// reclaimed the Room — the caller must restart through the manager
// to land in a fresh Room.
func (r *Room) addConn(rc *roomConn) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closing {
		return errRoomClosing
	}
	if r.graceTimer != nil {
		r.graceTimer.Stop()
		r.graceTimer = nil
	}
	r.conns[rc.conn] = rc
	return nil
}

// removeConn unsubscribes the connection from the bus and, if it was
// the last subscriber, schedules a graceTTL grace timer. The caller
// is responsible for closing the WebSocket itself; we only manage the
// in-room bookkeeping.
func (r *Room) removeConn(rc *roomConn) {
	r.bus.Unsubscribe(rc.bus)

	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.conns, rc.conn)
	if len(r.conns) == 0 && r.graceTimer == nil && !r.closing {
		r.graceTimer = time.AfterFunc(r.graceTTL, r.onGraceExpired)
	}
}

// onGraceExpired runs after graceTTL has passed without a fresh
// connection. If the room is still empty it's marked closing and
// reclaimed by the manager. If a connection arrived in the meantime
// (race: timer fired but addConn already cleared graceTimer), the
// stale-timer call no-ops on len(conns)>0 and clears its own slot.
func (r *Room) onGraceExpired() {
	r.mu.Lock()
	if len(r.conns) > 0 {
		// A fresh connection landed between scheduling and firing.
		// addConn already set graceTimer = nil; tolerate the residue.
		r.graceTimer = nil
		r.mu.Unlock()
		return
	}
	r.closing = true
	r.mu.Unlock()

	r.onIdle(r.itemID)
}

// replayTo sends every persisted op-log update for this room with id
// strictly greater than `since` to the given connection in order. Each
// row goes out as its own binary WebSocket frame so a Yjs peer applies
// them via the same y-protocol path it uses for live updates. Stops on
// the first WS write error (the connection is doomed); the caller's
// read loop will surface the same error and tear the connection down
// cleanly.
//
// `since == 0` is the fresh-client path (replay everything). The
// caller is responsible for the prerequisite check that `since`
// isn't below the current MIN(id) — an out-of-range cursor means
// rows the client expected have been pruned and the response should
// be a force_refresh, not a partial replay.
//
// Returns the highest replayed id (or 0 when no rows landed) so the
// caller can emit a follow-on op_log_cursor frame anchoring the
// client's resume point. Per TASK-1319.
func (r *Room) replayTo(rc *roomConn, since int64) (int64, error) {
	updates, err := r.store.LoadYjsUpdatesSince(r.itemID, since)
	if err != nil {
		return 0, err
	}
	var highest int64
	for _, u := range updates {
		if len(u.UpdateData) == 0 {
			continue
		}
		if werr := rc.writeMessage(websocket.BinaryMessage, u.UpdateData); werr != nil {
			return highest, werr
		}
		if u.ID > highest {
			highest = u.ID
		}
	}
	return highest, nil
}

// sendOpLogCursor emits an op_log_cursor JSON control frame to the
// given conn. Best-effort: any write error is returned to the caller
// (typically discarded — a doomed conn will surface the same error
// elsewhere and the read loop will tear down). Per TASK-1319.
func (r *Room) sendOpLogCursor(rc *roomConn, opLogID int64) error {
	payload, err := json.Marshal(ControlMessage{
		Type:    ControlMessageOpLogCursor,
		OpLogID: opLogID,
	})
	if err != nil {
		// json.Marshal of a pure-scalar struct cannot fail in
		// practice; log defensively rather than swallow.
		slog.Warn("collab: marshal op_log_cursor failed",
			"item_id", r.itemID,
			"client_id", rc.id,
			"error", err,
		)
		return err
	}
	return rc.writeMessage(websocket.TextMessage, payload)
}

// sendForceRefreshFrame writes a force_refresh control message on the
// conn. Used by the resume-cursor protocol when the client's
// announced `?since=<id>` is below MIN(item_yjs_updates.id) — the
// only safe response is to tell the client to discard local Y.Doc
// state and lazy-seed from items.content. The caller closes the
// conn after this returns. Per TASK-1319.
func sendForceRefreshFrame(conn *websocket.Conn) error {
	payload, err := json.Marshal(ControlMessage{
		Type: ControlMessageForceRefresh,
	})
	if err != nil {
		return err
	}
	// Set a short write deadline: a doomed conn shouldn't block the
	// upgrade-time check indefinitely.
	_ = conn.SetWriteDeadline(time.Now().Add(closeFrameDeadline))
	return conn.WriteMessage(websocket.TextMessage, payload)
}

// readLoop is the inbound side: pull frames off the WebSocket, route
// sync frames through the op-log + broadcast, awareness frames to
// broadcast only. Returns when the WS read returns an error
// (close frame or transport failure). The caller then runs
// removeConn and waits for writeLoop to exit.
func (r *Room) readLoop(rc *roomConn) error {
	for {
		msgType, data, err := rc.conn.ReadMessage()
		if err != nil {
			return err
		}

		// TextMessage frames carry JSON control messages (currently
		// just designated-applier acks; future entries can extend
		// the same envelope without disturbing the y-protocol path).
		if msgType == websocket.TextMessage {
			r.handleControlMessage(rc, data)
			continue
		}

		// Yjs sends only binary frames. Skip anything else (control
		// frames are already handled above) defensively so a bad
		// client can't break the loop with a malformed packet.
		if msgType != websocket.BinaryMessage || len(data) == 0 {
			continue
		}

		switch data[0] {
		case yMessageSync:
			// Hold appendMu across the persist+publish sequence so we
			// uphold TASK-1252's single-writer-per-item contract. The
			// dumb-relay design intends one writer per Room, but each
			// peer has its own readLoop — without serialisation here,
			// two peers' sync frames would race AppendYjsUpdate and
			// could surface a Postgres cursor gap (allocation order ≠
			// commit order). awareness frames and OTHER rooms are
			// unaffected by this lock.
			r.appendMu.Lock()
			// Persist before broadcast so a server crash between
			// persist and broadcast loses at most a live keystroke
			// that the originating peer will replay on reconnect
			// anyway.
			persistedID, err := r.store.AppendYjsUpdate(r.itemID, data, r.schemaVersion)
			if err != nil {
				slog.Error("collab: append op-log",
					"item_id", r.itemID,
					"client_id", rc.id,
					"error", err,
				)
				// Continue: broadcast keeps the live mesh consistent
				// even when persistence is blipping. persistedID == 0
				// → no cursor frame is emitted by writeLoop for this
				// event (we'd be advertising a fictional id).
			}
			r.bus.Publish(OpEvent{
				ItemID:   r.itemID,
				ClientID: rc.id,
				Type:     OpTypeSync,
				Data:     data,
				OpLogID:  persistedID,
			})
			r.appendMu.Unlock()

			// Notify the originator of its new cursor. writeLoop
			// skips self events (no echo of the binary frame) so
			// without this the originating peer would never see the
			// id its own ops were assigned. Best-effort: a write
			// error here is the same signal the readLoop would hit
			// on its next ReadMessage and tear down cleanly.
			if persistedID > 0 {
				if cerr := r.sendOpLogCursor(rc, persistedID); cerr != nil {
					// readLoop will hit the same conn error on its
					// next iteration; just record at debug.
					slog.Debug("collab: send op_log_cursor to originator failed",
						"item_id", r.itemID,
						"client_id", rc.id,
						"error", cerr,
					)
				}
			}

		case yMessageAwareness:
			// Awareness is presence — ephemeral. Never persisted.
			r.bus.Publish(OpEvent{
				ItemID:   r.itemID,
				ClientID: rc.id,
				Type:     OpTypeAwareness,
				Data:     data,
			})

		default:
			// Unknown y-protocol message types (custom extensions,
			// future revisions). Silently drop — logging at debug
			// would spam the operator under any client misbehaviour.
		}
	}
}

// writeLoop drains the bus subscription channel and writes every
// non-self event to the connection. Self-events (those whose
// ClientID matches our own) are skipped because the originator
// already has the local Y.Doc state — echoing would be a no-op for
// them but doubles the wire traffic.
//
// Exits when the bus channel closes (Unsubscribe) or a write fails.
// On write failure the connection is closed so the read loop on the
// other goroutine surfaces an error and tears down cleanly.
func (r *Room) writeLoop(rc *roomConn) {
	for ev := range rc.bus {
		if ev.ClientID == rc.id {
			continue
		}
		if err := rc.writeMessage(websocket.BinaryMessage, ev.Data); err != nil {
			// Force the read side to wake up and return.
			_ = rc.conn.Close()
			return
		}
		// Track the receiver's applied cursor when a sync frame
		// has a persisted id. Awareness frames and frames whose
		// AppendYjsUpdate failed (OpLogID == 0) do NOT advance the
		// receiver's cursor — neither was persisted. Per TASK-1319.
		//
		// During replay (replayDone == false) we suppress cursor
		// frames so a live op broadcast mid-replay can't bump the
		// client's persisted cursor past replay rows it hasn't
		// received yet. The post-replay cursor frame in runConn
		// flips replayDone, after which live ops resume cursor
		// advancement. Per Codex round 5 [P1].
		//
		// The suppressed id is recorded so runConn can fold the
		// highest live id into the post-replay cursor. Otherwise
		// a live op that landed binary-applied client-side during
		// the replay window would leave the client's cursor below
		// its applied state. Per Codex round 21 [P1].
		if ev.Type == OpTypeSync && ev.OpLogID > 0 {
			if rc.replayDone.Load() {
				if err := r.sendOpLogCursor(rc, ev.OpLogID); err != nil {
					_ = rc.conn.Close()
					return
				}
			} else {
				for {
					prev := rc.maxLiveOpLogIDDuringReplay.Load()
					if ev.OpLogID <= prev {
						break
					}
					if rc.maxLiveOpLogIDDuringReplay.CompareAndSwap(prev, ev.OpLogID) {
						break
					}
				}
			}
		}
	}
}

// handleControlMessage dispatches a TextMessage frame received from
// a peer. Today the only valid type is applier_ack (TASK-1257); any
// other type is dropped silently so a future client extension can
// add new control types without older servers blowing up.
func (r *Room) handleControlMessage(rc *roomConn, data []byte) {
	var ctl ControlMessage
	if err := json.Unmarshal(data, &ctl); err != nil {
		// Malformed JSON — drop. We don't log at warn here because
		// a bad client could otherwise spam logs by sending
		// arbitrary text frames.
		return
	}
	switch ctl.Type {
	case ControlMessageApplierAck:
		if ctl.RequestID == "" {
			return
		}
		r.routeApplierAck(ctl.RequestID, rc.conn)
	default:
		// Unknown control type — drop.
	}
}

// closeAll closes every active connection on this room and waits for
// the readers to drain. Used by RoomManager.Close on server shutdown.
// Holds r.mu only while collecting the conn set; the actual Close
// calls happen outside the lock so a slow OS-level close doesn't
// block addConn / removeConn on parallel rooms.
func (r *Room) closeAll() {
	r.mu.Lock()
	if r.graceTimer != nil {
		r.graceTimer.Stop()
		r.graceTimer = nil
	}
	r.closing = true
	conns := make([]*websocket.Conn, 0, len(r.conns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
}

// connIDCounter is package-scoped so multiple RoomManagers in the
// same process never hand out colliding ids. Atomic Add returns a
// fresh value per call; we never reset.
var connIDCounter atomic.Uint64

func nextConnID() uint64 {
	return connIDCounter.Add(1)
}
