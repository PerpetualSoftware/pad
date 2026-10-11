package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/gorilla/websocket"
)

// BUG-3586: a patch whose applier times out must NOT be re-sent to another
// tab. Tab A applies it but acks after the server's timeout; tab B already
// holds A's change (synced), so a re-sent insert-shaped edit (new contains
// old) keeps its anchor there and would be inserted twice. The answer is
// the unknown outcome, and B receives nothing.
func TestBUG3586_TimedOutPatchIsNotResentToAnotherTab(t *testing.T) {
	restore := collab.SetApplierTimeoutsForTesting(150*time.Millisecond, 150*time.Millisecond)
	t.Cleanup(restore)
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	f := newPatchFixture(t, srv, "the fox ran\n")
	u, _ := url.Parse(ts.URL)

	dial := func() *websocket.Conn {
		t.Helper()
		c, resp, err := websocket.DefaultDialer.Dial("ws://"+u.Host+"/api/v1/collab/"+f.item.ID+"?applier_bracket=1&patch_capable=1", nil)
		if err != nil {
			status := ""
			if resp != nil {
				status = resp.Status
			}
			t.Fatalf("dial: %v (%s)", err, status)
		}
		return c
	}
	waitPatchApplier := func() {
		t.Helper()
		deadline := time.Now().Add(applierProbeBudget)
		for !srv.collab.HasElectablePatchApplier(f.item.ID) {
			if time.Now().After(deadline) {
				t.Fatal("no patch-capable applier became electable")
			}
			time.Sleep(applierProbePoll)
		}
	}

	// Tab A, connected first so it is elected first: applies (brackets and
	// sends a frame) but acks only after the server's timeout.
	a := dial()
	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		for {
			mt, data, err := a.ReadMessage()
			if err != nil {
				return
			}
			var ctl collab.ControlMessage
			if mt != websocket.TextMessage || json.Unmarshal(data, &ctl) != nil || ctl.Type != collab.ControlMessageApplierRequest {
				continue
			}
			send := func(m collab.ControlMessage) { b, _ := json.Marshal(m); _ = a.WriteMessage(websocket.TextMessage, b) }
			send(collab.ControlMessage{Type: collab.ControlMessageApplierApplyStart, RequestID: ctl.RequestID})
			_ = a.WriteMessage(websocket.BinaryMessage, []byte{0x00, 0x01, 0x02})
			time.Sleep(400 * time.Millisecond) // past the timeout
			before, after := "the fox ran\n", "the fox, then a dog, ran\n"
			send(collab.ControlMessage{Type: collab.ControlMessageApplierAck, RequestID: ctl.RequestID, Before: &before, After: &after})
		}
	}()
	t.Cleanup(func() { _ = a.Close(); <-aDone })
	waitPatchApplier()
	time.Sleep(20 * time.Millisecond) // A's connectedAt strictly earlier than B's

	// Tab B: patch-capable, counts every applier_request it is sent.
	b := dial()
	var bAsked atomic.Int32
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		for {
			mt, data, err := b.ReadMessage()
			if err != nil {
				return
			}
			var ctl collab.ControlMessage
			if mt == websocket.TextMessage && json.Unmarshal(data, &ctl) == nil && ctl.Type == collab.ControlMessageApplierRequest {
				bAsked.Add(1)
			}
		}
	}()
	t.Cleanup(func() { _ = b.Close(); <-bDone })

	code, out := f.patch(t, map[string]any{"edits": edits("fox", "fox, then a dog,")})
	time.Sleep(200 * time.Millisecond)
	if n := bAsked.Load(); n != 0 {
		t.Fatalf("the timed-out patch was re-sent to another tab %d time(s); an insert-shaped edit would apply twice", n)
	}
	e, _ := out["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	if code != http.StatusConflict || e["code"] != "content_not_applied" || d["content_outcome"] != contentOutcomeUnknown {
		t.Fatalf("want the unknown outcome, got %d %v", code, out)
	}
}
