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
