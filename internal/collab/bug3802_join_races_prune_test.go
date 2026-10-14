package collab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// BUG-3802 (BUG-3746 residual, Heron's ruling): a prune-and-replace write
// stamps the in-memory seed boundary (SetLastRestoreSeq) inside PruneAndApply's
// applyFn, and Join checks that boundary under the same per-item lock. So a
// tab seeded before the write that joins WHILE the write is in flight must
// wait for it, and then be refused; it must never be judged against the
// boundary as it stood before the write. Only PruneAndApply's no-room branch
// runs here; the room-with-peers branch takes the same lock at the same
// point, at the top of the function.
func TestBUG3802_JoinRacingAPruneAndReplaceWaitsAndIsFenced(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()
	// A server of its own, which signals immediately before calling Join, so
	// the test knows the racing join is IN Join (and so at, or about to wait
	// on, the item lock) before it starts the silence window (Codex r1: the
	// handshake completes before the handler reaches Join, so without this a
	// slow handler could let the write finish first and pass by luck).
	entering := make(chan string, 4)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		seq, _ := strconv.ParseInt(r.URL.Query().Get("content_seq"), 10, 64)
		entering <- r.URL.Query().Get("content_seq")
		_ = mgr.Join(strings.TrimPrefix(r.URL.Path, "/ws/"), conn, 0, seq, true, false, nil)
	}))
	defer srv.Close()

	const item = "item-3802"
	inside := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- mgr.PruneAndApply(item, func() error {
			// What the server's prune-and-replace does inside applyFn
			// (noteSeedBoundary): the write committed at seq 20.
			mgr.SetLastRestoreSeq(item, 20)
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside

	// A tab seeded at seq 10, before the write, joins while it is in flight.
	stale := dialWSContentSeq(t, srv, item, 10)
	defer stale.Close()
	select {
	case <-entering:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the racing join never reached Join")
	}

	// The join's first control frame, read once (a gorilla read error is
	// permanent, so no read deadline is used to probe for silence).
	type frame struct {
		ctl ControlMessage
		err error
	}
	first := make(chan frame, 1)
	go func() {
		_ = stale.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			mt, data, err := stale.ReadMessage()
			if err != nil {
				first <- frame{err: err}
				return
			}
			if mt == websocket.TextMessage {
				var msg ControlMessage
				uerr := json.Unmarshal(data, &msg)
				first <- frame{ctl: msg, err: uerr}
				return
			}
		}
	}()

	// While the write holds the item lock, the join gets no answer.
	select {
	case f := <-first:
		close(release)
		t.Fatalf("the racing join was answered (%q, err %v) while the write was in flight: it did not wait for the item lock", f.ctl.Type, f.err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("PruneAndApply: %v", err)
	}
	f := <-first
	if f.err != nil {
		t.Fatalf("reading the racing join's first frame: %v", f.err)
	}
	if f.ctl.Type != ControlMessageForceRefresh {
		t.Fatalf("the racing stale join got %q, want %q", f.ctl.Type, ControlMessageForceRefresh)
	}

	// Control: a tab seeded from the write itself is admitted afterwards.
	fresh := dialWSContentSeq(t, srv, item, 20)
	defer fresh.Close()
	<-entering
	if ctl := readControlWithin(t, fresh, 2*time.Second); ctl.Type == ControlMessageForceRefresh {
		t.Fatalf("a tab seeded at the new seq was force-refreshed: the fence over-fences")
	}
}
