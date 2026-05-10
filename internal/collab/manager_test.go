package collab

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/gorilla/websocket"
)

// fakeOpLog is an in-memory opLogStore. We don't pull in the real
// store here because the room-manager contract is narrow and the
// store dependency would drag in SQLite + migrations for what is
// fundamentally a goroutine-orchestration test.
type fakeOpLog struct {
	mu      sync.Mutex
	rows    []models.YjsUpdate
	nextID  int64
	failOn  string // if non-empty, AppendYjsUpdate returns this as an error message for any row
	appendN int

	// contentFlushedIDs simulates the items.content_flushed_op_log_id
	// watermark per item (TASK-1309 round 4). Tests populate this
	// to exercise the schema-mismatch + sweeper paths that consult
	// it. Missing key = NULL watermark = "never flushed".
	contentFlushedIDs map[string]int64

	// onListDormantHook fires once per call to
	// ListDormantOpLogItemsBefore, AFTER the listing scan but BEFORE
	// the result is returned to the caller. Used by tests that
	// simulate a row being appended in the race window between
	// "candidate query returns" and "PruneItemOpLogIfDormantBefore
	// runs under the per-item lock". The hook receives the locked
	// store so it can mutate rows directly. nil = no-op.
	//
	// Per Codex review of TASK-1309 round 2 NIT:
	// TestPruneSweepSkipsRowAddedMidSweep wasn't actually exercising
	// the conditional-DELETE path because it inserted the recent row
	// before the listing query.
	onListDormantHook func(f *fakeOpLog)
}

func (f *fakeOpLog) AppendYjsUpdate(itemID string, data []byte, schemaVersion string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendN++
	if f.failOn != "" {
		return 0, errors.New(f.failOn)
	}
	f.nextID++
	cp := make([]byte, len(data))
	copy(cp, data)
	row := models.YjsUpdate{
		ID:            f.nextID,
		ItemID:        itemID,
		UpdateData:    cp,
		SchemaVersion: schemaVersion,
		CreatedAt:     time.Now().UTC(),
	}
	f.rows = append(f.rows, row)
	return row.ID, nil
}

func (f *fakeOpLog) LoadYjsUpdatesSince(itemID string, sinceID int64) ([]models.YjsUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []models.YjsUpdate
	for _, r := range f.rows {
		if r.ItemID == itemID && r.ID > sinceID {
			out = append(out, r)
		}
	}
	return out, nil
}

// LatestYjsUpdateSchemaVersion mirrors the production store: returns
// the most recent op-log row's schema_version AND id for an item, or
// (false) when no rows exist. The fake walks rows[] in append order
// and returns the latest matching entry — same semantic as the
// store's `ORDER BY id DESC LIMIT 1`.
func (f *fakeOpLog) LatestYjsUpdateSchemaVersion(itemID string) (string, int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.rows) - 1; i >= 0; i-- {
		if f.rows[i].ItemID == itemID {
			return f.rows[i].SchemaVersion, f.rows[i].ID, true, nil
		}
	}
	return "", 0, false, nil
}

// GetItemContentFlushedOpLogID returns the per-item watermark as
// configured by the test via fakeOpLog.contentFlushedIDs (a map
// itemID → id). Missing key = NULL watermark, returned as (0, false).
func (f *fakeOpLog) GetItemContentFlushedOpLogID(itemID string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.contentFlushedIDs == nil {
		return 0, false, nil
	}
	v, ok := f.contentFlushedIDs[itemID]
	return v, ok, nil
}

// PruneYjsUpdatesBefore deletes every row for itemID whose CreatedAt
// is strictly less than the cutoff. The schema-mismatch rebuild path
// passes a far-future cutoff so this becomes "delete every row".
func (f *fakeOpLog) PruneYjsUpdatesBefore(itemID string, before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.rows[:0]
	var pruned int64
	for _, r := range f.rows {
		if r.ItemID == itemID && r.CreatedAt.Before(before) {
			pruned++
			continue
		}
		kept = append(kept, r)
	}
	f.rows = kept
	return pruned, nil
}

// ListDormantOpLogItemsBefore returns item_ids whose entire op-log
// is older than the cutoff. Mirrors the production store's
// `GROUP BY item_id HAVING MAX(created_at) < ?`. The fake doesn't
// model the items.content_flushed_op_log_id watermark — tests that need it
// run against the real store via the server-level GC test.
func (f *fakeOpLog) ListDormantOpLogItemsBefore(before time.Time) ([]string, error) {
	f.mu.Lock()
	// Compute MAX(created_at) per item_id; collect items where MAX < cutoff.
	maxByItem := map[string]time.Time{}
	for _, r := range f.rows {
		if cur, ok := maxByItem[r.ItemID]; !ok || r.CreatedAt.After(cur) {
			maxByItem[r.ItemID] = r.CreatedAt
		}
	}
	var ids []string
	for id, mx := range maxByItem {
		if mx.Before(before) {
			ids = append(ids, id)
		}
	}
	hook := f.onListDormantHook
	f.mu.Unlock()
	// Fire the test hook (if any) AFTER the listing has been
	// computed but BEFORE returning, simulating a row appended in
	// the candidate-query→DELETE race window.
	if hook != nil {
		hook(f)
	}
	return ids, nil
}

// PruneItemOpLogIfDormantBefore atomically deletes every row for
// itemID iff no row exists with CreatedAt >= cutoff. Returns the
// count deleted.
func (f *fakeOpLog) PruneItemOpLogIfDormantBefore(itemID string, before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// First pass: is the item still dormant?
	for _, r := range f.rows {
		if r.ItemID == itemID && !r.CreatedAt.Before(before) {
			// A recent row exists — abort, don't delete anything.
			return 0, nil
		}
	}
	// Second pass: delete every row for this item.
	kept := f.rows[:0]
	var deleted int64
	for _, r := range f.rows {
		if r.ItemID == itemID {
			deleted++
			continue
		}
		kept = append(kept, r)
	}
	f.rows = kept
	return deleted, nil
}

// MinOpLogID + MaxOpLogID power TASK-1319's resume-cursor protocol.
func (f *fakeOpLog) MinOpLogID(itemID string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var minID int64
	found := false
	for _, r := range f.rows {
		if r.ItemID != itemID {
			continue
		}
		if !found || r.ID < minID {
			minID = r.ID
			found = true
		}
	}
	return minID, found, nil
}

func (f *fakeOpLog) MaxOpLogID(itemID string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var maxID int64
	found := false
	for _, r := range f.rows {
		if r.ItemID != itemID {
			continue
		}
		if !found || r.ID > maxID {
			maxID = r.ID
			found = true
		}
	}
	return maxID, found, nil
}

func (f *fakeOpLog) appendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appendN
}

func (f *fakeOpLog) rowCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// newCollabTestServer wires up an httptest server whose only handler
// upgrades the inbound WS, registers it with the manager, and waits
// for the session to end. Tests dial it via newWSClient below.
func newCollabTestServer(t *testing.T, mgr *RoomManager) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/", func(w http.ResponseWriter, r *http.Request) {
		itemID := r.URL.Path[len("/ws/"):]
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		// `?since=N` forwards the resume cursor into Join (TASK-1319).
		// Empty / unparseable → 0, matching the production handler.
		var since int64
		if raw := r.URL.Query().Get("since"); raw != "" {
			if v, perr := strconv.ParseInt(raw, 10, 64); perr == nil && v > 0 {
				since = v
			}
		}
		_ = mgr.Join(itemID, conn, since)
	})
	return httptest.NewServer(mux)
}

func dialWS(t *testing.T, server *httptest.Server, itemID string) *websocket.Conn {
	t.Helper()
	return dialWSSince(t, server, itemID, 0)
}

// dialWSSince is the resume-cursor variant: appends `?since=<id>` to
// the WS URL so the test exercises the TASK-1319 force_refresh path.
func dialWSSince(t *testing.T, server *httptest.Server, itemID string, since int64) *websocket.Conn {
	t.Helper()
	u, _ := url.Parse(server.URL)
	wsURL := "ws://" + u.Host + "/ws/" + itemID
	if since > 0 {
		wsURL += "?since=" + strconv.FormatInt(since, 10)
	}
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func sendSync(t *testing.T, c *websocket.Conn, payload byte) {
	t.Helper()
	// Build a minimal y-protocol sync frame: leading 0x00 for "sync",
	// followed by an arbitrary trailing byte that distinguishes one
	// frame from another in test assertions.
	if err := c.WriteMessage(websocket.BinaryMessage, []byte{yMessageSync, payload}); err != nil {
		t.Fatalf("write sync: %v", err)
	}
}

func sendAwareness(t *testing.T, c *websocket.Conn, payload byte) {
	t.Helper()
	if err := c.WriteMessage(websocket.BinaryMessage, []byte{yMessageAwareness, payload}); err != nil {
		t.Fatalf("write awareness: %v", err)
	}
}

// readBinaryWithin reads a single binary frame within d. Returns the
// payload bytes or fails the test on timeout.
func readBinaryWithin(t *testing.T, c *websocket.Conn, d time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(d)
	c.SetReadDeadline(deadline)
	defer c.SetReadDeadline(time.Time{})
	for {
		mt, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		// Drain TASK-1319 op_log_cursor TextMessage frames so the
		// existing binary-frame assertions stay focused on Y.Doc
		// payload semantics.
		if mt == websocket.TextMessage {
			c.SetReadDeadline(deadline)
			continue
		}
		if mt != websocket.BinaryMessage {
			t.Fatalf("expected binary frame, got %d", mt)
		}
		return data
	}
}

// expectNoMessage checks that no NON-cursor frame arrives within d.
// Cursor TextMessage frames (TASK-1319) are drained without failing
// the assertion — they're metadata, not Y.Doc payload — and the
// caller's intent is "no peer ops leaked through."
func expectNoMessage(t *testing.T, c *websocket.Conn, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	c.SetReadDeadline(deadline)
	defer c.SetReadDeadline(time.Time{})
	for {
		mt, data, err := c.ReadMessage()
		if err != nil {
			return // timeout / close — exactly what we expected
		}
		if mt == websocket.TextMessage {
			c.SetReadDeadline(deadline)
			continue
		}
		t.Fatalf("expected no payload frame, got mt=%d data=%v", mt, data)
	}
}

// TestRoomManagerLazyCreate verifies the manager doesn't materialise
// rooms until the first Join.
func TestRoomManagerLazyCreate(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)

	if got := mgr.RoomCount(); got != 0 {
		t.Fatalf("fresh manager: want 0 rooms, got %d", got)
	}

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	c := dialWS(t, srv, "item-a")
	defer c.Close()

	// Wait for the join to register.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.RoomCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := mgr.RoomCount(); got != 1 {
		t.Fatalf("after first Join: want 1 room, got %d", got)
	}
}

// TestRoomManagerOpLogReplay confirms a fresh peer receives every
// previously-persisted op-log row before any live frames.
func TestRoomManagerOpLogReplay(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()

	store := &fakeOpLog{}
	// Pre-seed the op-log with two rows for item-a. Use the y-sync
	// header (0x00) so the room treats them as sync frames if they
	// were ever fed back through the read path.
	if _, err := store.AppendYjsUpdate("item-a", []byte{yMessageSync, 0x01}, "1"); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if _, err := store.AppendYjsUpdate("item-a", []byte{yMessageSync, 0x02}, "1"); err != nil {
		t.Fatalf("seed 2: %v", err)
	}

	mgr := NewRoomManager(store, bus)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	c := dialWS(t, srv, "item-a")
	defer c.Close()

	first := readBinaryWithin(t, c, 2*time.Second)
	second := readBinaryWithin(t, c, 2*time.Second)

	if len(first) < 2 || first[1] != 0x01 {
		t.Errorf("replay row 1 mismatch: got %v", first)
	}
	if len(second) < 2 || second[1] != 0x02 {
		t.Errorf("replay row 2 mismatch: got %v", second)
	}
}

// TestRoomManagerSyncBroadcastAndPersist drives a sync frame from
// peer A and confirms B receives it AND the op-log gained a row.
func TestRoomManagerSyncBroadcastAndPersist(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	a := dialWS(t, srv, "item-a")
	defer a.Close()
	b := dialWS(t, srv, "item-a")
	defer b.Close()

	// Wait for both to be subscribed before publishing.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.SubscriberCount("item-a") == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := bus.SubscriberCount("item-a"); got != 2 {
		t.Fatalf("want 2 subscribers, got %d", got)
	}

	sendSync(t, a, 0x42)

	got := readBinaryWithin(t, b, 2*time.Second)
	if len(got) < 2 || got[0] != yMessageSync || got[1] != 0x42 {
		t.Errorf("B received unexpected frame: %v", got)
	}

	// A must NOT receive its own echo. Pause briefly to let any
	// stray broadcast land before asserting.
	expectNoMessage(t, a, 200*time.Millisecond)

	if store.appendCount() != 1 {
		t.Errorf("op-log appendCount: want 1, got %d", store.appendCount())
	}
}

// TestRoomManagerAwarenessNotPersisted is the symmetric test for
// awareness frames: broadcast to peers, never written to the op-log.
func TestRoomManagerAwarenessNotPersisted(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	a := dialWS(t, srv, "item-a")
	defer a.Close()
	b := dialWS(t, srv, "item-a")
	defer b.Close()

	// Wait for B's subscription.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.SubscriberCount("item-a") == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	sendAwareness(t, a, 0x77)

	got := readBinaryWithin(t, b, 2*time.Second)
	if len(got) < 2 || got[0] != yMessageAwareness || got[1] != 0x77 {
		t.Errorf("B received unexpected frame: %v", got)
	}

	if store.rowCount() != 0 {
		t.Errorf("op-log rowCount: want 0 (awareness must not persist), got %d", store.rowCount())
	}
}

// TestRoomManagerSerializesSyncAppends is a regression test for the
// "concurrent peers race AppendYjsUpdate" bug. With the per-room
// appendMu in place, N peers each writing M sync frames must produce
// exactly N*M op-log rows AND each peer must see every other peer's
// frames at least once. Without serialisation this could surface
// missing rows / out-of-order ids on Postgres.
func TestRoomManagerSerializesSyncAppends(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	const peers = 4
	const writesEach = 10
	wantRows := peers * writesEach

	conns := make([]*websocket.Conn, peers)
	for i := 0; i < peers; i++ {
		conns[i] = dialWS(t, srv, "item-a")
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	// Wait for all subscriptions to register.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.SubscriberCount("item-a") == peers {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := bus.SubscriberCount("item-a"); got != peers {
		t.Fatalf("subscriber count: want %d, got %d", peers, got)
	}

	// Drain readers so the subscriber channels stay empty (otherwise
	// the slow-consumer drop kicks in and the bus loses events).
	for _, c := range conns {
		go func(c *websocket.Conn) {
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		}(c)
	}

	var wg sync.WaitGroup
	for i := 0; i < peers; i++ {
		wg.Add(1)
		go func(c *websocket.Conn, base byte) {
			defer wg.Done()
			for j := 0; j < writesEach; j++ {
				if err := c.WriteMessage(
					websocket.BinaryMessage,
					[]byte{yMessageSync, base, byte(j)},
				); err != nil {
					t.Errorf("peer %d write %d: %v", base, j, err)
					return
				}
			}
		}(conns[i], byte(i))
	}
	wg.Wait()

	// Allow the read loops on each peer's roomConn to drain the in-flight
	// frames into the op-log. Poll instead of fixed sleep so the test
	// stays fast on a quiet machine and only waits as long as needed.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if store.rowCount() == wantRows {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := store.rowCount(); got != wantRows {
		t.Fatalf("op-log rowCount: want %d, got %d", wantRows, got)
	}
}

// TestRoomManagerCrossItemIsolation makes sure events for item-a do
// not leak to peers connected to item-b. Without per-item bus
// filtering this would silently scramble unrelated documents.
func TestRoomManagerCrossItemIsolation(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	a := dialWS(t, srv, "item-a")
	defer a.Close()
	b := dialWS(t, srv, "item-b")
	defer b.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.SubscriberCount("item-a") == 1 && bus.SubscriberCount("item-b") == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	sendSync(t, a, 0xAA)

	expectNoMessage(t, b, 200*time.Millisecond)
}

// TestRoomManagerGraceTTLReclaim exercises the grace-timer path with
// a short TTL injected via the per-manager config. After the timer
// fires the manager's room map drops the entry, so a fresh Join
// mints a new room with no in-memory state.
func TestRoomManagerGraceTTLReclaim(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManagerWithConfig(store, bus, RoomManagerConfig{GraceTTL: 50 * time.Millisecond})
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	c := dialWS(t, srv, "item-a")

	// Wait for room to register.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.RoomCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if mgr.RoomCount() != 1 {
		t.Fatalf("expected room created, got %d", mgr.RoomCount())
	}

	// Disconnect + verify reclaimed within the grace window.
	c.Close()

	deadline = time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.RoomCount() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if mgr.RoomCount() != 0 {
		t.Fatalf("expected room reclaimed after grace TTL, got %d", mgr.RoomCount())
	}
}

// TestRoomManagerGraceTTLCancelledByReconnect confirms a fresh
// connection within the grace window keeps the same Room — the
// room count does NOT bounce 1→0→1, it stays 1 throughout.
func TestRoomManagerGraceTTLCancelledByReconnect(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManagerWithConfig(store, bus, RoomManagerConfig{GraceTTL: 200 * time.Millisecond})
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	a := dialWS(t, srv, "item-a")

	// Wait for room registration.
	for i := 0; i < 200; i++ {
		if mgr.RoomCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Disconnect, reconnect well within the grace window.
	a.Close()
	time.Sleep(50 * time.Millisecond) // a fraction of grace; timer pending
	if mgr.RoomCount() != 1 {
		t.Fatalf("during grace: want room still present, got %d rooms", mgr.RoomCount())
	}

	b := dialWS(t, srv, "item-a")
	defer b.Close()

	// Wait past the original grace deadline; if the reconnect didn't
	// cancel the timer, we'd see RoomCount drop and a new room
	// allocate, which would be observable as a transient 0.
	time.Sleep(300 * time.Millisecond)
	if mgr.RoomCount() != 1 {
		t.Fatalf("after grace: want 1 room, got %d", mgr.RoomCount())
	}
}

// TestRoomManagerJoinAfterCloseFailsFast covers the post-Close race:
// http.Server.Shutdown does NOT wait for hijacked WS handlers to
// finish, so a Join can fire AFTER Close has returned. The manager
// must reject those Joins fast (errManagerClosed) instead of
// creating a fresh Room against a torn-down store.
func TestRoomManagerJoinAfterCloseFailsFast(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)

	mgr.Close()

	// Direct Join — bypass the WS plumbing since we want to exercise
	// the closed-flag short-circuit in isolation.
	err := mgr.Join("item-a", nil, 0) // conn is irrelevant; we never reach the WS path
	if !errors.Is(err, errManagerClosed) {
		t.Fatalf("want errManagerClosed, got %v", err)
	}

	// Idempotent: a second Close must not panic.
	mgr.Close()
}

// TestRoomManagerCloseShutsDownAllRooms exercises Close: after the
// call, any active connections should be closed (their reads
// terminate with an error) and the manager's room map should be
// empty.
func TestRoomManagerCloseShutsDownAllRooms(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	c := dialWS(t, srv, "item-a")
	defer c.Close()

	// Wait for room.
	for i := 0; i < 200; i++ {
		if mgr.RoomCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mgr.Close()

	if mgr.RoomCount() != 0 {
		t.Fatalf("after Close: want 0 rooms, got %d", mgr.RoomCount())
	}
	// Read should fail because Close closed the conn from the server
	// side. Drain the post-replay op_log_cursor TextMessage
	// (TASK-1319) so we hit the close, not a normal read.
	c.SetReadDeadline(time.Now().Add(1 * time.Second))
	for {
		mt, _, err := c.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.BinaryMessage {
			t.Errorf("unexpected payload frame after manager.Close")
			break
		}
	}
}

// TestRoomManagerSchemaMismatchPrunes is the TASK-1268 happy-path
// regression test. When a peer joins an item whose persisted op-log
// rows are stamped with a different schema version than the
// manager's current one, the room manager prunes the entire op-log
// for that item before replay so the new client doesn't replay
// (potentially incompatible) old-schema ops.
func TestRoomManagerSchemaMismatchPrunes(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()

	store := &fakeOpLog{}
	// Pre-seed two rows stamped with the OLD schema version.
	if _, err := store.AppendYjsUpdate("item-a", []byte{yMessageSync, 0x01}, "1"); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if _, err := store.AppendYjsUpdate("item-a", []byte{yMessageSync, 0x02}, "1"); err != nil {
		t.Fatalf("seed 2: %v", err)
	}

	// Manager runs at version "2" — first new-version client to
	// arrive should trigger the prune.
	mgr := NewRoomManagerWithConfig(store, bus, RoomManagerConfig{
		SchemaVersion: "2",
	})
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	c := dialWS(t, srv, "item-a")
	defer c.Close()

	// After the prune, the op-log replay sends NO BINARY frames.
	// The post-replay op_log_cursor TextMessage (TASK-1319) is
	// expected; everything else means the prune didn't take.
	c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		mt, _, err := c.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.BinaryMessage {
			t.Errorf("expected no replay binary frames after schema-mismatch prune; got one")
			break
		}
	}

	// Op-log should be empty for item-a. The store's rowCount
	// reflects the post-prune state (we pruned in maybeRebuildOnSchemaMismatch).
	if rc := store.rowCount(); rc != 0 {
		t.Errorf("after schema-mismatch prune: want 0 rows, got %d", rc)
	}
}

// TestRoomManagerSchemaCleanVersionPreservesOpLog is the negative
// case: when the persisted version matches the manager's, no prune
// runs and the replay path is undisturbed.
func TestRoomManagerSchemaCleanVersionPreservesOpLog(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()

	store := &fakeOpLog{}
	if _, err := store.AppendYjsUpdate("item-a", []byte{yMessageSync, 0x01}, "1"); err != nil {
		t.Fatalf("seed 1: %v", err)
	}

	// Manager and persisted rows agree on "1".
	mgr := NewRoomManagerWithConfig(store, bus, RoomManagerConfig{
		SchemaVersion: "1",
	})
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	c := dialWS(t, srv, "item-a")
	defer c.Close()

	// Replay should land normally — single row.
	first := readBinaryWithin(t, c, 2*time.Second)
	if len(first) < 2 || first[1] != 0x01 {
		t.Errorf("replay row 1 mismatch: got %v", first)
	}

	if rc := store.rowCount(); rc != 1 {
		t.Errorf("on clean-version connect: op-log should be untouched (want 1 row), got %d", rc)
	}
}

// TestRoomManagerSchemaPostRebuildClean confirms that after a
// rebuild fires once, subsequent connects on the same item are
// clean: the post-prune append carries the new schema version,
// LatestYjsUpdateSchemaVersion now reads as "2", and a second peer
// connecting with version "2" does NOT re-trigger the prune.
func TestRoomManagerSchemaPostRebuildClean(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()

	store := &fakeOpLog{}
	if _, err := store.AppendYjsUpdate("item-a", []byte{yMessageSync, 0x01}, "1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mgr := NewRoomManagerWithConfig(store, bus, RoomManagerConfig{
		SchemaVersion: "2",
	})
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	// First connect: triggers the prune.
	c1 := dialWS(t, srv, "item-a")

	// Wait for the prune to actually land. RoomCount() == 1 isn't a
	// prune-completion barrier (getOrCreate publishes the room
	// BEFORE maybeRebuildOnSchemaMismatch runs), so poll the store
	// state directly. Per Codex review round 1 NIT.
	for i := 0; i < 200; i++ {
		if store.rowCount() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if rc := store.rowCount(); rc != 0 {
		t.Errorf("after first join (mismatch): want 0 rows, got %d", rc)
	}

	// Have c1 send a sync frame so the post-prune op-log gets a row
	// stamped at the manager's "2".
	if err := c1.WriteMessage(websocket.BinaryMessage, []byte{yMessageSync, 0x55}); err != nil {
		t.Fatalf("write live frame: %v", err)
	}
	for i := 0; i < 200; i++ {
		if store.rowCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rc := store.rowCount(); rc != 1 {
		t.Fatalf("post-rebuild append: want 1 row, got %d", rc)
	}

	// Second connect on the same item: persisted version is now
	// "2", manager is "2", so no prune. The single row must
	// replay to the new peer untouched.
	c2 := dialWS(t, srv, "item-a")
	defer c2.Close()

	got := readBinaryWithin(t, c2, 2*time.Second)
	if len(got) < 2 || got[1] != 0x55 {
		t.Errorf("post-rebuild replay row mismatch: got %v", got)
	}

	c1.Close()
}

// TestPruneSweepRemovesDormantItems is the TASK-1309 happy-path test:
// a sweep finds items whose entire op-log is older than minAge and
// prunes them whole. Items with ANY recent activity are preserved
// completely (no prefix-pruning — Yjs causal references would break
// per the Codex P1 finding).
func TestPruneSweepRemovesDormantItems(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()

	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	// Item A: two old rows + one fresh row → mixed, NOT dormant.
	// Whole-log prune is unsafe here (suffix references prefix
	// structs in real Yjs). All three rows must survive.
	now := time.Now()
	store.mu.Lock()
	store.rows = append(store.rows,
		fakeYjsRow("item-a", []byte{yMessageSync, 0x01}, "1", now.Add(-48*time.Hour)),
		fakeYjsRow("item-a", []byte{yMessageSync, 0x02}, "1", now.Add(-30*time.Hour)),
		fakeYjsRow("item-a", []byte{yMessageSync, 0x03}, "1", now.Add(-1*time.Hour)),
	)
	// Item B: one fresh row → not dormant. Survives.
	store.rows = append(store.rows,
		fakeYjsRow("item-b", []byte{yMessageSync, 0x04}, "1", now.Add(-2*time.Hour)),
	)
	// Item C: two old rows, no recent activity → DORMANT. Whole
	// op-log gets pruned; cold reconnect lazy-seeds from
	// items.content (TASK-1261).
	store.rows = append(store.rows,
		fakeYjsRow("item-c", []byte{yMessageSync, 0x05}, "1", now.Add(-72*time.Hour)),
		fakeYjsRow("item-c", []byte{yMessageSync, 0x06}, "1", now.Add(-50*time.Hour)),
	)
	store.mu.Unlock()

	res, err := mgr.PruneSweep(24 * time.Hour)
	if err != nil {
		t.Fatalf("PruneSweep: %v", err)
	}

	// Only item-c is dormant (MAX(created_at) < cutoff).
	if got := res.ItemsScanned; got != 1 {
		t.Errorf("ItemsScanned: want 1 (only c is dormant), got %d", got)
	}
	if got := res.ItemsPruned; got != 1 {
		t.Errorf("ItemsPruned: want 1, got %d", got)
	}
	if got := res.RowsPruned; got != 2 {
		t.Errorf("RowsPruned: want 2 (c's whole op-log), got %d", got)
	}
	if got := res.Errors; got != 0 {
		t.Errorf("Errors: want 0, got %d", got)
	}

	// Surviving rows: item-a's three + item-b's one. item-c gone.
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := len(store.rows); got != 4 {
		t.Fatalf("after prune: want 4 surviving rows (3 from item-a + 1 from item-b), got %d", got)
	}
	byItem := map[string]int{}
	for _, r := range store.rows {
		byItem[r.ItemID]++
	}
	if byItem["item-a"] != 3 {
		t.Errorf("item-a: want 3 surviving rows (whole-log preserved because non-dormant), got %d", byItem["item-a"])
	}
	if byItem["item-b"] != 1 {
		t.Errorf("item-b: want 1 surviving row, got %d", byItem["item-b"])
	}
	if byItem["item-c"] != 0 {
		t.Errorf("item-c: want 0 surviving rows (dormant, whole-log pruned), got %d", byItem["item-c"])
	}
}

// TestPruneSweepDefaultMinAge verifies that passing minAge=0 falls
// back to DefaultPruneMinAge rather than treating the cutoff as
// "now" (which would prune every row in the table).
func TestPruneSweepDefaultMinAge(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()

	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	// One row, 1 hour old. With DefaultPruneMinAge=24h this row
	// should be PRESERVED. If the implementation accidentally used
	// `time.Now()` as the cutoff, the row would be deleted.
	now := time.Now()
	store.mu.Lock()
	store.rows = append(store.rows,
		fakeYjsRow("item-a", []byte{yMessageSync, 0x01}, "1", now.Add(-1*time.Hour)),
	)
	store.mu.Unlock()

	res, err := mgr.PruneSweep(0)
	if err != nil {
		t.Fatalf("PruneSweep: %v", err)
	}
	if res.RowsPruned != 0 {
		t.Errorf("with default minAge, 1-hour-old row should survive; got RowsPruned=%d", res.RowsPruned)
	}
	if got := store.rowCount(); got != 1 {
		t.Errorf("rows after sweep: want 1, got %d", got)
	}
}

// TestPruneSweepEmpty handles the no-eligible-rows case cleanly.
func TestPruneSweepEmpty(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()

	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	res, err := mgr.PruneSweep(24 * time.Hour)
	if err != nil {
		t.Fatalf("PruneSweep: %v", err)
	}
	if res.ItemsScanned != 0 || res.RowsPruned != 0 || res.Errors != 0 {
		t.Errorf("empty store should produce zero counters, got %+v", res)
	}
}

// TestPruneSweepBailsOnClose ensures a sweep mid-loop respects a
// concurrent Close — once the manager is closed, the sweep stops
// iterating remaining items rather than continuing to touch a
// torn-down store.
func TestPruneSweepBailsOnClose(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()

	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)

	now := time.Now()
	store.mu.Lock()
	for i := 0; i < 10; i++ {
		itemID := fmt.Sprintf("item-%d", i)
		store.rows = append(store.rows,
			fakeYjsRow(itemID, []byte{yMessageSync, byte(i)}, "1", now.Add(-48*time.Hour)),
		)
	}
	store.mu.Unlock()

	// Pre-close the manager: the sweep should bail on the first
	// per-item iteration. Close() runs synchronously here so the
	// closed flag is set before PruneSweep starts.
	mgr.Close()

	res, err := mgr.PruneSweep(24 * time.Hour)
	if err != nil {
		t.Fatalf("PruneSweep: %v", err)
	}
	// ItemsScanned reflects the initial query (which ran before
	// the closed-flag check), but no items should have been
	// pruned because the per-item loop bailed immediately.
	if res.ItemsScanned != 10 {
		t.Errorf("ItemsScanned: want 10 (query ran), got %d", res.ItemsScanned)
	}
	if res.RowsPruned != 0 {
		t.Errorf("RowsPruned after Close: want 0, got %d", res.RowsPruned)
	}
}

// TestPruneSweepSkipsActiveRoom verifies the dormant-item check
// honours an in-memory active Room — even if the database query
// returned the item as dormant, an existing Room means we shouldn't
// prune. (Active rooms can buffer awareness state, hold a grace
// timer, etc.; a fresh peer reconnecting via grace would expect to
// find the prior op-log intact.)
func TestPruneSweepSkipsActiveRoom(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()

	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	// Seed one dormant item.
	now := time.Now()
	store.mu.Lock()
	store.rows = append(store.rows,
		fakeYjsRow("item-a", []byte{yMessageSync, 0x01}, "1", now.Add(-48*time.Hour)),
	)
	store.mu.Unlock()

	// Create a room for item-a directly. We don't need a real WS;
	// just having the entry in m.rooms is enough to trigger the
	// active-room skip.
	mgr.mu.Lock()
	mgr.rooms["item-a"] = &Room{
		itemID: "item-a",
		store:  store,
		bus:    bus,
		conns:  make(map[*websocket.Conn]*roomConn),
	}
	mgr.mu.Unlock()

	res, err := mgr.PruneSweep(24 * time.Hour)
	if err != nil {
		t.Fatalf("PruneSweep: %v", err)
	}

	if res.ItemsPruned != 0 {
		t.Errorf("ItemsPruned: want 0 (active room blocks prune), got %d", res.ItemsPruned)
	}
	if res.ItemsSkipped != 1 {
		t.Errorf("ItemsSkipped: want 1 (item-a skipped due to active room), got %d", res.ItemsSkipped)
	}
	if got := store.rowCount(); got != 1 {
		t.Errorf("rows after sweep: want 1 (active room preserved), got %d", got)
	}
}

// TestPruneSweepSkipsRowAddedMidSweep exercises the mid-sweep race
// window: the dormancy listing returns item-a (it WAS dormant at
// query time), but a row gets appended before PruneItemOpLogIfDormantBefore
// runs under the per-item lock. The conditional DELETE in the store
// must re-check dormancy at SQL level and refuse to delete.
//
// We simulate the race via the onListDormantHook on fakeOpLog,
// which fires after the listing is computed but before
// PruneSweep iterates. The hook injects a recent row that the
// candidate-list already committed to but that the conditional
// DELETE must catch.
//
// Per Codex review of TASK-1309 round 2 NIT.
func TestPruneSweepSkipsRowAddedMidSweep(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()

	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	// Seed a single old row → item-a IS dormant at query time.
	now := time.Now()
	store.mu.Lock()
	store.rows = append(store.rows,
		fakeYjsRow("item-a", []byte{yMessageSync, 0x01}, "1", now.Add(-48*time.Hour)),
	)
	// Inject a recent row in the race window — listing already
	// computed, but per-item DELETE hasn't run yet.
	store.onListDormantHook = func(f *fakeOpLog) {
		f.mu.Lock()
		f.rows = append(f.rows,
			fakeYjsRow("item-a", []byte{yMessageSync, 0x02}, "1", now.Add(-1*time.Hour)),
		)
		f.mu.Unlock()
	}
	store.mu.Unlock()

	res, err := mgr.PruneSweep(24 * time.Hour)
	if err != nil {
		t.Fatalf("PruneSweep: %v", err)
	}

	// item-a was returned by the listing (1 candidate scanned),
	// but the conditional DELETE saw the newly-appended row and
	// refused. Expect 1 scanned, 0 pruned, 1 skipped.
	if res.ItemsScanned != 1 {
		t.Errorf("ItemsScanned: want 1 (listing saw dormant item), got %d", res.ItemsScanned)
	}
	if res.ItemsPruned != 0 {
		t.Errorf("ItemsPruned: want 0 (conditional DELETE refused), got %d", res.ItemsPruned)
	}
	if res.ItemsSkipped != 1 {
		t.Errorf("ItemsSkipped: want 1 (DELETE returned 0 rows), got %d", res.ItemsSkipped)
	}
	if got := store.rowCount(); got != 2 {
		t.Errorf("rows after sweep: want 2 (both preserved), got %d", got)
	}
}

// fakeYjsRow constructs a single row for the fakeOpLog seeding helpers.
// The store assigns ids in production; tests don't depend on them.
func fakeYjsRow(itemID string, data []byte, schemaVersion string, createdAt time.Time) models.YjsUpdate {
	cp := make([]byte, len(data))
	copy(cp, data)
	return models.YjsUpdate{
		ItemID:        itemID,
		UpdateData:    cp,
		SchemaVersion: schemaVersion,
		CreatedAt:     createdAt.UTC(),
	}
}

// readControlWithin reads frames off the conn within d, drains
// payload (binary) frames, and returns the FIRST TextMessage parsed
// as a ControlMessage. Tests use this to assert the post-replay
// op_log_cursor frame and force_refresh frame land cleanly.
func readControlWithin(t *testing.T, c *websocket.Conn, d time.Duration) ControlMessage {
	t.Helper()
	deadline := time.Now().Add(d)
	c.SetReadDeadline(deadline)
	defer c.SetReadDeadline(time.Time{})
	for {
		mt, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read control: %v", err)
		}
		if mt == websocket.TextMessage {
			var msg ControlMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("unmarshal control: %v (raw=%q)", err, data)
			}
			return msg
		}
		// Skip binary replay frames; the cursor is what we want.
		c.SetReadDeadline(deadline)
	}
}

// TestRoomManagerInitialOpLogCursorAfterReplay confirms a fresh peer
// receives an op_log_cursor TextMessage after replay so its
// sessionStorage cursor is anchored without a round trip. Per TASK-1319.
func TestRoomManagerInitialOpLogCursorAfterReplay(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	if _, err := store.AppendYjsUpdate("item-a", []byte{yMessageSync, 0x01}, "1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.AppendYjsUpdate("item-a", []byte{yMessageSync, 0x02}, "1"); err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	mgr := NewRoomManager(store, bus)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	c := dialWS(t, srv, "item-a")
	defer c.Close()

	// Drain the two replay binary frames.
	_ = readBinaryWithin(t, c, time.Second)
	_ = readBinaryWithin(t, c, time.Second)

	// Now the cursor frame must arrive, anchored at the highest
	// replayed id (2 after two seeds).
	cursor := readControlWithin(t, c, time.Second)
	if cursor.Type != ControlMessageOpLogCursor {
		t.Fatalf("type: want %q, got %q", ControlMessageOpLogCursor, cursor.Type)
	}
	if cursor.OpLogID != 2 {
		t.Errorf("op_log_id: want 2, got %d", cursor.OpLogID)
	}
}

// TestRoomManagerInitialCursorEmptyOpLog confirms a fresh peer joining
// an item with no persisted ops still receives an op_log_cursor frame
// with id 0 — clears any stale sessionStorage cursor from an earlier
// life of this item id. Per TASK-1319.
func TestRoomManagerInitialCursorEmptyOpLog(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	c := dialWS(t, srv, "item-a")
	defer c.Close()

	cursor := readControlWithin(t, c, time.Second)
	if cursor.Type != ControlMessageOpLogCursor {
		t.Fatalf("type: want %q, got %q", ControlMessageOpLogCursor, cursor.Type)
	}
	if cursor.OpLogID != 0 {
		t.Errorf("op_log_id: want 0 (empty op-log), got %d", cursor.OpLogID)
	}
}

// TestRoomManagerForceRefreshOnStaleSince exercises the resume-cursor
// safety path: a client announcing `?since=N` where N is below
// MIN(item_yjs_updates.id) gets a force_refresh frame and a closed
// conn. Per TASK-1319.
func TestRoomManagerForceRefreshOnStaleSince(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	// Seed two rows then prune the older one so MIN(id) > 1.
	if _, err := store.AppendYjsUpdate("item-a", []byte{yMessageSync, 0x01}, "1"); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if _, err := store.AppendYjsUpdate("item-a", []byte{yMessageSync, 0x02}, "1"); err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	// Manually drop row id=1 to simulate a partial prune. The fake's
	// rows slice is in-place; we reuse the same field write semantics
	// as PruneYjsUpdatesBefore would.
	store.mu.Lock()
	kept := store.rows[:0]
	for _, r := range store.rows {
		if r.ID != 1 {
			kept = append(kept, r)
		}
	}
	store.rows = kept
	store.mu.Unlock()

	mgr := NewRoomManager(store, bus)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	// Client claims it has applied id=0 (no rows seen) but server's
	// MIN is now 2 — wait, since=0 is special-cased as "fresh client"
	// and skips the force_refresh check. Use since=1 instead — below
	// MIN=2 so the protocol fires.
	c := dialWSSince(t, srv, "item-a", 1)
	defer c.Close()

	cursor := readControlWithin(t, c, time.Second)
	if cursor.Type != ControlMessageForceRefresh {
		t.Fatalf("type: want %q, got %q", ControlMessageForceRefresh, cursor.Type)
	}
	// After the force_refresh frame the server closes; the next
	// read should error.
	c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Errorf("expected close after force_refresh; got another frame")
	}
}

// TestRoomManagerSinceAtOrAboveMinReplaysDelta confirms a client
// resuming with `?since=N` where N >= MIN gets only the rows with
// id > N (delta replay), not the entire op-log. Per TASK-1319.
func TestRoomManagerSinceAtOrAboveMinReplaysDelta(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	for i := byte(1); i <= 4; i++ {
		if _, err := store.AppendYjsUpdate("item-a", []byte{yMessageSync, i}, "1"); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	mgr := NewRoomManager(store, bus)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	// since=2 → expect rows id 3 and 4 only (two binary frames),
	// then a cursor frame at id 4.
	c := dialWSSince(t, srv, "item-a", 2)
	defer c.Close()

	got1 := readBinaryWithin(t, c, time.Second)
	if len(got1) < 2 || got1[1] != 0x03 {
		t.Errorf("first replayed payload: want byte 0x03, got %v", got1)
	}
	got2 := readBinaryWithin(t, c, time.Second)
	if len(got2) < 2 || got2[1] != 0x04 {
		t.Errorf("second replayed payload: want byte 0x04, got %v", got2)
	}

	// No more binary frames — the cursor should be next.
	cursor := readControlWithin(t, c, time.Second)
	if cursor.Type != ControlMessageOpLogCursor || cursor.OpLogID != 4 {
		t.Errorf("cursor: want type=op_log_cursor id=4, got type=%q id=%d", cursor.Type, cursor.OpLogID)
	}
}

// TestRoomManagerSyncCursorBroadcast confirms that after a peer
// pushes a sync frame, the originator gets an op_log_cursor frame
// with the new id (so it can advance its session-storage cursor)
// AND every other peer also gets one piggybacked on the broadcast
// (so they stay in lockstep without a round trip).
func TestRoomManagerSyncCursorBroadcast(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	a := dialWS(t, srv, "item-a")
	defer a.Close()
	b := dialWS(t, srv, "item-a")
	defer b.Close()

	// Drain initial cursor frames on both peers.
	_ = readControlWithin(t, a, time.Second)
	_ = readControlWithin(t, b, time.Second)

	// Wait for both subscriptions to register so the broadcast is
	// guaranteed to reach B.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.SubscriberCount("item-a") == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	sendSync(t, a, 0x42)

	// A (originator) gets a cursor frame with the new id (1).
	cursorA := readControlWithin(t, a, time.Second)
	if cursorA.Type != ControlMessageOpLogCursor || cursorA.OpLogID != 1 {
		t.Errorf("originator cursor: want op_log_cursor id=1, got type=%q id=%d", cursorA.Type, cursorA.OpLogID)
	}

	// B receives the binary fan-out plus a cursor frame at id=1.
	gotB := readBinaryWithin(t, b, time.Second)
	if len(gotB) < 2 || gotB[1] != 0x42 {
		t.Errorf("peer payload: %v", gotB)
	}
	cursorB := readControlWithin(t, b, time.Second)
	if cursorB.Type != ControlMessageOpLogCursor || cursorB.OpLogID != 1 {
		t.Errorf("peer cursor: want op_log_cursor id=1, got type=%q id=%d", cursorB.Type, cursorB.OpLogID)
	}
}

// TestRoomManagerForceRefreshOnEmptyOpLogWithSince exercises the
// branch added in Codex round 2 [P1] of TASK-1319: a client
// announces `?since=N>0` against an item whose ENTIRE op-log has
// been pruned (PruneAndApply, schema rebuild, or dormant GC). MIN
// is undefined, so the original `hasMin && since < minID` predicate
// would have admitted the connection — and the client's on-open
// `Y.encodeStateAsUpdate` write would have resurrected the stale
// pre-prune document.
func TestRoomManagerForceRefreshOnEmptyOpLogWithSince(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	// No seed rows — empty op-log, simulating post-prune state.
	c := dialWSSince(t, srv, "item-a", 5)
	defer c.Close()

	cursor := readControlWithin(t, c, time.Second)
	if cursor.Type != ControlMessageForceRefresh {
		t.Fatalf("type: want %q, got %q", ControlMessageForceRefresh, cursor.Type)
	}
}

// TestRoomManagerCursorSuppressedDuringReplay is the round 5 [P1]
// regression: a live op broadcast during the replay window MUST NOT
// generate an op_log_cursor TextMessage to a peer mid-replay. If it
// did, a client disconnecting after that cursor but before the rest
// of the replay rows would persist a cursor pointing past unreplayed
// rows.
//
// We exercise the path by:
//   - seeding 3 rows so peer B's replay has work to do,
//   - waiting for B to subscribe AND start replay,
//   - sending a sync from peer A (live op id=4) — broadcast lands in
//     B's bus channel, processed by B's writeLoop concurrently with
//     replayTo's writes.
//
// The assertion is loose-but-decisive: B sees no op_log_cursor frame
// with id == 4 BEFORE all four binary frames have arrived. After
// replay completes the post-replay cursor (id=3) and then live cursor
// (id=4) flow normally.
func TestRoomManagerCursorSuppressedDuringReplay(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	for i := byte(1); i <= 3; i++ {
		if _, err := store.AppendYjsUpdate("item-a", []byte{yMessageSync, i}, "1"); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	mgr := NewRoomManager(store, bus)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	a := dialWS(t, srv, "item-a")
	defer a.Close()
	// Drain A's replay (3 binary) + post-replay cursor frame.
	_ = readBinaryWithin(t, a, time.Second)
	_ = readBinaryWithin(t, a, time.Second)
	_ = readBinaryWithin(t, a, time.Second)
	_ = readControlWithin(t, a, time.Second)

	b := dialWS(t, srv, "item-a")
	defer b.Close()

	// Push a live op from A while B is still in setup.
	sendSync(t, a, 0xFF)

	// B is racing replay + the live op fan-out. Read frames in
	// order until we've collected all 4 expected binary frames
	// (3 replayed + 1 live) and the post-replay + live cursor
	// frames. If a cursor frame for id=4 arrives BEFORE all
	// binary frames are accounted for, this is the regression.
	binarySeen := 0
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.SetReadDeadline(deadline)
		mt, data, err := b.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.BinaryMessage {
			binarySeen++
			continue
		}
		if mt != websocket.TextMessage {
			continue
		}
		var msg ControlMessage
		if jerr := json.Unmarshal(data, &msg); jerr != nil {
			continue
		}
		if msg.Type != ControlMessageOpLogCursor {
			continue
		}
		// A live cursor with id=4 must only arrive AFTER all 4
		// binary frames are accounted for; otherwise replay-mid
		// cursor advancement leaked through.
		if msg.OpLogID == 4 && binarySeen < 4 {
			t.Errorf("cursor id=4 arrived after only %d/4 binary frames; replay-mid cursor leaked", binarySeen)
		}
	}
	if binarySeen != 4 {
		t.Errorf("binary frames seen: want 4, got %d", binarySeen)
	}
}
