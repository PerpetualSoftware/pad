package collab

import (
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// BUG-3749: an applier ack resolved as persisted when no frame of its apply
// bracket was frozen-dropped, without asking whether the append FAILED. A
// failed op-log append left the external write in the tabs' memory only, and
// the REST caller was told it landed.

// answerApplierRequests plays a tab: on each applier_request it opens the
// bracket (when bracketed), sends frames, then acks.
func answerApplierRequests(t *testing.T, conn *websocket.Conn, bracketed bool, frames int) {
	t.Helper()
	go func() {
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			var ctl ControlMessage
			if json.Unmarshal(data, &ctl) != nil || ctl.Type != ControlMessageApplierRequest {
				continue
			}
			if bracketed {
				start, _ := json.Marshal(ControlMessage{Type: ControlMessageApplierApplyStart, RequestID: ctl.RequestID})
				_ = conn.WriteMessage(websocket.TextMessage, start)
			}
			for i := 0; i < frames; i++ {
				_ = conn.WriteMessage(websocket.BinaryMessage, []byte{yMessageSync, 0x02, 0x02, 0xB0, byte(i)})
			}
			ack, _ := json.Marshal(ControlMessage{Type: ControlMessageApplierAck, RequestID: ctl.RequestID})
			_ = conn.WriteMessage(websocket.TextMessage, ack)
		}
	}()
}

func applyWithStore(t *testing.T, itemID string, bracketed bool, frames int, failAppend bool) (error, *fakeOpLog) {
	t.Helper()
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	var conn *websocket.Conn
	if bracketed {
		conn = dialWSBracket(t, srv, itemID)
	} else {
		conn = dialWS(t, srv, itemID)
	}
	defer conn.Close()
	answerApplierRequests(t, conn, bracketed, frames)
	waitElectable(t, mgr, itemID, 1)

	if failAppend {
		store.mu.Lock()
		store.failOn = "db down"
		store.mu.Unlock()
	}
	done := make(chan error, 1)
	go func() { done <- mgr.ApplyExternalContent(itemID, "# external") }()
	select {
	case err := <-done:
		return err, store
	case <-time.After(10 * time.Second):
		t.Fatal("ApplyExternalContent did not return")
		return nil, store
	}
}

func TestBUG3749_FailedAppendInBracketIsNotPersisted(t *testing.T) {
	err, store := applyWithStore(t, "item-3749-fail", true, 1, true)
	if !errors.Is(err, ErrApplierNotDurable) {
		t.Fatalf("a bracket whose append failed resolved as %v, want ErrApplierNotDurable", err)
	}
	store.mu.Lock()
	n := len(store.rows)
	store.mu.Unlock()
	if n != 0 {
		t.Fatalf("precondition: the failing store kept %d rows", n)
	}
}

// A legacy (un-bracketed) round-trip cannot attribute the failure to its
// frame, and still must not report a failed append as stored.
func TestBUG3749_FailedAppendInLegacyRoundTripIsNotPersisted(t *testing.T) {
	err, _ := applyWithStore(t, "item-3749-legacy", false, 1, true)
	if !errors.Is(err, ErrApplierNotDurable) {
		t.Fatalf("a legacy round-trip whose append failed resolved as %v, want ErrApplierNotDurable", err)
	}
}

// Controls: a stored frame and a no-op diff (no frame at all) still succeed.
func TestBUG3749_StoredAppendAndNoOpDiffStillSucceed(t *testing.T) {
	if err, store := applyWithStore(t, "item-3749-ok", true, 1, false); err != nil {
		t.Fatalf("a stored bracket resolved as %v, want success", err)
	} else {
		store.mu.Lock()
		n := len(store.rows)
		store.mu.Unlock()
		if n == 0 {
			t.Fatal("precondition: the frame was not stored")
		}
	}
	if err, _ := applyWithStore(t, "item-3749-noop", true, 0, true); err != nil {
		t.Fatalf("a no-op diff (no frame) resolved as %v, want success", err)
	}
}

// The freeze-finalize path: a restore that freezes the room while a bracket
// holds a failed append must not finalize it as not-persisted. That outcome
// re-applies after the rollback, the re-apply diffs against a document that
// already holds the content, sends nothing, and would read as stored.
func TestBUG3749_RestoreFinalizesAFailedBracketAsNotDurable(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWSBracket(t, srv, "item-3749-restore")
	defer conn.Close()

	var requests atomic.Int32
	sent := make(chan struct{}, 1)
	go func() {
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			var ctl ControlMessage
			if json.Unmarshal(data, &ctl) != nil || ctl.Type != ControlMessageApplierRequest {
				continue
			}
			requests.Add(1)
			start, _ := json.Marshal(ControlMessage{Type: ControlMessageApplierApplyStart, RequestID: ctl.RequestID})
			_ = conn.WriteMessage(websocket.TextMessage, start)
			_ = conn.WriteMessage(websocket.BinaryMessage, []byte{yMessageSync, 0x02, 0x02, 0xC0, 0x01})
			// A barrier is answered after the frame before it, so its ack
			// proves the failed append happened. No applier_ack: the restore's
			// finalization must decide.
			sendBarrier(t, conn, 1)
			select {
			case sent <- struct{}{}:
			default:
			}
		}
	}()
	waitElectable(t, mgr, "item-3749-restore", 1)

	store.mu.Lock()
	store.failOn = "db down"
	store.mu.Unlock()
	applyDone := make(chan error, 1)
	go func() { applyDone <- mgr.ApplyExternalContent("item-3749-restore", "# external") }()
	<-sent
	waitAppendFailed(t, mgr, "item-3749-restore")

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- mgr.ForceRefreshRoom("item-3749-restore",
			func() (int64, int64, error) { return 0, 0, errors.New("commit: rolled back") },
			nil)
	}()
	select {
	case err := <-applyDone:
		if !errors.Is(err, ErrApplierNotDurable) {
			t.Fatalf("a restore finalized a failed bracket as %v, want ErrApplierNotDurable", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("ApplyExternalContent did not return")
	}
	select {
	case <-restoreDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ForceRefreshRoom did not return")
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("a not-durable round-trip was re-applied: %d requests, want 1", n)
	}
}

// waitAppendFailed waits until the room's only conn has recorded a failed
// append.
func waitAppendFailed(t *testing.T, mgr *RoomManager, itemID string) {
	t.Helper()
	room := roomFor(t, mgr, itemID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		room.mu.Lock()
		failed := false
		for _, rc := range room.conns {
			if rc.appendFailSeq.Load() > 0 {
				failed = true
			}
		}
		room.mu.Unlock()
		if failed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no append failure was recorded")
}
