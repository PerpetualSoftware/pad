package collab

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// DefaultSchemaVersion is the schema-version stamp used by all rooms
// today. TASK-1268 will add a real client-driven version with a
// snapshot-and-rebuild flow on mismatch; for now every persisted
// op-log row carries the same value, which is exactly what
// LoadYjsUpdatesSince expects.
const DefaultSchemaVersion = "1"

// errTooManyJoinRetries surfaces when RoomManager.Join lost the
// addConn-vs-grace-expiry race more times than feels like a real
// race. In practice this should never trigger — the race window is
// microseconds — but it caps the retry loop so a misbehaving room
// can't deadlock a Join indefinitely.
var errTooManyJoinRetries = errors.New("collab: too many room-close races; aborting Join")

// RoomManagerConfig collects optional knobs for NewRoomManagerWithConfig.
// Production callers should use NewRoomManager (which fills in the
// defaults); the config form exists so tests can drop graceTTL to a
// few milliseconds without sleeping the full minute.
type RoomManagerConfig struct {
	// SchemaVersion stamped on every persisted op-log row.
	// Empty → DefaultSchemaVersion.
	SchemaVersion string
	// GraceTTL controls how long a Room survives without subscribers.
	// Zero → DefaultGraceTTL.
	GraceTTL time.Duration
}

// RoomManager is the single entry point for the collab WS handler.
// It owns the OpBus, the per-item Room map, and the lifecycle (lazy
// create, grace-TTL reclaim, graceful shutdown).
//
// Construction is via NewRoomManager(store, bus). The bus must be
// the SAME instance that any other broadcasting code (e.g. future
// designated-applier hooks in TASK-1257) shares — multiple buses
// would silo their fan-out and break cross-tab live editing.
type RoomManager struct {
	store         opLogStore
	bus           OpBus
	schemaVersion string
	graceTTL      time.Duration

	mu    sync.Mutex
	rooms map[string]*Room

	// activeJoins tracks every in-flight Join goroutine so Close can
	// act as a true drain barrier on server shutdown. Without this
	// Wait, http.Server.Shutdown returns before hijacked WS sessions
	// finish their tear-down, and a deferred store close races
	// in-flight AppendYjsUpdate calls.
	activeJoins sync.WaitGroup
}

// NewRoomManager wires the store + bus together with production defaults.
func NewRoomManager(store opLogStore, bus OpBus) *RoomManager {
	return NewRoomManagerWithConfig(store, bus, RoomManagerConfig{})
}

// NewRoomManagerWithConfig is the explicit-config form. Empty config
// fields fall back to package defaults.
func NewRoomManagerWithConfig(store opLogStore, bus OpBus, cfg RoomManagerConfig) *RoomManager {
	schemaVersion := cfg.SchemaVersion
	if schemaVersion == "" {
		schemaVersion = DefaultSchemaVersion
	}
	graceTTL := cfg.GraceTTL
	if graceTTL <= 0 {
		graceTTL = DefaultGraceTTL
	}
	return &RoomManager{
		store:         store,
		bus:           bus,
		schemaVersion: schemaVersion,
		graceTTL:      graceTTL,
		rooms:         make(map[string]*Room),
	}
}

// Join attaches a freshly-upgraded WebSocket connection to the room
// for itemID. Replays the op-log to the new peer, spins up an inbound
// reader and an outbound writer, and blocks until the WebSocket
// closes (graceful close frame or transport failure). The caller —
// typically the HTTP handler — should defer conn.Close so that any
// resources held by the WS upgrader are released after this returns.
//
// Returns whatever error caused the WebSocket to close, or nil on a
// normal close. The handler typically logs but doesn't act on the
// return value: the connection is gone either way.
func (m *RoomManager) Join(itemID string, conn *websocket.Conn) error {
	m.activeJoins.Add(1)
	defer m.activeJoins.Done()

	for attempt := 0; attempt < 3; attempt++ {
		room := m.getOrCreate(itemID)

		rc := &roomConn{
			id:   nextConnID(),
			conn: conn,
			bus:  m.bus.Subscribe(itemID),
		}

		if err := room.addConn(rc); err != nil {
			// Race: the grace timer reclaimed the room between
			// getOrCreate and addConn. Unsubscribe the channel we
			// just opened (otherwise the bus leaks the slot until
			// the bus is closed) and retry. The next getOrCreate
			// won't find the now-deleted room and will mint a
			// fresh one.
			m.bus.Unsubscribe(rc.bus)
			if errors.Is(err, errRoomClosing) {
				continue
			}
			return err
		}

		return m.runConn(room, rc)
	}
	return errTooManyJoinRetries
}

// runConn drives one connection through its full lifecycle: spawn
// writer (drains the bus subscription concurrently with replay),
// stream the op-log replay, run reader, tear down.
//
// The writer is started BEFORE the replay so live broadcasts that
// arrive during a long replay can't overflow the 64-event bus
// channel and silently drop. Yjs CRDTs are commutative — applying
// op 100 (live) then op 50 (replay) produces the same final Y.Doc
// as the reverse order — so interleaving replay frames and live
// updates on the same conn is correct. Both code paths write
// through rc.writeMessage which holds writeMu, so we never violate
// gorilla's "one writer at a time per conn" rule.
//
// The trade-off: a peer might briefly see updates "out of causal
// order" during the replay window. That's a UX wobble, not a
// correctness issue. The alternative — buffer-then-flush — would
// require an unbounded queue or risk losing live updates the way
// the original implementation did.
func (m *RoomManager) runConn(room *Room, rc *roomConn) error {
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		room.writeLoop(rc)
	}()

	if err := room.replayTo(rc); err != nil {
		room.removeConn(rc)
		<-writerDone
		return err
	}

	// Read loop blocks until the WS closes.
	readErr := room.readLoop(rc)

	// Reader returned: take the conn out of the room (which closes
	// the bus subscription, which unblocks the writer).
	room.removeConn(rc)

	// Wait for the writer to drain before returning so the handler's
	// `defer conn.Close()` doesn't fire mid-WriteMessage.
	<-writerDone

	return readErr
}

// getOrCreate returns the existing Room for itemID or, atomically
// under m.mu, mints a new one. Holding m.mu across the lookup +
// insertion keeps the grace-expiry path (which also takes m.mu)
// from interleaving and orphaning a freshly-created Room.
func (m *RoomManager) getOrCreate(itemID string) *Room {
	m.mu.Lock()
	defer m.mu.Unlock()

	if r, ok := m.rooms[itemID]; ok {
		return r
	}
	r := &Room{
		itemID:        itemID,
		store:         m.store,
		bus:           m.bus,
		schemaVersion: m.schemaVersion,
		graceTTL:      m.graceTTL,
		conns:         make(map[*websocket.Conn]*roomConn),
		onIdle:        m.markRoomGone,
	}
	m.rooms[itemID] = r
	return r
}

// markRoomGone is the Room → Manager callback the grace timer fires
// on its way out. The Room has already set closing = true under its
// own mutex; here we just unhook the manager's lookup so the next
// Join mints a fresh Room.
func (m *RoomManager) markRoomGone(itemID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rooms, itemID)
	slog.Debug("collab: room reclaimed after grace TTL", "item_id", itemID)
}

// RoomCount is a test/debug accessor. Production code shouldn't make
// decisions based on this — the count is racy with grace-timer
// expirations.
func (m *RoomManager) RoomCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rooms)
}

// Close stops every active room AND blocks until every in-flight
// Join goroutine has returned. After Close, Join is undefined —
// callers must coordinate shutdown so no new Join races happen
// alongside Close. Used by Server.Stop on graceful shutdown to
// ensure no collab goroutine is still running by the time the
// store is closed.
//
// Two phases:
//
//   1. closeAll on every room — closes each WebSocket from the
//      server side, which causes the corresponding readLoop to
//      return, removeConn to fire, the bus subscription to close,
//      and writeLoop to exit. The Join goroutine that was running
//      runConn then returns naturally.
//   2. activeJoins.Wait — blocks until step 1's effects propagate
//      through every still-running Join. Without this Wait, Close
//      returns before the goroutines actually exit.
func (m *RoomManager) Close() {
	m.mu.Lock()
	rooms := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		rooms = append(rooms, r)
	}
	m.rooms = make(map[string]*Room)
	m.mu.Unlock()

	for _, r := range rooms {
		r.closeAll()
	}

	m.activeJoins.Wait()
}
