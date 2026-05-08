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

// errManagerClosed is returned by Join when Close has already run.
// http.Server.Shutdown does NOT wait for hijacked WS handlers, so a
// late Join can race a finishing shutdown. Returning a fast error
// closes the WS cleanly and avoids touching a torn-down store.
var errManagerClosed = errors.New("collab: room manager is closed")

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

	mu     sync.Mutex
	rooms  map[string]*Room
	closed bool // set under mu by Close; Join short-circuits when true

	// activeJoins tracks every in-flight Join goroutine so Close can
	// act as a true drain barrier on server shutdown. Without this
	// Wait, http.Server.Shutdown returns before hijacked WS sessions
	// finish their tear-down, and a deferred store close races
	// in-flight AppendYjsUpdate calls. The Add call lives inside
	// m.mu so it can't interleave with Close's closed=true write —
	// either the Add happens before closed=true (Wait will block
	// for it) or closed=true happens first (Join returns
	// errManagerClosed without ever Add'ing).
	activeJoins sync.WaitGroup

	// itemLocks is a per-item Mutex pool that serialises Join's
	// addConn+replayTo critical section with PruneAndApply. Without
	// this, a CLI/MCP/API direct write that ApplyExternalContent
	// classified as "no live editors" can race a fresh Join: the new
	// client's replayTo loads the soon-to-be-pruned op-log and
	// ends up with stale Y.Doc state, which later overwrites the
	// freshly-written items.content on the next idle flush.
	//
	// The lock is released before Join's readLoop so concurrent
	// peers can edit simultaneously — only the setup phase (where
	// op-log staleness matters) is serialised. Per Codex review
	// round 5.
	itemLocksMu sync.Mutex
	itemLocks   map[string]*sync.Mutex
}

// itemLock returns the lazily-allocated mutex guarding setup-phase
// operations on itemID. Locks live in the manager for the lifetime of
// the process — for a workspace with many items this is at most a few
// hundred bytes per item, which is acceptable.
func (m *RoomManager) itemLock(itemID string) *sync.Mutex {
	m.itemLocksMu.Lock()
	defer m.itemLocksMu.Unlock()
	if l, ok := m.itemLocks[itemID]; ok {
		return l
	}
	if m.itemLocks == nil {
		m.itemLocks = make(map[string]*sync.Mutex)
	}
	l := &sync.Mutex{}
	m.itemLocks[itemID] = l
	return l
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
	// Gate Add on the closed flag under m.mu so a late Join (e.g. a
	// hijacked WS handler that didn't enter Join until AFTER Close
	// returned) can't sneak past the drain barrier.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errManagerClosed
	}
	m.activeJoins.Add(1)
	m.mu.Unlock()
	defer m.activeJoins.Done()

	itemLock := m.itemLock(itemID)

	for attempt := 0; attempt < 3; attempt++ {
		room := m.getOrCreate(itemID)
		if room == nil {
			// Close raced in between our closed-check above and
			// getOrCreate. Bail with the same fast error so the
			// handler closes the WS cleanly.
			return errManagerClosed
		}

		rc := &roomConn{
			id:          nextConnID(),
			conn:        conn,
			bus:         m.bus.Subscribe(itemID),
			connectedAt: time.Now(),
		}

		// Hold the per-item lock across addConn + replayTo so a
		// concurrent PruneAndApply (CLI / MCP / API direct write
		// while we're mid-setup) can't pull stale op-log rows out
		// from under our feet. The lock is released inside runConn
		// before the long-lived readLoop so concurrent peers can
		// edit simultaneously. Per Codex review round 5.
		itemLock.Lock()
		if err := room.addConn(rc); err != nil {
			itemLock.Unlock()
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

		return m.runConn(room, rc, itemLock)
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
func (m *RoomManager) runConn(room *Room, rc *roomConn, itemLock *sync.Mutex) error {
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		room.writeLoop(rc)
	}()

	replayErr := room.replayTo(rc)
	// Release the per-item setup lock before the long-lived readLoop
	// so concurrent peers + future PruneAndApply calls aren't gated
	// on this conn's full lifetime.
	itemLock.Unlock()

	if replayErr != nil {
		room.removeConn(rc)
		<-writerDone
		return replayErr
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
//
// Returns nil after Close has been called — the caller should
// translate that to errManagerClosed. In practice Join checks
// m.closed earlier and bails before reaching here, but this guard
// keeps a future caller honest if getOrCreate gets reused.
func (m *RoomManager) getOrCreate(itemID string) *Room {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
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
		pendingAcks:   make(map[string]*pendingApplierAck),
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

// ErrRoomActiveDuringPrune is returned by PruneAndApply when a live
// room (with at least one connected peer) appears for the itemID
// between the caller's ApplyExternalContent check and PruneAndApply's
// own re-check under the per-item lock. Callers should fall through
// to a plain direct write (without pruning the op-log) — the live
// peers' Y.Doc state cannot be invalidated safely.
var ErrRoomActiveDuringPrune = errors.New("collab: room became active during prune attempt")

// PruneAndApply runs applyFn under the per-item setup lock so it is
// strictly serialised with any in-flight Join's addConn+replayTo for
// the same itemID. Used by the items PATCH handler to prune the
// op-log + write items.content directly when ApplyExternalContent
// classifies the request as "no live editors" (ErrNoActiveRoom or
// ErrNoApplierAvailable).
//
// Returns ErrRoomActiveDuringPrune if a room with live conns has
// appeared since the caller's classification check; otherwise the
// error from applyFn (if any). The caller is expected to fall
// through to a plain direct write in the active-room case so the
// PATCH still completes.
//
// Why this matters: ApplyExternalContent's "no room" answer is a
// point-in-time snapshot. Without serialisation, a fresh Join can
// slip in between that check and the prune, replay the
// soon-to-be-pruned op-log into a new client, and end up with stale
// Y.Doc state that later overwrites the freshly-written
// items.content on the next idle flush. Per Codex review round 5.
func (m *RoomManager) PruneAndApply(itemID string, applyFn func() error) error {
	lock := m.itemLock(itemID)
	lock.Lock()
	defer lock.Unlock()

	// Re-verify under the lock: if a room with live conns has
	// appeared, refuse to prune (peers' Y.Doc would diverge from
	// an empty op-log).
	m.mu.Lock()
	hasLivePeers := false
	if r, ok := m.rooms[itemID]; ok {
		r.mu.Lock()
		hasLivePeers = len(r.conns) > 0
		r.mu.Unlock()
	}
	m.mu.Unlock()
	if hasLivePeers {
		return ErrRoomActiveDuringPrune
	}

	return applyFn()
}

// closeFrameDeadline is the absolute time budget for sending a
// CloseMessage frame via WriteControl before falling through to a
// plain Close. Generous enough that a healthy connection always
// completes; short enough that a stuck-write conn doesn't block
// the revoke path.
const closeFrameDeadline = 1 * time.Second

// CloseConn force-closes a single WebSocket connection registered
// with the manager, sending a close frame with a machine-readable
// reason first. Used by the auth-revalidation timer in
// handleCollab (TASK-1256) to evict a peer whose workspace access
// was revoked mid-stream.
//
//   - itemID  scopes the lookup; (purely informational here, the
//     close-frame call doesn't actually need it but the
//     param keeps the API symmetric for a future
//     find-by-room metric).
//   - conn    the *exact* websocket.Conn the manager is tracking;
//     not a tab/session id.
//   - code    a websocket.Close* code (e.g. ClosePolicyViolation
//     for "you are no longer authorized").
//   - reason  human-readable string the close frame carries to the
//     client. Kept short — the WS spec caps the close
//     frame's reason at ~123 bytes.
//
// CRITICAL: the close frame is sent via conn.WriteControl which
// is concurrency-safe with the room's writeLoop / replay (per
// gorilla's documented contract — WriteControl does not contend
// on the conn's normal write mutex). Acquiring writeMu would
// instead block the revoke until any in-flight WriteMessage to a
// slow peer finished, defeating the "evict immediately" goal.
//
// Best-effort: WriteControl errors (already-closed conn, deadline
// exceeded) fall through to plain Close. Either way the conn is
// not usable when this returns.
func (m *RoomManager) CloseConn(itemID string, conn *websocket.Conn, code int, reason string) {
	if conn == nil {
		return
	}
	_ = conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason),
		time.Now().Add(closeFrameDeadline),
	)
	_ = conn.Close()
	_ = itemID // reserved for future per-room metrics; see doc above
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
//  1. closeAll on every room — closes each WebSocket from the
//     server side, which causes the corresponding readLoop to
//     return, removeConn to fire, the bus subscription to close,
//     and writeLoop to exit. The Join goroutine that was running
//     runConn then returns naturally.
//  2. activeJoins.Wait — blocks until step 1's effects propagate
//     through every still-running Join. Without this Wait, Close
//     returns before the goroutines actually exit.
func (m *RoomManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
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
