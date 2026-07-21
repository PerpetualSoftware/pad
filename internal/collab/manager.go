package collab

import (
	"encoding/json"
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

	// restoreBoundaries records, per item, the pre-prune MAX(op-log.id)+1 at the
	// most recent version RESTORE (BUG-2264, prune+reseed). A collab-snapshot
	// flush whose op_log_cursor is BELOW this boundary captured a Y.Doc that
	// predates the restore's op-log wipe — its markdown is the stale PRE-restore
	// content — so the PATCH handler rejects it (RestoreBoundary), preventing an
	// in-flight pre-restore flush from re-clobbering items.content after the
	// restore write.
	//
	// In-memory (process-lifetime, monotonic non-decreasing). Op-log ids never
	// reset, so a stale boundary can never falsely reject a genuine post-restore
	// flush (whose cursor is always >= the boundary). One int64 per restored
	// item — same acceptable footprint as itemLocks. For the process-restart
	// residual this shares with lastRestoreSeqs, see that field's doc.
	restoreBoundaryMu sync.Mutex
	restoreBoundaries map[string]int64

	// lastRestoreSeqs records, per item, the item.seq assigned by the most
	// recent version RESTORE — i.e. the content generation of the restored (and
	// now canonical) items.content that the pruned op-log's peers must rebuild
	// from (BUG-2264, closing the additional-P1 + Codex-xhigh stale-seed race).
	// On Join a client announces the items.content `seq` its Y.Doc was seeded
	// from (?content_seq=); if that seed predates the last restore (contentSeq <
	// lastRestoreSeq), the client's seed is the stale PRE-restore content and the
	// empty/post-restore op-log can't reconcile it (a since==0 client slips the
	// resume-cursor MIN check entirely), so Join force_refreshes it BEFORE its
	// on-open Y.encodeStateAsUpdate can re-push the stale document. This closes
	// the two seed-based clobbers the resume-cursor boundary alone can't: a lost
	// force_refresh frame on a cursor-0 peer, and a browser that GETs
	// items.content, blocks in Join behind the restore, and joins after the prune.
	//
	// In-memory / monotonic, mirroring restoreBoundaries. Seq is
	// workspace-monotonic and only bumped when THIS item is written, so a stale
	// value can never falsely reject a client that seeded at-or-after the restore.
	//
	// This is a FAST-PATH cache, not the source of truth. The restart hole it
	// used to have — a server restart resets the map, and a restart is NOT a safe
	// refresh barrier because the BROWSER's in-memory Y.Doc survives the WS drop,
	// so a cursor-0 pre-restore provider reconnecting post-restart would slip the
	// since==0 resume-cursor check and re-push its stale document — is now closed
	// DURABLY by items.last_restore_seq (BUG-2264, migration 075/pg 053). The
	// restore stamps that column in its own tx; Join reads it (via
	// store.ItemLastRestoreSeq) whenever this in-memory fast-path misses, so a
	// stale-seeded tab is fenced even after a restart. This map only saves the
	// column read in the common (same-process) case.
	lastRestoreSeqMu sync.Mutex
	lastRestoreSeqs  map[string]int64
}

// SetRestoreBoundary records a restore boundary for itemID, keeping the
// stored value monotonically non-decreasing (a later restore always has a
// higher op-log MAX, and a stale/duplicate call can never lower it). See the
// restoreBoundaries field doc for the invariant this enforces (BUG-2264).
func (m *RoomManager) SetRestoreBoundary(itemID string, boundary int64) {
	if itemID == "" || boundary <= 0 {
		return
	}
	m.restoreBoundaryMu.Lock()
	defer m.restoreBoundaryMu.Unlock()
	if m.restoreBoundaries == nil {
		m.restoreBoundaries = make(map[string]int64)
	}
	if boundary > m.restoreBoundaries[itemID] {
		m.restoreBoundaries[itemID] = boundary
	}
}

// RestoreBoundary returns the restore boundary recorded for itemID, or
// (0, false) when none has been set. The collab-snapshot PATCH handler
// consults it to reject stale pre-restore flushes (BUG-2264).
func (m *RoomManager) RestoreBoundary(itemID string) (int64, bool) {
	m.restoreBoundaryMu.Lock()
	defer m.restoreBoundaryMu.Unlock()
	v, ok := m.restoreBoundaries[itemID]
	return v, ok
}

// SetLastRestoreSeq records the item.seq of the most recent restore for itemID,
// kept monotonically non-decreasing (a later restore always has a higher seq;
// a stale/duplicate call can never lower it). See the lastRestoreSeqs field doc
// for the seed-based clobber this closes (BUG-2264).
func (m *RoomManager) SetLastRestoreSeq(itemID string, seq int64) {
	if itemID == "" || seq <= 0 {
		return
	}
	m.lastRestoreSeqMu.Lock()
	defer m.lastRestoreSeqMu.Unlock()
	if m.lastRestoreSeqs == nil {
		m.lastRestoreSeqs = make(map[string]int64)
	}
	if seq > m.lastRestoreSeqs[itemID] {
		m.lastRestoreSeqs[itemID] = seq
	}
}

// LastRestoreSeq returns the item.seq of the most recent restore for itemID, or
// (0, false) when none has been recorded. Join consults it to force_refresh a
// client whose announced ?content_seq predates the last restore (BUG-2264).
func (m *RoomManager) LastRestoreSeq(itemID string) (int64, bool) {
	m.lastRestoreSeqMu.Lock()
	defer m.lastRestoreSeqMu.Unlock()
	v, ok := m.lastRestoreSeqs[itemID]
	return v, ok
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

// ErrStaleSeedFenceUnavailable is returned by Join when the DURABLE stale-seed
// boundary read (items.last_restore_seq) fails on an in-memory miss, so we can't
// confirm the client's ?content_seq seed is current (BUG-2264). We fail CLOSED —
// the conn is closed WITHOUT admitting it (so a stale seed can't be pushed) — but
// via a plain close, NOT a force_refresh: force_refresh would make the client
// DISCARD its Y.Doc and rebuild, and a persistent read error (DB down) would then
// spin an unbounded refresh/GET loop that also drops local-only edits (Codex
// xhigh). A plain close is retryable: the client's provider reconnects with
// backoff, Y.Doc intact, and once the DB recovers it's admitted (or legitimately
// force_refreshed if genuinely stale). The handler closes the conn on this.
var ErrStaleSeedFenceUnavailable = errors.New("collab: durable restore-boundary read failed; retryable close")

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
// `canWrite` reports whether this connection may PERSIST inbound sync
// frames. A read-only participant (workspace viewer / view-only guest,
// TASK-265) is admitted for live view + presence but its inbound sync
// frames are dropped in readLoop rather than persisted/rebroadcast.
// The handler's periodic revalidation can update this mid-session via
// SetConnWritable (editor⇄viewer role change).
//
// `onRegistered`, when non-nil, is invoked exactly once immediately
// after the conn has been added to the room's conn map (so a
// concurrent SetConnWritable can find it). The handler uses it to gate
// the start of its revalidation loop, closing the startup-window race
// where a demotion observed before registration would no-op. It is NOT
// called on the bail-out paths (schema rebuild, force_refresh, manager
// closed, retries) that return before addConn succeeds. Per TASK-265.
//
// Returns whatever error caused the WebSocket to close, or nil on a
// normal close. The handler typically logs but doesn't act on the
// return value: the connection is gone either way.
func (m *RoomManager) Join(itemID string, conn *websocket.Conn, since int64, contentSeq int64, canWrite bool, onRegistered func()) error {
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

		// Stale-seed force_refresh (BUG-2264). Independent of the resume-cursor
		// check above: `?content_seq=<seq>` is the items.content generation the
		// client's Y.Doc was SEEDED from. If it predates the most recent restore
		// (contentSeq < lastRestoreSeq), the seed is the stale PRE-restore content
		// and the pruned op-log can't reconcile it — a since==0 client (fresh
		// session OR a cursor-0 peer whose force_refresh frame was lost) slips the
		// MIN check entirely and would re-push that stale document on open.
		//
		// The boundary has two tiers: the in-memory lastRestoreSeqs fast-path
		// (set under itemLock by ForceRefreshRoom, so a Join serialised behind a
		// restore sees it) and the DURABLE items.last_restore_seq column (written
		// in the restore's own tx). The durable value is the source of truth
		// across a server RESTART: after a restart the in-memory map is empty, so
		// a stale-seeded cursor-0 tab reconnecting would otherwise slip through —
		// on an in-memory miss we read the column so it is still fenced. contentSeq
		// ==0 (old bundle that doesn't announce it) falls through to the legacy
		// behaviour — no regression.
		if contentSeq > 0 {
			lastRestoreSeq, ok := m.LastRestoreSeq(itemID)
			if !ok {
				durable, hasDurable, derr := m.store.ItemLastRestoreSeq(itemID)
				if derr != nil {
					// FAIL CLOSED, but RETRYABLE (Codex xhigh): a read error means
					// we can't confirm the seed is current, so admitting the client
					// would let a stale cursor-0 tab push its pre-restore Y.Doc and
					// clobber the restored content once the DB recovers. Close the
					// conn WITHOUT admitting it — but via a plain close, not a
					// force_refresh: force_refresh discards the Y.Doc + rebuilds, so
					// a persistent read error would spin an unbounded refresh/GET
					// loop and drop local-only edits. A plain close lets the client
					// reconnect with backoff, Y.Doc intact, until the DB recovers.
					// Only reachable on an in-memory miss (post-restart), contentSeq>0.
					slog.Warn("collab: durable restore-boundary read failed; retryable close (fence unavailable)",
						"item_id", itemID, "error", derr)
					itemLock.Unlock()
					return ErrStaleSeedFenceUnavailable
				}
				if hasDurable {
					lastRestoreSeq, ok = durable, true
				}
			}
			if ok && contentSeq < lastRestoreSeq {
				slog.Info("collab: client seed predates last restore; sending force_refresh",
					"item_id", itemID,
					"content_seq", contentSeq,
					"last_restore_seq", lastRestoreSeq,
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
		rc.canWrite.Store(canWrite)

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

		// Conn is now registered in the room's conn map (SetConnWritable
		// can find it). Signal the handler so it can start revalidation
		// without racing this setup. Per TASK-265.
		if onRegistered != nil {
			onRegistered()
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

	// Anchor the client's resume cursor (TASK-1319). The cursor
	// MUST NEVER advertise an id whose binary frame this conn
	// hasn't actually delivered, otherwise a disconnect right after
	// the cursor leaves the client's persisted resume cursor
	// pointing past unreplayed rows. Two safe sources only:
	//   - `highestReplayed`: an id we just sent the binary frame
	//     for during replayTo.
	//   - `since`: the client's announced cursor — the client has
	//     already applied that op-log id locally, so re-advertising
	//     it never regresses past content.
	// We deliberately do NOT consult MAX(op-log.id) here: a live
	// op that landed during replay would be reflected in MAX but
	// hasn't been broadcast through this conn's writeLoop yet, and
	// a cursor=MAX would let the client persist a value that
	// outpaces its received binary frames. Per Codex round 6 [P1].
	// Acquire writeMu across the read-max + cursor-send +
	// replayDone-flip so writeLoop's per-event critical section
	// (round 22) cannot interleave a record-vs-read window.
	// Holding the lock means: any in-flight writeLoop event
	// finishes its record/send before we read max; any subsequent
	// event sees replayDone=true and emits its own live cursor.
	// Per Codex round 22 [P1] of TASK-1319.
	rc.writeMu.Lock()
	cursorID := highestReplayed
	if cursorID < since {
		cursorID = since
	}
	if liveMax := rc.maxLiveOpLogIDDuringReplay.Load(); liveMax > cursorID {
		cursorID = liveMax
	}
	payload, perr := json.Marshal(ControlMessage{
		Type:    ControlMessageOpLogCursor,
		OpLogID: cursorID,
	})
	if perr != nil {
		rc.writeMu.Unlock()
		room.removeConn(rc)
		<-writerDone
		return perr
	}
	if werr := rc.conn.WriteMessage(websocket.TextMessage, payload); werr != nil {
		rc.writeMu.Unlock()
		room.removeConn(rc)
		<-writerDone
		return werr
	}
	rc.replayDone.Store(true)
	rc.writeMu.Unlock()

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

// UnderItemLock runs fn while the per-item setup lock for itemID is
// held — the SAME lock Join's addConn+replayTo acquires and the SAME
// lock PruneAndApply runs its prune+write under. Used by the items
// PATCH handler's collab-snapshot validation path so the
// MIN(op-log.id) check and the items.content write are atomic
// w.r.t. concurrent prunes (PruneAndApply, schema rebuild, dormant
// GC's per-item DELETE). Without this serialization, a prune
// landing between the check and the write lets the stale
// collab-snapshot overwrite canonical content. Per Codex round 13
// [P1] of TASK-1319.
//
// Best-effort with a fast bail-out: if Close has fired the lock
// goroutine returns ErrManagerClosed without invoking fn. fn's
// own error (if any) is returned verbatim.
func (m *RoomManager) UnderItemLock(itemID string, fn func() error) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errManagerClosed
	}
	m.mu.Unlock()
	lock := m.itemLock(itemID)
	lock.Lock()
	defer lock.Unlock()
	return fn()
}

// PruneAndApply runs applyFn under the per-item setup lock so it is
// strictly serialised with any in-flight Join's addConn+replayTo for
// the same itemID. Used by the items PATCH handler to prune the
// op-log + write items.content directly when ApplyExternalContent
// classifies the request as "no live editors" (ErrNoActiveRoom or
// ErrNoApplierAvailable).
//
// Returns ErrRoomActiveDuringPrune if a room with a live WRITER conn
// has appeared since the caller's classification check; otherwise the
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
//
// Only a live WRITER peer blocks the prune (TASK-265). A read-only
// peer (workspace viewer / view-only guest) can never persist — its
// sync frames are dropped and it can't be an applier — so its presence
// does NOT force the caller onto the unsafe direct-write-without-prune
// fallback: the op-log is safely pruned even while viewers are
// attached, so a later editor lazy-seeds from the fresh items.content
// instead of replaying stale ops.
//
// ACCEPTED RESIDUAL (TASK-265 / BUG-2103): connected read-only peers
// keep a possibly-stale in-memory Y.Doc after this direct write until
// their next reconnect/refresh (their resume cursor now sits below the
// pruned op-log's MIN, so a reconnect force_refreshes and re-seeds). A
// viewer promoted to editor BEFORE re-syncing could push that stale
// content — a lost-update edge, not a security hole (a promoted viewer
// is a legitimate editor). This is best-effort degradation consistent
// with the pre-existing direct-write contract (see applier.go); a
// proactive re-seed/refresh to remaining read-only peers is tracked in
// BUG-2103.
func (m *RoomManager) PruneAndApply(itemID string, applyFn func() error) error {
	lock := m.itemLock(itemID)
	lock.Lock()
	defer lock.Unlock()

	m.mu.Lock()
	room := m.rooms[itemID]
	m.mu.Unlock()

	if room == nil {
		// No room / no peers — nothing can race the prune + write.
		return applyFn()
	}

	// Hold appendMu across the writer-scan AND applyFn so the
	// classification and the prune+write are atomic w.r.t. a concurrent
	// viewer→editor promotion. appendMu is the same lock readLoop takes
	// across its canWrite-check+persist and SetConnWritable takes when
	// flipping canWrite; without holding it here a viewer promoted right
	// after the scan could append a stale frame while the op-log is
	// pruned and content is written (a persist/prune ordering data
	// race). No socket I/O happens under this lock, and applyFn is a
	// pure store op (prune + UpdateItem) that never re-enters itemLock /
	// appendMu / room.mu, so there's no inversion or re-entrant
	// deadlock. Lock order: itemLock → appendMu → room.mu. Per TASK-265.
	room.appendMu.Lock()
	defer room.appendMu.Unlock()

	// Re-verify under the lock: only a live WRITER conn blocks the
	// prune (its Y.Doc would diverge from the emptied op-log) — route
	// through the applier protocol instead. A read-only conn can't
	// persist, so it does NOT block (see the accepted residual above).
	room.mu.Lock()
	for _, rc := range room.conns {
		if rc.canWrite.Load() {
			room.mu.Unlock()
			return ErrRoomActiveDuringPrune
		}
	}
	room.mu.Unlock()

	return applyFn()
}

// ForceRefreshRoom makes items.content the canonical source for an item's collab
// state: it runs `commit` under the per-item lock (so the caller can atomically
// set items.content to the value clients should rebuild from), prunes the ENTIRE
// per-item op-log (the commit does the prune in its own transaction), then
// force-refreshes every connected client (force_refresh frame + close) so each
// peer discards its in-memory Y.Doc and lazy-seeds from items.content on
// reconnect. Used by version-restore (BUG-2264): the restored content becomes
// canonical and every peer converges on it — unflushed edits are discarded,
// which is exactly restore semantics.
//
// `commit` MUST perform the canonical items.content write, the version row, AND
// the op-log wipe in ONE store transaction (the restore handler passes
// UpdateItemWithPreCheck + PruneItemOpLogTx). ForceRefreshRoom no longer prunes
// separately: a split prune/commit leaves a divergent state on any failure
// (Codex xhigh [P1]). The op-log wipe is what makes reconnecting/cold clients
// lazy-seed from items.content (an empty op-log ⇒ nothing to replay).
//
// Holding the per-item lock across the commit + boundary + broadcast serialises
// against Join's addConn+replayTo (and the collab-snapshot gate, which also runs
// under this lock), so a client reconnecting after its force_refresh blocks here
// until the op-log is empty AND the boundary is published, then rebuilds cleanly.
//
// Failure handling (Codex xhigh [P1]): the conns are frozen (rc.frozen=true) and
// appendMu is held BEFORE the fallible commit. If the commit fails the restore
// did not happen (the ONE tx rolled back — items.content, version, op-log wipe,
// and the boundary MAX read all together), so we UN-freeze the conns (peers keep
// editing their live Y.Doc, viewers keep their read-only status), do NOT publish
// the boundary, skip the reseed, and return the error — the room is left exactly
// as it was. The freeze uses a dedicated rc.frozen flag, NOT canWrite, so the
// mid-session auth revalidation loop (which writes canWrite) can neither thaw the
// freeze mid-restore nor get its viewer/editor decision clobbered by it.
//
// COMMIT-OUTCOME RECONCILIATION (BUG-2276 residual 1): a commit ERROR is
// ambiguous on Postgres but not on SQLite. On SQLite (the self-host shape) a
// commit either fsyncs or it doesn't, so when `reconcile` is nil an error is
// taken as a rollback verbatim (un-freeze + bail, above). On Postgres a commit
// that DURABLY lands but whose acknowledgement is lost (connection drop at the
// commit boundary) ALSO surfaces here as an error; blindly un-freezing would
// resume peers on a stale Y.Doc even though the DB now holds restored content + a
// pruned op-log, and a subsequent stale flush could then clobber. Un-freezing is
// still the RIGHT call for a genuine rollback (a real rollback must not discard
// peers' unflushed edits), so the fix is not "reseed on every error" (that
// regresses the common case) but commit-outcome reconciliation: the Postgres
// caller supplies a `reconcile` callback that RE-READS after a commit error to
// learn what actually happened, yielding three outcomes — (a) LANDED → treat as
// success (publish the boundary + reseed using the durably-stamped boundary/seq;
// do NOT un-freeze); (b) DEFINITELY rolled back → un-freeze + bail (the
// genuine-rollback case); (c) the reconcile read itself failed / outcome
// UNCERTAIN → keep the conns FROZEN and return (never un-freeze onto a
// possibly-stale doc — a frozen peer can't persist and converges cleanly once it
// reconnects/times out, the safe degraded mode). The whole mechanism is gated to
// Postgres by the caller passing `reconcile` ONLY when the store dialect is
// Postgres; with `reconcile == nil` the SQLite/self-host path stays byte-for-byte
// the pre-fix behavior. Tracked in BUG-2276.
//
// `commit` returns (pre-prune MAX(op-log), restored item.seq): both captured
// INSIDE its transaction, so the boundary can't fail-open on a MAX read error
// (the whole restore rolls back instead) and the seq is this restore's, not a
// concurrent writer's.
//
// The stale-SEED clobber that survives the boundary (BUG-2264 additional-P1 +
// Codex-xhigh) is closed by the restore boundary generation: the restore stamps
// the item's seq both in-memory (lastRestoreSeqs fast-path) and DURABLY
// (items.last_restore_seq, in the commit tx), and Join force_refreshes any client
// whose announced ?content_seq predates it. That catches a client whose Y.Doc
// seeded from PRE-restore items.content and would otherwise re-push it as a fresh
// (post-MAX) op — (1) a lost force_refresh frame on a cursor-0 peer, (2) a browser
// that GETs items.content, blocks in Join behind this restore, and joins AFTER
// the prune (so it isn't in forceRefreshAll), and (3) a cursor-0 pre-restore tab
// that reconnects AFTER a server restart (fenced off the durable column since the
// in-memory fast-path is empty then). See the lastRestoreSeqs field doc.
func (m *RoomManager) ForceRefreshRoom(itemID string, commit func() (int64, int64, error), reconcile func() (RestoreReconcileResult, error)) error {
	lock := m.itemLock(itemID)
	lock.Lock()
	defer lock.Unlock()

	m.mu.Lock()
	room := m.rooms[itemID]
	m.mu.Unlock()

	// P1c — freeze inbound persistence for the duration: hold appendMu (which
	// readLoop takes across its frozen/canWrite check + AppendYjsUpdate) AND mark
	// every conn frozen, so no live readLoop can append a frame after the prune
	// (which would survive as a stale op replayed on reconnect). A readLoop that
	// already read a frame and is queued on appendMu will, on acquiring it, see
	// frozen=true and drop the frame. Lock order: itemLock → appendMu → room.mu.
	// No socket I/O happens under appendMu.
	if room != nil {
		room.appendMu.Lock()
		room.mu.Lock()
		for _, rc := range room.conns {
			rc.frozen.Store(true)
		}
		room.mu.Unlock()
	}

	// The commit reads the pre-prune MAX(op-log), writes items.content=restored +
	// the version, and wipes the op-log in ONE transaction, returning (MAX, seq).
	// boundary/restoredSeq are the values the success path publishes: on a clean
	// commit they are (MAX+1, seq) straight from the return; on a Postgres commit
	// whose ack was lost but whose tx reconciliation proves DID land, they are
	// recovered from the durable stamps by `reconcile` (BUG-2276 residual 1). The
	// defaults cover the (prod-unused) commit==nil path — the historical
	// SetRestoreBoundary(1) / SetLastRestoreSeq(0).
	boundary, restoredSeq := int64(1), int64(0)
	if commit != nil {
		maxID, seq, err := commit()
		switch {
		case err == nil:
			boundary, restoredSeq = maxID+1, seq
		case reconcile == nil:
			// SQLite / self-host: a commit error is unambiguous — the ONE tx rolled
			// back (items.content, version, op-log wipe, boundary read all together),
			// so nothing changed on disk. Un-freeze the room and bail without
			// publishing the boundary or reseeding; the room is left exactly as it
			// was. (Byte-for-byte the pre-BUG-2276 behavior.)
			m.unfreezeAndReleaseAppend(room)
			return err
		default:
			// Postgres: the commit reported an error, but a durably-landed commit
			// whose ack was lost at the connection boundary ALSO surfaces here. Re-read
			// to learn what actually happened before discarding peers' state.
			res, rerr := reconcile()
			switch {
			case rerr != nil:
				// (c) UNCERTAIN — the reconcile read itself failed (or a not-found
				// re-read: the item may have been archived AFTER a durable-but-ack-lost
				// commit), so we cannot tell a genuine rollback from an ack-lost-but-
				// landed commit. Un-freezing onto a possibly-stale Y.Doc could let a
				// stale flush clobber content that may in fact be committed; but simply
				// leaving the conns frozen-and-open would silently drop every subsequent
				// edit forever (the collab read path has no WS read-deadline/heartbeat).
				// SAFEST: release appendMu (no socket I/O under appendMu), then
				// PLAIN-CLOSE the sockets — NOT force_refresh (we don't know
				// items.content is authoritative). Each client reconnects and
				// re-evaluates the durable restore fences fresh through Join, which is
				// correct whichever way the commit actually went. Leave rc.frozen set so
				// any already-read frame is still dropped as the readLoop unwinds.
				if room != nil {
					room.appendMu.Unlock()
					room.closeAllConnsPlain()
				}
				slog.Error("collab: version-restore commit outcome uncertain after ack loss; froze + plain-closed peers to force a safe reconnect",
					"item_id", itemID, "commit_err", err, "reconcile_err", rerr)
				return errors.Join(err, rerr)
			case !res.Landed:
				// (b) definitely rolled back — un-freeze + bail, identical to the
				// self-host path. Peers keep their unflushed edits.
				m.unfreezeAndReleaseAppend(room)
				return err
			default:
				// (a) landed despite the lost ack — treat as SUCCESS. Recover the
				// boundary + restored seq from the durable stamps and fall through to
				// the success path (publish + reseed); do NOT un-freeze.
				boundary, restoredSeq = res.Boundary, res.Seq
				slog.Warn("collab: version-restore commit ack lost but effects landed; reconciled to success",
					"item_id", itemID, "boundary", boundary, "restored_seq", restoredSeq, "commit_err", err)
			}
		}
	}

	// Commit succeeded (or reconciled to landed): items.content=restored, op-log
	// empty. Publish the stale-flush boundary = pre-prune MAX+1 (or 1 when empty).
	// IDs are AUTOINCREMENT/BIGSERIAL-monotonic across prunes, so every in-flight
	// snapshot's cursor (≤ pre-prune MAX) is below the boundary and rejected by
	// the collab-snapshot gate, while every genuine post-refresh op gets an id ≥
	// MAX+1 and is accepted. The gate runs under this same itemLock, so no
	// in-flight snapshot can slip a write between the prune (committed above) and
	// the boundary becoming visible.
	m.SetRestoreBoundary(itemID, boundary)

	// Record the restored content generation so Join can force_refresh any peer
	// whose ?content_seq seed predates it (the stale-SEED clobber the op-log-id
	// boundary alone can't fence — see the lastRestoreSeqs field doc). Set under
	// this same itemLock, so a Join serialised behind this restore sees the
	// updated generation.
	m.SetLastRestoreSeq(itemID, restoredSeq)

	// Reseed: every peer discards its in-memory Y.Doc and rebuilds from the (now
	// canonical) items.content on reconnect. The conns stay frozen (never cleared
	// on the success path) until forceRefreshAll closes them, so no late frame can
	// append even after appendMu is released for the socket fan-out.
	if room != nil {
		room.appendMu.Unlock()
		room.forceRefreshAll()
	}
	return nil
}

// RestoreReconcileResult reports what a post-commit-error re-read learned about
// whether a version-restore's transaction DURABLY landed despite a lost commit
// acknowledgement (BUG-2276 residual 1, Postgres-only). Boundary/Seq are valid
// ONLY when Landed is true — they are recovered from the durable restore stamps
// (items.restore_boundary_op_id = pre-prune MAX(op-log.id)+1, and
// items.last_restore_seq = the restored item.seq) so ForceRefreshRoom can reuse
// its normal success path. The producer returns a non-nil error instead (leaving
// this zero) when it cannot tell landed from rolled-back — the UNCERTAIN outcome,
// which keeps the room frozen.
type RestoreReconcileResult struct {
	Landed   bool
	Boundary int64
	Seq      int64
}

// unfreezeAndReleaseAppend reverses the freeze applied at the top of
// ForceRefreshRoom on a genuine rollback: it clears every conn's frozen flag
// (peers resume editing their live Y.Doc, viewers keep read-only) and releases
// appendMu, leaving the room exactly as it was before the restore attempt. A nil
// room is a no-op (nothing was frozen). MUST be called with appendMu held; it
// preserves the room.mu-inside-appendMu lock order.
func (m *RoomManager) unfreezeAndReleaseAppend(room *Room) {
	if room == nil {
		return
	}
	room.mu.Lock()
	for _, rc := range room.conns {
		rc.frozen.Store(false)
	}
	room.mu.Unlock()
	room.appendMu.Unlock()
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

// SetConnWritable updates the write permission of a single live
// connection mid-session. Called by the collab handler's periodic
// authorization revalidation when a still-authorized peer's edit
// permission changed — e.g. a workspace editor demoted to viewer (or
// a viewer promoted to editor) — so readLoop's per-frame gate
// reflects the current role without waiting for a reconnect. This is
// the write-side complement of CloseConn: a demotion that keeps SOME
// access downgrades the conn to read-only rather than evicting it.
//
// No-op when the conn is nil or the room / conn has already been torn
// down (a disconnect that raced the reval tick).
//
// The flag is an atomic.Bool AND the store happens under the room's
// appendMu — the same lock readLoop holds across its (now
// under-appendMu) canWrite check + persist. That fencing means a
// demotion can't interleave with an in-flight frame: readLoop either
// runs the whole check+persist before this store, or observes the new
// value and drops the frame. Room lookup (m.mu) and conn lookup
// (room.mu) are released before appendMu is taken, so no lock nesting
// is introduced. Per TASK-265.
func (m *RoomManager) SetConnWritable(itemID string, conn *websocket.Conn, canWrite bool) {
	if conn == nil {
		return
	}
	m.mu.Lock()
	room := m.rooms[itemID]
	m.mu.Unlock()
	if room == nil {
		return
	}
	room.mu.Lock()
	rc := room.conns[conn]
	room.mu.Unlock()
	if rc == nil {
		return
	}
	room.appendMu.Lock()
	rc.canWrite.Store(canWrite)
	room.appendMu.Unlock()
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
