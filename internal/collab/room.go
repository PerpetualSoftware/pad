package collab

import (
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
// channel that feeds outbound writes, and a write mutex (gorilla's
// rule — at most one writer goroutine at a time per conn).
type roomConn struct {
	id      uint64
	conn    *websocket.Conn
	bus     chan OpEvent
	writeMu sync.Mutex
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
}

// opLogStore is the store surface a Room needs. Pulling it into a
// narrow interface lets manager_test stub op-log behaviour without
// dragging in the entire *store.Store API surface.
type opLogStore interface {
	AppendYjsUpdate(itemID string, data []byte, schemaVersion string) (int64, error)
	LoadYjsUpdatesSince(itemID string, sinceID int64) ([]models.YjsUpdate, error)
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

// replayTo sends every persisted op-log update for this room to the
// given connection in order. Each row goes out as its own binary
// WebSocket frame so a Yjs peer applies them via the same y-protocol
// path it uses for live updates. Stops on the first WS write error
// (the connection is doomed); the caller's read loop will surface
// the same error and tear the connection down cleanly.
func (r *Room) replayTo(rc *roomConn) error {
	updates, err := r.store.LoadYjsUpdatesSince(r.itemID, 0)
	if err != nil {
		return err
	}
	for _, u := range updates {
		if len(u.UpdateData) == 0 {
			continue
		}
		if werr := rc.writeMessage(websocket.BinaryMessage, u.UpdateData); werr != nil {
			return werr
		}
	}
	return nil
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
		// Yjs sends only binary frames. Skip text/control frames defensively
		// so a bad client cannot break the loop with a malformed packet.
		if msgType != websocket.BinaryMessage || len(data) == 0 {
			continue
		}

		switch data[0] {
		case yMessageSync:
			// Persist before broadcast so a server crash between persist
			// and broadcast loses at most a live keystroke that the
			// originating peer will replay on reconnect anyway.
			if _, err := r.store.AppendYjsUpdate(r.itemID, data, r.schemaVersion); err != nil {
				slog.Error("collab: append op-log",
					"item_id", r.itemID,
					"client_id", rc.id,
					"error", err,
				)
				// Continue: broadcast keeps the live mesh consistent
				// even when persistence is blipping.
			}
			r.bus.Publish(OpEvent{
				ItemID:   r.itemID,
				ClientID: rc.id,
				Type:     OpTypeSync,
				Data:     data,
			})

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
	}
}

// peerCount is a test hook — production callers go through the bus's
// SubscriberCount. Holds r.mu so a concurrent removeConn doesn't
// produce a torn read.
func (r *Room) peerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
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
