package collab

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// BUG-3771 / BUG-3636: a collab peer that stops reading or answering is
// detected and closed within a bounded time. The server pings and reads with
// a deadline (a half-open peer is reaped), every write has a deadline (a
// non-reading peer cannot hold the item's setup lock or a writer), and
// teardown closes the socket before waiting on the writer.

// setCollabTimeouts shortens the manager's liveness timeouts. Call it before
// the manager serves: each connection copies them at Join.
func setCollabTimeouts(mgr *RoomManager, write, pong, ping time.Duration) {
	mgr.writeTimeout, mgr.pongWait, mgr.pingPeriod = write, pong, ping
}

// joinServer is newCollabTestServer that also reports when each Join
// returns, so a test can bound how long a dead peer is held.
func joinServer(t *testing.T, mgr *RoomManager) (*httptest.Server, chan error) {
	t.Helper()
	done := make(chan error, 8)
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
		var since int64
		if raw := r.URL.Query().Get("since"); raw != "" {
			since, _ = strconv.ParseInt(raw, 10, 64)
		}
		done <- mgr.Join(itemID, conn, since, 0, true, false, nil)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, done
}

func dialItem(t *testing.T, srv *httptest.Server, itemID string) *websocket.Conn {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	c, _, err := websocket.DefaultDialer.Dial("ws://"+u.Host+"/ws/"+itemID, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func waitJoinReturn(t *testing.T, done chan error, within time.Duration, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("%s: Join still running after %v", what, within)
	}
}

// BUG-3771: a peer that stops reading after it is anchored never answers a
// ping, so its read deadline lapses and Join returns.
func TestBUG3771_SilentPeerIsReaped(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	setCollabTimeouts(mgr, time.Second, 300*time.Millisecond, 100*time.Millisecond)
	srv, done := joinServer(t, mgr)

	c := dialItem(t, srv, "item-silent")
	readControlWithin(t, c, 2*time.Second) // anchored; then it goes silent
	waitJoinReturn(t, done, 3*time.Second, "a peer that never pongs")
}

// A live peer that keeps reading answers the pings and stays connected
// well past the pong wait: the keepalive reaps only the silent.
func TestBUG3771_LivePeerSurvivesThePongWait(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	setCollabTimeouts(mgr, time.Second, 300*time.Millisecond, 100*time.Millisecond)
	srv, done := joinServer(t, mgr)

	c := dialItem(t, srv, "item-live")
	readControlWithin(t, c, 2*time.Second)
	go func() {
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}()
	select {
	case err := <-done:
		t.Fatalf("a reading peer was dropped: Join returned %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
}

// bigHistory seeds enough op-log bytes that a peer which never reads cannot
// absorb the replay into socket buffers.
func bigHistory(t *testing.T, store *fakeOpLog, itemID string) {
	t.Helper()
	frame := bytes.Repeat([]byte{0x00}, 1<<20)
	for i := 0; i < 64; i++ {
		if _, err := store.AppendYjsUpdate(itemID, frame, DefaultSchemaVersion); err != nil {
			t.Fatal(err)
		}
	}
}

// BUG-3636 (1): a peer that connects and never reads its replay used to hold
// the item's setup lock for as long as its socket stayed open, so a second
// peer could not join the item. Bounded by the write deadline now.
func TestBUG3636_NonReadingReplayDoesNotHoldTheItem(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	bigHistory(t, store, "item-heavy")
	mgr := NewRoomManager(store, bus)
	setCollabTimeouts(mgr, 300*time.Millisecond, time.Minute, time.Minute)
	srv, done := joinServer(t, mgr)

	_ = dialItem(t, srv, "item-heavy") // never reads
	time.Sleep(200 * time.Millisecond)
	second := dialItem(t, srv, "item-heavy")
	got := make(chan []byte, 1)
	go func() {
		for {
			mt, data, err := second.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage {
				got <- data
				return
			}
		}
	}()
	select {
	case data := <-got:
		if !bytes.Contains(data, []byte(ControlMessageOpLogCursor)) {
			t.Fatalf("second peer's first control frame = %s, want its op_log_cursor", data)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("a second peer could not join the item while the first never read its replay")
	}
	waitJoinReturn(t, done, 5*time.Second, "the non-reading peer's replay")
}

// BUG-3636 (2): a peer that stops reading while live traffic fills its socket
// leaves its writer blocked mid-write. When its read then fails, teardown
// used to wait on that writer before closing the socket, forever. The write
// timeout here is far longer than the test, so only closing first ends it.
func TestBUG3636_TeardownDoesNotWaitOnABlockedWriter(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	setCollabTimeouts(mgr, time.Hour, 300*time.Millisecond, 100*time.Millisecond)
	srv, done := joinServer(t, mgr)

	stuck := dialItem(t, srv, "item-flood")
	readControlWithin(t, stuck, 2*time.Second) // anchored; then it goes silent
	sender := dialItem(t, srv, "item-flood")
	readControlWithin(t, sender, 2*time.Second)
	go func() {
		for {
			if _, _, err := sender.ReadMessage(); err != nil {
				return
			}
		}
	}()
	// y-protocol sync update frames (messageSync, syncUpdate), large enough
	// to fill the silent peer's socket buffers within a few writes.
	frame := append([]byte{0x00, 0x02}, bytes.Repeat([]byte{0x01}, 512<<10)...)
	for i := 0; i < 48; i++ {
		_ = sender.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if err := sender.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			break
		}
	}
	waitJoinReturn(t, done, 5*time.Second, "the silent peer with a blocked writer")
}
