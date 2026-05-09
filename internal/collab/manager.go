package collab

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// DefaultSchemaVersion is the schema-version stamp used by all rooms
// today. TASK-1268 plumbs the rebuild flow: each Join's announced
// client version is checked against the latest op-log row's stamp
// and the room manager prunes the op-log when they diverge. The
// constant itself bumps in lockstep with the web client's
// `web/src/lib/collab/schemaVersion.ts` SCHEMA_VERSION on any
// breaking change to the Tiptap extension set or Y.Doc shape.
const DefaultSchemaVersion = "1"

// SchemaVersion exposes the version this manager stamps on persisted
// op-log rows. The HTTP collab handler uses it to validate incoming
// `?schema_version=...` query params before upgrading the WebSocket —
// a client running a different version is rejected at the upgrade
// stage rather than admitted and silently corrupting the op-log.
func (m *RoomManager) SchemaVersion() string { return m.schemaVersion }

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

// ErrForceRefreshSent is returned by Join when the client's
// announced `?since=<id>` was below MIN(item_yjs_updates.id) — rows
// it expected to replay have been pruned. The handler has already
// emitted a force_refresh JSON control frame; the caller should
// close the conn cleanly. Per TASK-1319.
var ErrForceRefreshSent = errors.New("collab: client cursor below op-log MIN; force_refresh sent")

// Join attaches a freshly-upgraded WebSocket connection to the room
// for itemID. Replays the op-log to the new peer, spins up an inbound
// reader and an outbound writer, and blocks until the WebSocket
// closes (graceful close frame or transport failure). The caller —
// typically the HTTP handler — should defer conn.Close so that any
// resources held by the WS upgrader are released after this returns.
//
// `since` is the client's announced highest-applied op-log id (parsed
// from `?since=<id>` on the upgrade URL). When non-zero AND below
// MIN(item_yjs_updates.id) for this item, Join sends a force_refresh
// control frame and returns ErrForceRefreshSent — the client must
// discard local Y.Doc state and reconnect with `?since=0`. Per
// TASK-1319.
//
// Returns whatever error caused the WebSocket to close, or nil on a
// normal close. The handler typically logs but doesn't act on the
// return value: the connection is gone either way.
func (m *RoomManager) Join(itemID string, conn *websocket.Conn, since int64) error {
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
		// Acquire the per-item setup lock BEFORE creating the room
		// so the schema-rebuild + force_refresh checks below can
		// run without leaking an empty m.rooms entry on bail-out.
		// Per Codex round 5 [P2] of TASK-1319.
		itemLock.Lock()

		// Schema-mismatch rebuild (TASK-1268). Runs before the
		// room is created so a rebuild-then-bail path can't leave
		// an orphan room behind; the rebuild itself is a store-
		// only mutation that doesn't depend on the in-memory Room.
		// Concurrent fresh Joins for this item block on itemLock,
		// so a peer arriving in this window sees the post-rebuild
		// op-log when it gets its turn.
		if err := m.maybeRebuildOnSchemaMismatch(itemID); err != nil {
			itemLock.Unlock()
			return err
		}

		// Resume-cursor / force_refresh check (TASK-1319). Run
		// AFTER the schema rebuild so a post-rebuild empty op-log
		// (which has no MIN) is treated correctly.
		//
		// `since > 0` means the client claims to have applied at
		// least one persisted op locally. Two ways that claim is
		// incompatible with the current op-log:
		//   - No rows exist (`!hasMin`): the entire op-log was
		//     pruned (PruneAndApply, schema rebuild, or dormant
		//     GC). The client's Y.Doc is built on top of ops that
		//     no longer exist; admitting it would let its on-open
		//     `Y.encodeStateAsUpdate` write resurrect the stale
		//     pre-prune document and overwrite items.content on
		//     the next flush.
		//   - `since < minID`: rows the client expected to replay
		//     have been pruned (the same hazard, just with a
		//     non-empty post-prune suffix).
		// Both branches force_refresh and bail BEFORE we touch
		// m.rooms — no orphan-room leak. Per Codex round 5 [P2].
		if since > 0 {
			minID, hasMin, merr := m.store.MinOpLogID(itemID)
			if merr != nil {
				itemLock.Unlock()
				return merr
			}
			needsRefresh := !hasMin || since < minID
			if needsRefresh {
				slog.Info("collab: client cursor incompatible with op-log; sending force_refresh",
					"item_id", itemID,
					"since", since,
					"min_id", minID,
					"has_min", hasMin,
				)
				_ = sendForceRefreshFrame(conn)
				itemLock.Unlock()
				return ErrForceRefreshSent
			}
		}

		room := m.getOrCreate(itemID)
		if room == nil {
			// Close raced in between our closed-check above and
			// getOrCreate. Bail with the same fast error so the
			// handler closes the WS cleanly.
			itemLock.Unlock()
			return errManagerClosed
		}

		rc := &roomConn{
			id:          nextConnID(),
			conn:        conn,
			bus:         m.bus.Subscribe(itemID),
			connectedAt: time.Now(),
		}

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

		return m.runConn(room, rc, itemLock, since)
	}
	return errTooManyJoinRetries
}

// distantFuture is the prune-everything cutoff we hand to
// PruneYjsUpdatesBefore. The store's prune is a strict-less-than on
// created_at; any row written with a sane RFC3339 timestamp will
// satisfy `created_at < 9999-01-01`.
var distantFuture = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// maybeRebuildOnSchemaMismatch implements the TASK-1268 rebuild flow.
//
// Reads the latest persisted op-log row's schema_version for itemID.
// If a row exists AND its version differs from the manager's current
// `schemaVersion`, the entire op-log for the item is pruned. Caller
// MUST hold the per-item setup lock so a concurrent peer's replayTo
// can't load the soon-to-be-pruned rows.
//
// Returns nil on the no-rows path and on the matched-version path —
// both are "nothing to do". A real DB error from either step short-
// circuits Join with the same error so the WS upgrade fails loudly.
//
// **Data loss disclosure.** When the latest op-log row's id exceeds
// `items.content_flushed_op_log_id` for the item (i.e. unflushed
// edits exist), the prune is unrecoverable: those ops are stamped
// with the OLD schema and can't be replayed against the new schema
// regardless of where they're stored. Lazy-seed (TASK-1261) will
// repopulate the Y.Doc from items.content, which is stale relative
// to the unflushed ops. We log a warn so operators see when a
// schema bump is dropping unsaved client edits. Per Codex review of
// TASK-1309 round 4 [P2].
func (m *RoomManager) maybeRebuildOnSchemaMismatch(itemID string) error {
	latest, latestID, ok, err := m.store.LatestYjsUpdateSchemaVersion(itemID)
	if err != nil {
		return err
	}
	if !ok || latest == m.schemaVersion {
		return nil
	}

	// Pre-prune watermark check. Unflushed ops would be lost; we
	// can't avoid the loss (old-schema ops can't migrate forward),
	// but we surface it.
	flushedID, flushedOK, err := m.store.GetItemContentFlushedOpLogID(itemID)
	if err != nil {
		// Watermark read failed — proceed with the prune (we still
		// have to: the schema-mismatch case is non-negotiable) but
		// log the failure separately.
		slog.Warn("collab: schema-mismatch rebuild: watermark read failed",
			"item_id", itemID,
			"error", err,
		)
	} else if !flushedOK || latestID > flushedID {
		// flushedOK==false → never flushed, every op is unflushed.
		// latestID > flushedID → some ops past the watermark.
		// We can't avoid the prune here (old-schema ops can't replay
		// in the new schema regardless of where they're stored), but
		// the WARN tells operators a schema bump dropped some
		// unsaved client edits — they may want to investigate which
		// items were affected and contact the affected users.
		// Per Codex review of TASK-1309 round 4 [P2].
		slog.Warn("collab: schema-mismatch rebuild will drop unflushed ops",
			"item_id", itemID,
			"latest_op_log_id", latestID,
			"content_flushed_op_log_id", flushedID,
			"watermark_set", flushedOK,
		)
	}

	pruned, err := m.store.PruneYjsUpdatesBefore(itemID, distantFuture)
	if err != nil {
		return err
	}
	slog.Info("collab: schema-version mismatch; pruned op-log",
		"item_id", itemID,
		"server_version", m.schemaVersion,
		"persisted_version", latest,
		"rows_pruned", pruned,
	)
	return nil
}

// DefaultPruneMinAge is the floor age for op-log dormancy in the
// periodic prune sweeper (TASK-1309). An item must have NO op-log
// rows newer than `now - minAge` to be eligible. 24 hours covers
// every realistic mobile-suspend / network-blip / lock-screen
// interval and leaves headroom for travel-on-flaky-wifi reconnects.
//
// Pass `0` (or any non-positive value) to PruneSweep to fall back
// to this default.
const DefaultPruneMinAge = 24 * time.Hour

// PruneSweepResult records what one PruneSweep accomplished. Surfaced
// to the server-level periodic ticker so it can log a one-line
// summary per sweep.
type PruneSweepResult struct {
	// ItemsScanned: items returned by the dormancy query at sweep
	// start. Some of these may turn out to be non-dormant by the
	// time we acquire their per-item lock and run the conditional
	// delete; those count toward ItemsSkipped, not ItemsPruned.
	ItemsScanned int
	// ItemsPruned: items where the conditional DELETE actually
	// removed rows (i.e. confirmed dormant under the lock).
	ItemsPruned int
	// ItemsSkipped: items that became non-dormant between the
	// candidate query and the conditional DELETE (a peer reconnected
	// and wrote a row), or that were skipped because an active
	// in-memory Room exists (covers the grace-TTL window after the
	// last peer disconnected).
	ItemsSkipped int
	// RowsPruned: total rows deleted across all pruned items.
	RowsPruned int64
	// Errors: per-item prune failures. Sweep continues past errors
	// so a single broken item doesn't block GC for the whole table.
	Errors int
}

// PruneSweep finds every item whose ENTIRE op-log is older than
// `minAge` and prunes the whole op-log for those items under the
// per-item lock. Returns a summary.
//
// **Why whole-log only.** Yjs op streams are causally linked: a
// recent op can reference structs created in older ops. Prefix-
// pruning (delete old rows, keep recent) corrupts replay because
// the suffix's references can't be resolved. Per Codex review of
// the original TASK-1309 [P1]. Whole-log prune is safe because the
// next cold connect lazy-seeds from items.content (TASK-1261),
// producing a fresh self-consistent Y.Doc.
//
// `minAge` is the minimum age of the NEWEST op-log row for an item
// to be considered dormant. Pass 0 to use DefaultPruneMinAge.
//
// Coordination:
//   - Per-item lock matches the lock Join takes for its addConn +
//     replayTo critical section; a fresh peer can't race the
//     prune-then-replay sequence.
//   - In-memory active Room check before the prune skips items
//     where peers are still attached (the grace-TTL window after
//     the last conn dropped also counts as "active" — the room
//     could come back via grace-cancel-on-reconnect).
//   - Conditional DELETE in the store re-checks dormancy
//     atomically. If a row was appended between the candidate
//     query and the DELETE (e.g. a sneaky readLoop write under
//     appendMu), the DELETE deletes nothing.
//
// Per TASK-1309 (PLAN-1248).
func (m *RoomManager) PruneSweep(minAge time.Duration) (PruneSweepResult, error) {
	var res PruneSweepResult

	if minAge <= 0 {
		minAge = DefaultPruneMinAge
	}
	cutoff := time.Now().Add(-minAge)

	items, err := m.store.ListDormantOpLogItemsBefore(cutoff)
	if err != nil {
		return res, err
	}
	res.ItemsScanned = len(items)

	for _, itemID := range items {
		// Bail early if Close has fired — no point pruning a
		// store the manager is winding down.
		m.mu.Lock()
		closed := m.closed
		hasRoom := m.rooms[itemID] != nil
		m.mu.Unlock()
		if closed {
			return res, nil
		}
		if hasRoom {
			// Active Room (or grace-TTL pending). Skip — the
			// next sweep can pick this item up if the room
			// goes idle by then. Active rooms naturally accrue
			// new op-log rows that would defeat dormancy
			// anyway; skipping here is just a fast-path before
			// taking the per-item lock.
			res.ItemsSkipped++
			continue
		}

		lock := m.itemLock(itemID)
		lock.Lock()

		// Re-check active room under the lock. A Join could have
		// raced between our outer check and this lock acquisition;
		// the lock now serialises us against any in-flight Join's
		// addConn+replayTo, but a Join that has already created
		// the Room and released the lock could have left m.rooms
		// non-nil. (Joins hold the lock across replay; once
		// released, the room is live.)
		m.mu.Lock()
		hasRoom = m.rooms[itemID] != nil
		m.mu.Unlock()
		if hasRoom {
			lock.Unlock()
			res.ItemsSkipped++
			continue
		}

		// Conditional DELETE: deletes everything for itemID iff
		// no row >= cutoff exists. n=0 means a recent row was
		// appended between the candidate query and now; that's
		// fine, just a skip.
		n, err := m.store.PruneItemOpLogIfDormantBefore(itemID, cutoff)
		lock.Unlock()

		if err != nil {
			slog.Warn("collab: prune sweep: per-item prune failed",
				"item_id", itemID,
				"error", err,
			)
			res.Errors++
			continue
		}
		if n == 0 {
			res.ItemsSkipped++
			continue
		}
		res.RowsPruned += n
		res.ItemsPruned++
	}
	return res, nil
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
func (m *RoomManager) runConn(room *Room, rc *roomConn, itemLock *sync.Mutex, since int64) error {
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		room.writeLoop(rc)
	}()

	highestReplayed, replayErr := room.replayTo(rc, since)
	// Release the per-item setup lock before the long-lived readLoop
	// so concurrent peers + future PruneAndApply calls aren't gated
	// on this conn's full lifetime.
	itemLock.Unlock()

	if replayErr != nil {
		room.removeConn(rc)
		<-writerDone
		return replayErr
	}

	// Anchor the client's resume cursor (TASK-1319). If the replay
	// produced rows, advertise the highest id we sent. If not — a
	// fresh item, an empty op-log after a recent prune, or an
	// up-to-date `since` cursor that already covered every row —
	// look up the current MAX and use that. Best-effort: a write
	// error here mirrors any other writeMessage error and the read
	// loop will tear the conn down.
	cursorID := highestReplayed
	if cursorID == 0 {
		if maxID, ok, merr := m.store.MaxOpLogID(room.itemID); merr != nil {
			slog.Warn("collab: lookup MaxOpLogID for initial cursor failed",
				"item_id", room.itemID,
				"error", merr,
			)
		} else if ok {
			cursorID = maxID
		}
		// `since > 0` callers might already be ahead of the
		// server's MAX (e.g. they had ops the server didn't —
		// the on-open state push will reconcile). Fall back to
		// `since` so we don't accidentally regress the client's
		// stored cursor.
		if cursorID < since {
			cursorID = since
		}
	}
	// cursorID == 0 is a legitimate value for a never-touched item;
	// emit it anyway so the client clears any stale sessionStorage
	// entry from an earlier life of this item id.
	if err := room.sendOpLogCursor(rc, cursorID); err != nil {
		// Treat a cursor write failure the same as a replay
		// write failure — the conn is doomed.
		room.removeConn(rc)
		<-writerDone
		return err
	}
	// Replay + initial cursor are committed; allow writeLoop to
	// resume cursor advancement on subsequent live ops. Per Codex
	// round 5 [P1] of TASK-1319.
	rc.replayDone.Store(true)

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
