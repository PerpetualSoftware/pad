package collab

import (
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// runApplierRefuse answers every applier_request with applier_refuse
// (BUG-3542) and records whether each request was guarded.
func runApplierRefuse(conn *websocket.Conn, guardedSeen *atomic.Int64) {
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
			if ctl.Guarded {
				guardedSeen.Add(1)
			}
			payload, _ := json.Marshal(ControlMessage{Type: ControlMessageApplierRefuse, RequestID: ctl.RequestID, Reason: "unconfirmed_edits"})
			if conn.WriteMessage(websocket.TextMessage, payload) != nil {
				return
			}
		}
	}()
}

// A guarded request the applier refuses ends the round-trip with
// ErrApplierRefusedUnconfirmed, carries `guarded` on the wire, and is NOT
// re-elected onto another (willing) applier: another tab applying would
// replace the same edits in the shared document.
func TestBUG3542_RefusedGuardedApplyIsNotReElected(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	refuser := dialWS(t, srv, "item-r")
	defer refuser.Close()
	var guarded atomic.Int64
	runApplierRefuse(refuser, &guarded)
	waitElectable(t, mgr, "item-r", 1)

	// A second, willing applier: it must never be asked.
	willing := dialWS(t, srv, "item-r")
	willingAsked := drainCountingRequests(willing)
	defer willing.Close()
	waitElectable(t, mgr, "item-r", 2)

	done := make(chan error, 1)
	go func() { done <- mgr.ApplyExternalContentGuarded("item-r", "# api", true) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrApplierRefusedUnconfirmed) {
			t.Fatalf("want ErrApplierRefusedUnconfirmed, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a refused apply did not return promptly")
	}
	if guarded.Load() != 1 {
		t.Fatalf("the request was not sent guarded (guarded seen %d)", guarded.Load())
	}
	if n := willingAsked(); n != 0 {
		t.Fatalf("a refused apply was re-elected onto another applier (%d request(s))", n)
	}
}

// An unguarded request (an explicit overwrite) is sent without the flag.
func TestBUG3542_UnguardedRequestCarriesNoFlag(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-u")
	var guarded atomic.Int64
	sawRequest := make(chan struct{}, 1)
	go func() {
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var ctl ControlMessage
			if mt != websocket.TextMessage || json.Unmarshal(data, &ctl) != nil || ctl.Type != ControlMessageApplierRequest {
				continue
			}
			if ctl.Guarded {
				guarded.Add(1)
			}
			ack, _ := json.Marshal(ControlMessage{Type: ControlMessageApplierAck, RequestID: ctl.RequestID})
			_ = conn.WriteMessage(websocket.TextMessage, ack)
			select {
			case sawRequest <- struct{}{}:
			default:
			}
		}
	}()
	defer conn.Close()
	waitElectable(t, mgr, "item-u", 1)
	if err := mgr.ApplyExternalContentGuarded("item-u", "# api", false); err != nil {
		t.Fatalf("unguarded apply: %v", err)
	}
	<-sawRequest
	if guarded.Load() != 0 {
		t.Fatal("an unguarded request carried `guarded`")
	}
}
