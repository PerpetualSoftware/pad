package collab

import (
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
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
	// canWrite reports whether this connection may PERSIST inbound
	// sync frames. Non-editor participants (workspace viewers,
	// view-only guests) are admitted read-only — they still receive
	// live broadcasts + presence, but readLoop drops their inbound
	// sync frames (not persisted, not rebroadcast). Set at Join time
	// from the collab authorization decision and flipped live by the
	// manager's mid-session revalidation when the peer's edit
	// permission changes (editor⇄viewer). atomic.Bool so the
	// revalidation goroutine's write can't race readLoop's read.
	// Server-side mirror of the REST requireEditPermission gate
	// (TASK-265).
	canWrite atomic.Bool

	// frozen reports whether ForceRefreshRoom (version restore, BUG-2264) has
	// paused this connection's inbound sync persistence for the duration of a
	// prune+reseed. Kept SEPARATE from canWrite deliberately: the mid-session
	// auth revalidation loop writes canWrite freely, so overloading it as the
	// restore freeze let a revalidation tick thaw the freeze mid-restore
	// (Codex xhigh [P1]) and a failed restore silently promote a viewer to
	// writable. readLoop drops a sync frame when EITHER frozen or !canWrite.
	// Set under appendMu (so a readLoop already queued on appendMu observes it
	// on the very next check) and cleared only by ForceRefreshRoom's failure
	// path; on success the conn is force-closed instead. atomic.Bool so the
	// restore goroutine's write can't race readLoop's read.
	frozen atomic.Bool

	// evicted marks a read-only conn that a no-applier direct write has
	// outdated (BUG-2103): the op-log it synced from was pruned and
	// items.content replaced, so its in-memory Y.Doc is stale and it is being
	// force-refreshed. TERMINAL — nothing clears it; the conn is closed
	// instead. Deliberately not `frozen`: a restore's failure path un-freezes
	// every conn, which would thaw this one. And not `canWrite`, which the
	// auth revalidation loop rewrites. readLoop drops its sync frames and
	// pickApplier skips it, so a viewer promoted between the eviction and the
	// close can flip canWrite but can never persist its stale state. Set under
	// appendMu, the same fence readLoop checks it under.
	evicted atomic.Bool

	// lastPersistedOpID + frozenDropSeq are the DURABLE persistence-correlation
	// signals for the designated-applier round-trip (BUG-2276 residual 2). Both
	// are mutated ONLY by this conn's readLoop under appendMu:
	//   - lastPersistedOpID holds the op-log id of the last sync frame this conn
	//     PERSISTED. It advances only for frames that actually landed
	//     (AppendYjsUpdate returned id>0 while UNFROZEN), so it is a monotonic
	//     durable high-water for the conn.
	//   - frozenDropSeq counts sync frames this conn had DROPPED because it was
	//     frozen (a version restore froze it mid-apply). It advances only on a
	//     frozen drop, never on a viewer/canWrite drop.
	// The applier flow captures both at apply_start (the bracket baseline). A
	// restore's finalization reads lastPersistedOpID UNDER appendMu (atomic with
	// setting frozen) to decide, durably, whether the applier's setContent frame
	// landed BEFORE the freeze; the ack path reads frozenDropSeq to decide whether
	// it was dropped by a freeze. See restore_coord.go for the soundness argument.
	// atomic so the finalizing/ack goroutines can read them without appendMu.
	lastPersistedOpID atomic.Int64
	frozenDropSeq     atomic.Int64

	// bracketCapable reports whether this connection's client sends the
	// applier_apply_start bracket (BUG-2276 residual 2). Announced at Join via
	// `?applier_bracket=1`. pickApplier prefers capable conns; a round-trip elected
	// on a NON-capable (legacy) conn that a restore can't confirm resolves to an
	// AMBIGUOUS outcome (fail-safe: the external write is retried, never re-applied /
	// clobbered). Set once at Join; read on the election + finalization paths.
	bracketCapable atomic.Bool
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

// writeMessageWithDeadline writes one frame with a bounded write deadline, both
// the deadline set AND the write under writeMu — gorilla treats SetWriteDeadline
// as a write-side method, so doing it outside the mutex would race the writeLoop
// (BUG-2264 P2). Used by force_refresh so a slow/dead peer can't block the caller
// indefinitely while it holds the per-item lock.
func (rc *roomConn) writeMessageWithDeadline(messageType int, data []byte, deadline time.Time) error {
	rc.writeMu.Lock()
	defer rc.writeMu.Unlock()
	_ = rc.conn.SetWriteDeadline(deadline)
	err := rc.conn.WriteMessage(messageType, data)
	_ = rc.conn.SetWriteDeadline(time.Time{})
	return err
}

// Room is the per-item collab fan-out point. One Room per `itemID`
// at a time; created lazily by the RoomManager on first Join and
// reclaimed `graceTTL` after the last subscriber leaves.
type Room struct {
	itemID string
	// Who owns which awareness client IDs (TASK-2206), created on first use
	// so a Room built without the manager still works.
	awOnce sync.Once
	aw     *awarenessTracker
	// awPubMu orders each awareness decision with its publish: a relayed
	// frame's ownership check and its broadcast, and a closing connection's
	// release and its removal, happen as one step each, so the bus carries
	// them in the order ownership was decided (codex r2). Publish never
	// blocks (a slow subscriber is dropped), so holding it there is cheap.
	awPubMu sync.Mutex
	// afterAwarenessRelease runs between a release and its publish; tests
	// use it to hold that window open. Nil in production.
	afterAwarenessRelease func()
	// beforeAwarenessRelay runs as a relay is about to take awPubMu; tests
	// use it to know a racing frame has reached the lock. Nil in production.
	beforeAwarenessRelay func()
	store                opLogStore
	bus                  OpBus
	schemaVersion        string
	graceTTL             time.Duration
	onIdle               func(string) // RoomManager.markRoomGone

	mu    sync.Mutex
	conns map[*websocket.Conn]*roomConn
	// seeder is the one live connection allowed to run the client's
	// lazy seed (BUG-3240), or nil. Guarded by mu.
	seeder     *roomConn
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

	// --- version-restore ↔ designated-applier coordination (BUG-2276 residual 2) ---
	//
	// These serialise the designated-applier round-trip (ApplyExternalContent)
	// against an in-progress version restore (ForceRefreshRoom). A NEW round-trip
	// WAITS for an in-progress restore to resolve (commit OR rollback) before it
	// elects (enterApplierGate); an ALREADY in-flight round-trip is FINALIZED by the
	// restore under appendMu, atomic with the freeze, using the durable
	// persistence-correlation signal (see restore_coord.go + the roomConn atomics).
	// So an applier ack can never race the frozen window: the applier's fate is
	// decided from DURABLE op-log state, not timing.
	//
	// restoreMu is a LEAF lock: never held while acquiring appendMu / room.mu /
	// pendingMu, and never acquires them while held. Lock order stays itemLock →
	// appendMu → room.mu, with restoreMu orthogonal.
	restoreMu sync.Mutex
	// restoreActive is true from beginRestore (before the freeze) until resolveRestore
	// (after commit/rollback/uncertain). While true, new applier round-trips block in
	// enterApplierGate.
	restoreActive bool
	// restoreResolved is closed by resolveRestore when the active restore finishes;
	// nil when no restore is in progress. Gate-blocked / finalized-and-parked appliers
	// wait on it.
	restoreResolved chan struct{}
	// admittedUnregistered counts round-trips that passed enterApplierGate but have
	// NOT yet finished registering their pending-ack entry (the enter→register gap).
	// beginRestore drains this to 0 BEFORE the freeze scan so no admitted round-trip
	// is missed by freezeAndFinalizePending (BUG-2276 residual 2, P1). The gap is
	// mutex-only (pickApplier + registerPendingAck, no I/O), so the drain is
	// microseconds — never the applier round-trip. restoreCond signals it reaching 0.
	restoreCond          *sync.Cond
	admittedUnregistered int
}

// opLogStore is the store surface a Room needs. Pulling it into a
// narrow interface lets manager_test stub op-log behaviour without
// dragging in the entire *store.Store API surface.
type opLogStore interface {
	// AppendSyncFrame persists one relay frame, or skips it when it is a
	// byte-identical duplicate of an earlier row (BUG-3135). Either way the
	// returned ID is what the frame is acknowledged with.
	AppendSyncFrame(itemID string, data []byte, schemaVersion string) (store.SyncFrameAppend, error)
	// AppendSyncFrames is AppendSyncFrame for a run of frames in one
	// transaction, all or nothing, one result per frame in order (BUG-3253).
	AppendSyncFrames(itemID string, frames [][]byte, schemaVersion string) ([]store.SyncFrameAppend, error)
	LoadYjsUpdatesSince(itemID string, sinceID int64) ([]models.YjsUpdate, error)
	// LatestYjsUpdateSchemaVersion + SetAsideAndClearOpLog power the
	// schema-mismatch rebuild (TASK-1268). The room manager checks the most
	// recent op-log row's stamp on Join and empties the item's op-log when
	// the persisted version no longer matches the server's SCHEMA_VERSION.
	// SetAsideAndClearOpLog moves the unflushed content-bearing rows to the
	// set-aside table in the same transaction (BUG-3244), so edits the row
	// never received are kept, and marked, rather than deleted.
	LatestYjsUpdateSchemaVersion(itemID string) (string, int64, bool, error)
	SetAsideAndClearOpLog(itemID string) (setAside, cleared int64, err error)
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
	MinOpLogID(itemID string) (int64, bool, error)
	// MaxOpLogID is consulted ONLY by ForceRefreshRoom (BUG-2264), which reads
	// it under the per-item lock WITH appendMu held (inbound persistence frozen)
	// to set the pre-prune restore boundary = MAX+1. That fencing is why the
	// broadcast-vs-MAX race (Codex round 6 [P1] of TASK-1319) — which kept this
	// off the initial-cursor path — does not apply here: no frame can be persisted
	// but not yet delivered, because appends are frozen while we read.
	MaxOpLogID(itemID string) (int64, bool, error)
	// ItemLastRestoreSeq returns the DURABLE per-item restore boundary
	// (items.last_restore_seq) — the item.seq of the most recent version restore
	// (BUG-2264). Join consults it to force_refresh a client whose ?content_seq
	// seed predates the last restore; being persisted, it fences a stale-seeded
	// cursor-0 tab that reconnects AFTER a server restart (when the in-memory
	// lastRestoreSeqs fast-path is empty). (0, false) = never restored.
	ItemLastRestoreSeq(itemID string) (int64, bool, error)
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

	// The presence this connection carried leaves with it (TASK-2206): peers
	// drop its carets now instead of at y-protocols' 30-second timeout. After
	// Unsubscribe, and published under this conn's id, so it reaches every
	// peer and not the closed socket.
	r.releaseAwareness(rc)

	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.conns, rc.conn)
	if r.seeder == rc {
		r.seeder = nil
		// Pass the role on, so a peer still waiting on an empty doc is
		// not left blank. Published on the bus AFTER Unsubscribe above
		// and after this conn's readLoop returned, so every op it sent
		// is already queued ahead of the grant in each peer's channel.
		if next := r.pickSeederLocked(); next != nil {
			r.seeder = next
			r.bus.Publish(OpEvent{ItemID: r.itemID, Type: OpTypeSeedGrant, TargetClientID: next.id})
		}
	}
	if len(r.conns) == 0 && r.graceTimer == nil && !r.closing {
		r.graceTimer = time.AfterFunc(r.graceTTL, r.onGraceExpired)
	}
}

// pickSeederLocked returns a live connection that may write, or nil.
// Caller holds mu.
func (r *Room) pickSeederLocked() *roomConn {
	for _, c := range r.conns {
		if c.canWrite.Load() {
			return c
		}
	}
	return nil
}

// claimSeeder makes rc the room's seeder if no live connection holds the
// role (BUG-3240). Called once per connection, under the per-item setup
// lock, before its initial op_log_cursor frame. The client seeds only if
// it holds the role AND its Y.Doc is still empty after the replay, so the
// grant says nothing about the op-log's contents and needs no query.
func (r *Room) claimSeeder(rc *roomConn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seeder != nil || !rc.canWrite.Load() {
		return false
	}
	if _, live := r.conns[rc.conn]; !live {
		return false
	}
	r.seeder = rc
	return true
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
// maxSyncBatch bounds how many waiting frames one run takes (BUG-3253). A
// typed keystroke is TWO frames, its sync update and the cursor's awareness
// update, so this is about 64 keystrokes; past it the next run takes the rest.
const maxSyncBatch = 128

// inboundFrame is one ReadMessage result, handed from the socket reader to
// readLoop in order.
type inboundFrame struct {
	msgType int
	data    []byte
	err     error
}

// readLoop is the inbound side: pull frames off the WebSocket, route
// sync frames through the op-log + broadcast, awareness frames to
// broadcast only. Returns when the WS read returns an error
// (close frame or transport failure). The caller then runs
// removeConn and waits for writeLoop to exit.
//
// BUG-3253: a goroutine reads the socket (it is the connection's only
// reader) and hands frames over in order, so readLoop can see which sync
// frames are already waiting and persist them together. It used to commit
// one op-log row per frame before reading the next, so a typed burst
// drained at one store commit per keystroke. The reader exits on its first
// error, which readLoop returns after handling every frame before it.
func (r *Room) readLoop(rc *roomConn) error {
	frames := make(chan inboundFrame, maxSyncBatch)
	go func() {
		for {
			msgType, data, err := rc.conn.ReadMessage()
			frames <- inboundFrame{msgType: msgType, data: data, err: err}
			if err != nil {
				return
			}
		}
	}()
	var held *inboundFrame
	for {
		var f inboundFrame
		if held != nil {
			f, held = *held, nil
		} else {
			f = <-frames
		}
		if f.err != nil {
			return f.err
		}
		msgType, data := f.msgType, f.data

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
			// Take every sync and awareness frame already waiting, in order.
			// Awareness rides along because every keystroke sends one right
			// behind its sync update: stopping at it (the first version of
			// this) kept every run at one frame, measured. Anything else (a
			// control message, an error) ends the run and is handled next,
			// so the order of everything is kept.
			batch := [][]byte{data}
		drain:
			for len(batch) < maxSyncBatch {
				select {
				case g := <-frames:
					if g.err == nil && g.msgType == websocket.BinaryMessage && len(g.data) > 0 &&
						(g.data[0] == yMessageSync || g.data[0] == yMessageAwareness) {
						batch = append(batch, g.data)
						continue
					}
					held = &g
					break drain
				default:
					break drain
				}
			}
			r.persistSyncFrames(rc, batch)

		case yMessageAwareness:
			// Awareness is presence — ephemeral. Never persisted.
			r.relayAwareness(rc, data)

		default:
			// Unknown y-protocol message types (custom extensions,
			// future revisions). Silently drop — logging at debug
			// would spam the operator under any client misbehaviour.
		}
	}
}

// persistSyncFrames persists and publishes a run of frames read from one
// connection (BUG-3253): a sync frame and every sync or awareness frame
// already waiting behind it. The sync frames persist in one transaction;
// then every frame is published in the order it arrived. Awareness frames
// are never persisted and are relayed even when the sync frames are not.
func (r *Room) persistSyncFrames(rc *roomConn, run [][]byte) {
	var batch [][]byte
	for _, f := range run {
		if f[0] == yMessageSync {
			batch = append(batch, f)
		}
	}
	// Hold appendMu across the persist+publish sequence so we
	// uphold TASK-1252's single-writer-per-item contract. The
	// dumb-relay design intends one writer per Room, but each
	// peer has its own readLoop — without serialisation here,
	// two peers' sync frames would race AppendYjsUpdate and
	// could surface a Postgres cursor gap (allocation order ≠
	// commit order). awareness frames and OTHER rooms are
	// unaffected by this lock.
	r.appendMu.Lock()
	// Read-only participants (workspace viewers / view-only
	// guests, TASK-265) are admitted for live view + presence
	// but MUST NOT mutate content. Drop their inbound sync
	// frames — these are the frames that would otherwise persist
	// to item_yjs_updates and get canonicalized into
	// items.content by a co-present editor's authorized flush.
	// Awareness (presence) frames below are still relayed so the
	// viewer's cursor stays visible to editors. This is the
	// collab-side mirror of the REST requireEditPermission gate.
	//
	// The canWrite check is INSIDE appendMu and SetConnWritable
	// takes appendMu when flipping the flag, so a demotion that
	// races an in-flight frame can't slip between the check and
	// the persist: either the frame's whole persist runs before
	// the demotion, or the demotion is observed and the frame is
	// dropped. Without this fencing a reader could read
	// canWrite=true, block on appendMu, and persist AFTER
	// SetConnWritable(false) returned (TOCTOU).
	//
	// The `frozen` check shares the same appendMu fence and closes
	// the restore-prune window (BUG-2264): a readLoop that already
	// read a frame and is queued on appendMu when ForceRefreshRoom
	// sets frozen=true will, on acquiring appendMu, drop the frame
	// instead of appending it AFTER the prune (which would survive as
	// a stale post-boundary op). It is a separate flag from canWrite
	// so the auth revalidation loop can't thaw it mid-restore.
	if rc.frozen.Load() {
		// Frozen by an in-progress version restore: this frame is dropped.
		// Record the drop (BUG-2276 residual 2) so the applier flow's ack
		// path can tell, durably, that a frame in this conn's apply bracket
		// was dropped by the freeze → the external content did NOT land.
		rc.frozenDropSeq.Add(int64(len(batch)))
		r.publishAwareness(rc, run)
		r.appendMu.Unlock()
		return
	}
	if !rc.canWrite.Load() || rc.evicted.Load() {
		r.publishAwareness(rc, run)
		r.appendMu.Unlock()
		return
	}
	// Persist before broadcast so a server crash between
	// persist and broadcast loses at most a live keystroke
	// that the originating peer will replay on reconnect
	// anyway.
	// BUG-3135: a byte-identical re-send is not stored, but it is
	// broadcast and acknowledged exactly like a stored frame. Its ack
	// id is the item's current MAX(id), read under appendMu: every row
	// at or below it was published to each conn's bus before this
	// event, so the cursor cannot overtake an undelivered binary (the
	// round-23 hazard below).
	//
	// BUG-3253: the frames of a burst that are already waiting are persisted
	// in ONE transaction (AppendSyncFrames), each with exactly the treatment
	// it had alone, and published below one by one in order, each with its
	// own op-log id. All or nothing: a failed batch publishes every frame
	// with id 0, as one failed frame always was.
	results, err := r.store.AppendSyncFrames(r.itemID, batch, r.schemaVersion)
	if err == nil && len(results) != len(batch) {
		// The contract is one result per frame. A store that answered
		// otherwise is treated as a failed batch rather than indexed past
		// its end (codex r2).
		err = errors.New("AppendSyncFrames did not return one result per frame")
	}
	if err != nil {
		slog.Error("collab: append op-log",
			"item_id", r.itemID,
			"client_id", rc.id,
			"frames", len(batch),
			"error", err,
		)
		// Continue: broadcast keeps the live mesh consistent
		// even when persistence is blipping. persistedID == 0
		// → no cursor frame is emitted by writeLoop for this
		// event (we'd be advertising a fictional id).
		results = make([]store.SyncFrameAppend, len(batch))
	}
	i := 0
	for _, data := range run {
		if data[0] != yMessageSync {
			r.relayAwareness(rc, data)
			continue
		}
		appended := results[i]
		i++
		persistedID := appended.ID
		if appended.Persisted && persistedID > 0 {
			// Advance the conn's durable high-water (BUG-2276 residual 2). Only
			// frames that actually landed advance it, so a restore's finalization
			// can read it (under appendMu) to know this conn's applier frame
			// persisted BEFORE the freeze. Still under appendMu, so it is ordered
			// with the finalization's read.
			rc.lastPersistedOpID.Store(persistedID)
		}
		r.bus.Publish(OpEvent{
			ItemID:   r.itemID,
			ClientID: rc.id,
			Type:     OpTypeSync,
			Data:     data,
			OpLogID:  persistedID,
		})
	}
	r.appendMu.Unlock()
	// Originator cursor delivery is handled by writeLoop:
	// it processes the self event from rc.bus, skips the
	// binary echo, and emits the cursor frame for OpLogID.
	// Sending the cursor from here (the original design)
	// could overtake older peer ops queued in rc.bus and
	// let the client persist a cursor past undelivered
	// binaries — the round-23 [P1] hazard.
}

// publishAwareness relays a run's awareness frames, in order, when its sync
// frames are not persisted (BUG-3253): presence is relayed for every
// connection, read-only and frozen ones included, as it always was.
func (r *Room) publishAwareness(rc *roomConn, run [][]byte) {
	for _, data := range run {
		if data[0] == yMessageAwareness {
			r.relayAwareness(rc, data)
		}
	}
}

func (r *Room) awareness() *awarenessTracker {
	r.awOnce.Do(func() { r.aw = newAwarenessTracker() })
	return r.aw
}

// relayAwareness broadcasts an awareness frame unchanged, after noting which
// client IDs this connection owns (TASK-2206; see awareness.go). Every
// awareness frame the room relays goes through here.
func (r *Room) relayAwareness(rc *roomConn, data []byte) {
	if r.beforeAwarenessRelay != nil {
		r.beforeAwarenessRelay()
	}
	r.awPubMu.Lock()
	defer r.awPubMu.Unlock()
	r.awareness().observe(rc.id, data)
	r.bus.Publish(OpEvent{ItemID: r.itemID, ClientID: rc.id, Type: OpTypeAwareness, Data: data})
}

// releaseAwareness forgets a closing connection's clients and tells the room
// they are gone. Under awPubMu with relayAwareness: a reconnected client's
// first frame is either observed before this (it takes ownership by its newer
// clock, so nothing is removed) or published after this removal (so peers see
// the removal first and the live state last). Without the lock a live state
// could be overtaken by a removal at the same clock, which y-protocols applies.
func (r *Room) releaseAwareness(rc *roomConn) {
	r.awPubMu.Lock()
	defer r.awPubMu.Unlock()
	gone := r.awareness().release(rc.id)
	if r.afterAwarenessRelease != nil {
		r.afterAwarenessRelease()
	}
	if len(gone) > 0 {
		r.bus.Publish(OpEvent{ItemID: r.itemID, ClientID: rc.id, Type: OpTypeAwareness, Data: encodeAwarenessRemoval(gone)})
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
		if ev.Type == OpTypeSeedGrant {
			if ev.TargetClientID != rc.id {
				continue
			}
			payload, _ := json.Marshal(ControlMessage{Type: ControlMessageSeedGrant})
			rc.writeMu.Lock()
			err := rc.conn.WriteMessage(websocket.TextMessage, payload)
			rc.writeMu.Unlock()
			if err != nil {
				_ = rc.conn.Close()
				return
			}
			continue
		}
		isSelf := ev.ClientID == rc.id
		// `isSelf`: we skip the binary echo (the originator already
		// has the Y.Doc state) but STILL process the cursor logic
		// below. Routing the originator's cursor through the bus
		// FIFO is what enforces "older peer ops dispatched first."
		// Sending it from readLoop directly (the previous design)
		// could overtake a peer op already buffered in this conn's
		// rc.bus, letting the client persist a cursor past
		// undelivered binaries. Per Codex round 23 [P1] of TASK-1319.

		// Hold writeMu across the entire per-event sequence —
		// binary frame, replayDone observation, AND either cursor
		// send (replayDone) or maxLive record (!replayDone). With
		// the same lock used by runConn's post-replay cursor
		// write, runConn observes either:
		//   - this event's record (we ran first), OR
		//   - this event's send (replayDone flipped before we
		//     got the lock, so we publish a normal live cursor).
		// Without the wider lock, runConn could read maxLive
		// AFTER we wrote the binary but BEFORE we recorded — the
		// record would be lost. Per Codex round 22 [P1] of
		// TASK-1319.
		rc.writeMu.Lock()
		if !isSelf {
			if err := rc.conn.WriteMessage(websocket.BinaryMessage, ev.Data); err != nil {
				rc.writeMu.Unlock()
				_ = rc.conn.Close()
				return
			}
		}
		if ev.Type == OpTypeSync && ev.OpLogID > 0 {
			if rc.replayDone.Load() {
				payload, perr := json.Marshal(ControlMessage{
					Type:    ControlMessageOpLogCursor,
					OpLogID: ev.OpLogID,
				})
				if perr != nil {
					slog.Warn("collab: marshal op_log_cursor failed",
						"item_id", r.itemID,
						"client_id", rc.id,
						"error", perr,
					)
				} else if werr := rc.conn.WriteMessage(websocket.TextMessage, payload); werr != nil {
					rc.writeMu.Unlock()
					_ = rc.conn.Close()
					return
				}
			} else {
				// CAS-loop into the suppressed-cursor max so
				// runConn's read after replay sees the latest
				// id. Held under writeMu so an external read
				// won't observe a stale value while we're still
				// in this critical section.
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
		rc.writeMu.Unlock()
	}
}

// handleControlMessage dispatches a TextMessage frame received from a peer:
// applier_apply_start / applier_ack (the designated-applier bracket, TASK-1257 +
// BUG-2276 residual 2). Unknown types are dropped silently so a future client
// extension can add new control types without older servers blowing up.
func (r *Room) handleControlMessage(rc *roomConn, data []byte) {
	var ctl ControlMessage
	if err := json.Unmarshal(data, &ctl); err != nil {
		// Malformed JSON — drop. We don't log at warn here because
		// a bad client could otherwise spam logs by sending
		// arbitrary text frames.
		return
	}
	switch ctl.Type {
	case ControlMessageApplierApplyStart:
		if ctl.RequestID == "" {
			return
		}
		// A read-only participant can never be a legitimate applier (pickApplier
		// excludes non-writers), so ignore a bracket-start from one.
		if !rc.canWrite.Load() {
			return
		}
		// Open the durable persistence bracket for this request (BUG-2276
		// residual 2). The client sends apply_start IMMEDIATELY before setContent
		// (synchronously), so capturing this conn's high-water + frozen-drop count
		// now means the bracket contains EXACTLY the setContent frame — attribution
		// is airtight against concurrent typing. Processed in readLoop order (before
		// the setContent frame), even while frozen (control frames are never
		// frozen-dropped). See restore_coord.go.
		r.startApplierApply(ctl.RequestID, rc)
	case ControlMessageApplierAck:
		if ctl.RequestID == "" {
			return
		}
		// A read-only participant can never be a legitimate applier —
		// pickApplier excludes non-writers — so an applier_ack from a
		// conn whose canWrite is false MUST be ignored (TASK-265).
		// Otherwise a viewer (or an editor demoted mid-apply) could
		// report a phantom success while its resulting sync frames were
		// dropped by the read-only gate, making ApplyExternalContent
		// return nil, skip the PATCH handler's direct-write fallback,
		// and silently lose the external edit.
		//
		// A FROZEN conn is NO LONGER dropped here (BUG-2276 residual 2). The old
		// design dropped a frozen conn's ack because it couldn't tell whether the
		// applier's frames had persisted. Now the durable bracket answers that: the
		// ack path resolves the round-trip to SUCCESS only if no frame was
		// frozen-dropped in its apply bracket (frozenDropSeq unchanged), and to
		// RETRY otherwise — so a frozen conn whose setContent was dropped correctly
		// resolves to retry, and one whose setContent already persisted before the
		// freeze correctly resolves to success. No timing, no phantom success.
		if !rc.canWrite.Load() {
			slog.Warn("collab: applier_ack from read-only conn; ignoring",
				"item_id", r.itemID,
				"client_id", rc.id,
			)
			return
		}
		r.resolveApplierAck(ctl.RequestID, rc)
	case ControlMessageSyncSafetyNet:
		// BUG-3240: this connection never saw its post-replay cursor
		// frame. Should be unreachable; logged so it is not silent.
		slog.Warn("collab: client sync safety net fired (no post-replay op_log_cursor)",
			"item_id", r.itemID,
			"client_id", rc.id,
		)
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

// closeAllConnsPlain force-closes every connection on this room with a PLAIN
// socket Close — no force_refresh frame, and the room is left ALIVE (not marked
// closing) so a reconnect rejoins normally. Used by ForceRefreshRoom's UNCERTAIN
// commit-outcome path (BUG-2276 residual 1): when a Postgres restore commit
// errors and reconciliation cannot tell whether the tx landed, we must NOT tell
// peers to reseed from items.content (a force_refresh would assert the content is
// authoritative, which is exactly what we don't know). We also must not leave the
// peers frozen-but-open: the collab read path has no WS read-deadline/heartbeat,
// so a frozen conn would silently drop every subsequent edit forever. A plain
// close makes each client reconnect and re-evaluate the DURABLE restore fences
// (items.last_restore_seq / restore_boundary_op_id) fresh through Join, which is
// safe whichever way the commit actually went. conn.Close is concurrency-safe
// with an in-flight WriteMessage (gorilla), so no writeMu handshake is needed;
// each conn's frozen flag is left set so any frame already read is still dropped
// before the readLoop unwinds. Mirrors closeAll's collect-under-lock /
// close-outside-lock discipline.
func (r *Room) closeAllConnsPlain() {
	r.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(r.conns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
}

// forceRefreshAll sends a force_refresh control frame to every connected client
// and then closes each connection, so every peer discards its in-memory Y.Doc
// and rebuilds from the canonical items.content on reconnect (BUG-2264). Used by
// RoomManager.ForceRefreshRoom after the op-log has been pruned. The frame goes
// through rc.writeMessage (holds writeMu) so it can't race the live writeLoop's
// concurrent write — sendForceRefreshFrame's bare conn.WriteMessage is only safe
// pre-addConn. Snapshots the conn set under r.mu, then does socket I/O OUTSIDE
// the lock (matching closeAll) so a slow write/close can't block the room's
// addConn/removeConn.
func (r *Room) forceRefreshAll() {
	r.mu.Lock()
	conns := make([]*roomConn, 0, len(r.conns))
	for _, rc := range r.conns {
		conns = append(conns, rc)
	}
	r.mu.Unlock()
	r.forceRefreshConns(conns)
}

// forceRefreshConns sends force_refresh then closes each of conns, with the
// same per-conn bound forceRefreshAll documents. The caller must NOT hold
// appendMu: this is socket I/O.
func (r *Room) forceRefreshConns(conns []*roomConn) {
	payload, err := json.Marshal(ControlMessage{Type: ControlMessageForceRefresh})
	if err != nil {
		slog.Warn("collab: marshal force_refresh frame failed", "item_id", r.itemID, "error", err)
		payload = nil
	}

	// Fan the frame+close out per conn concurrently (additional-P2): a slow peer
	// already inside a deadline-free WriteMessage would otherwise block the whole
	// loop — and the caller still holds the per-item lock, stalling that item's
	// restore/joins/snapshots.
	//
	// Each conn's frame is written with a bounded write deadline, but the deadline
	// is set INSIDE writeMu (gorilla treats SetWriteDeadline as a write op), so it
	// can't bound the wait to ACQUIRE writeMu. If the writeLoop is wedged in a
	// deadline-free WriteMessage to a dead peer it holds writeMu indefinitely and
	// writeMessageWithDeadline would block on writeMu.Lock() forever, hanging
	// wg.Wait while the caller holds the per-item lock (BUG-2264 P1, Codex xhigh).
	// So arm an independent timer per conn that Closes it after the deadline:
	// conn.Close is concurrency-safe with an in-flight WriteMessage (gorilla), so
	// it forces the wedged write to error, releasing writeMu and unblocking us.
	var wg sync.WaitGroup
	deadline := time.Now().Add(closeFrameDeadline)
	for _, rc := range conns {
		wg.Add(1)
		go func(rc *roomConn) {
			defer wg.Done()
			// The timer bounds writeMu acquisition even if the frame write itself
			// never gets to run its own deadline; Close is idempotent so the
			// explicit Close below is harmless if the timer already fired.
			t := time.AfterFunc(closeFrameDeadline, func() { _ = rc.conn.Close() })
			if payload != nil {
				_ = rc.writeMessageWithDeadline(websocket.TextMessage, payload, deadline)
			}
			t.Stop()
			_ = rc.conn.Close()
		}(rc)
	}
	wg.Wait()
}

// connIDCounter is package-scoped so multiple RoomManagers in the
// same process never hand out colliding ids. Atomic Add returns a
// fresh value per call; we never reset.
var connIDCounter atomic.Uint64

func nextConnID() uint64 {
	return connIDCounter.Add(1)
}
