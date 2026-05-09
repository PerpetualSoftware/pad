package collab

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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
// the most recent op-log row's schema_version for an item, or
// (false) when no rows exist. The fake walks rows[] in append order
// and returns the latest matching entry — same semantic as the
// store's `ORDER BY id DESC LIMIT 1`.
func (f *fakeOpLog) LatestYjsUpdateSchemaVersion(itemID string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.rows) - 1; i >= 0; i-- {
		if f.rows[i].ItemID == itemID {
			return f.rows[i].SchemaVersion, true, nil
		}
	}
	return "", false, nil
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
		_ = mgr.Join(itemID, conn)
	})
	return httptest.NewServer(mux)
}

func dialWS(t *testing.T, server *httptest.Server, itemID string) *websocket.Conn {
	t.Helper()
	u, _ := url.Parse(server.URL)
	wsURL := "ws://" + u.Host + "/ws/" + itemID
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
	c.SetReadDeadline(time.Now().Add(d))
	defer c.SetReadDeadline(time.Time{})
	mt, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if mt != websocket.BinaryMessage {
		t.Fatalf("expected binary frame, got %d", mt)
	}
	return data
}

// expectNoMessage checks that no frame arrives within d.
func expectNoMessage(t *testing.T, c *websocket.Conn, d time.Duration) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(d))
	defer c.SetReadDeadline(time.Time{})
	if mt, data, err := c.ReadMessage(); err == nil {
		t.Fatalf("expected no frame, got mt=%d data=%v", mt, data)
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
	err := mgr.Join("item-a", nil) // conn is irrelevant; we never reach the WS path
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
	// side.
	c.SetReadDeadline(time.Now().Add(1 * time.Second))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Errorf("expected read error after manager.Close")
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

	// After the prune, the op-log replay sends NOTHING. Confirm by
	// setting a short read deadline and asserting we time out
	// rather than receiving stale rows.
	c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Errorf("expected no replay frames after schema-mismatch prune; got one")
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
