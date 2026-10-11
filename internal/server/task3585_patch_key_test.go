package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/gorilla/websocket"
)

// TASK-3585: idempotency keys on patch.

func (f *patchFixture) patchKey(t *testing.T, key string, body map[string]any) (int, map[string]any, http.Header) {
	t.Helper()
	rr := doRequestWithHeaders(f.srv, "POST", f.path+"/patch", body, map[string]string{"Idempotency-Key": key})
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out, rr.Header()
}

// An insert-shaped edit keeps its anchor, so a plain retry would insert it
// twice. With a key, the retry replays the first answer.
func TestTASK3585_ReplayDoesNotApplyTwice(t *testing.T) {
	f := newPatchFixture(t, testServer(t), "the fox ran\n")
	body := map[string]any{"edits": edits("fox", "fox, then a dog,")}
	code, first, _ := f.patchKey(t, "k-1", body)
	if code != http.StatusOK {
		t.Fatalf("first: %d %v", code, first)
	}
	code, again, hdr := f.patchKey(t, "k-1", body)
	if code != http.StatusOK || hdr.Get("Idempotent-Replayed") != "true" || again["patch_id"] != first["patch_id"] || again["diff"] != first["diff"] {
		t.Fatalf("retry: %d %v %v", code, again, hdr)
	}
	if got := f.current(t).Content; got != "the fox, then a dog, ran\n" {
		t.Fatalf("the retry applied again: %q", got)
	}
	// The same key for a different request is refused.
	code, out, _ := f.patchKey(t, "k-1", map[string]any{"edits": edits("ran", "RAN")})
	if e, _ := out["error"].(map[string]any); code != http.StatusUnprocessableEntity || e["code"] != "idempotency_key_reused" {
		t.Fatalf("reused key: %d %v", code, out)
	}
	// op_key in the body is the same key, and is not part of the request.
	rr := doRequest(f.srv, "POST", f.path+"/patch", map[string]any{"edits": edits("fox", "fox, then a dog,"), "op_key": "k-1"})
	if rr.Code != http.StatusOK || rr.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("op_key: %d %s", rr.Code, rr.Body.String())
	}
}

// Refusals are not stored: a corrected retry with the same key proceeds.
func TestTASK3585_RefusalReleasesTheKey(t *testing.T) {
	f := newPatchFixture(t, testServer(t), "alpha\n")
	if code, out, _ := f.patchKey(t, "k-2", map[string]any{"edits": edits("nope", "x")}); code != http.StatusConflict {
		t.Fatalf("premise refusal: %d %v", code, out)
	}
	if code, out, hdr := f.patchKey(t, "k-2", map[string]any{"edits": edits("alpha", "ALPHA")}); code != http.StatusOK || hdr.Get("Idempotent-Replayed") != "" {
		t.Fatalf("corrected retry: %d %v", code, out)
	}
}

func TestTASK3585_InFlightKeyIsRefused(t *testing.T) {
	f := newPatchFixture(t, testServer(t), "alpha\n")
	// Another request holds the claim right now.
	hash := "held-by-someone-else"
	if _, err := f.srv.store.ClaimIdempotencyKey(f.item.WorkspaceID, "", "k-3", "item.patch", hash, time.Minute); err != nil {
		t.Fatal(err)
	}
	code, out, hdr := f.patchKey(t, "k-3", map[string]any{"edits": edits("alpha", "A")})
	e, _ := out["error"].(map[string]any)
	// A held key with a different hash reads as reused; what matters is that
	// nothing was written and no claim was stolen.
	if code < 400 || (e["code"] != "idempotency_in_flight" && e["code"] != "idempotency_key_reused") || f.current(t).Content != "alpha\n" {
		t.Fatalf("held key: %d %v %v", code, out, hdr)
	}
}

// fakeTab is a patch-capable tab that remembers what it applied (as the
// provider does) and can be told to ack late.
type fakeTab struct {
	conn    *websocket.Conn
	asked   atomic.Int32
	applied atomic.Int32
	acked   atomic.Int32
	lateAck atomic.Bool
	mu      sync.Mutex
	seen    map[string][2]string
	done    chan struct{}
}

func dialFakeTab(t *testing.T, baseURL, itemID, body string) *fakeTab {
	t.Helper()
	u, _ := url.Parse(baseURL)
	c, resp, err := websocket.DefaultDialer.Dial("ws://"+u.Host+"/api/v1/collab/"+itemID+"?applier_bracket=1&patch_capable=1", nil)
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dial: %v (%s)", err, status)
	}
	ft := &fakeTab{conn: c, seen: map[string][2]string{}, done: make(chan struct{})}
	go func() {
		defer close(ft.done)
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var ctl collab.ControlMessage
			if mt != websocket.TextMessage || json.Unmarshal(data, &ctl) != nil || ctl.Type != collab.ControlMessageApplierRequest || ctl.PatchID == "" {
				continue
			}
			ft.asked.Add(1)
			send := func(m collab.ControlMessage) { b, _ := json.Marshal(m); _ = c.WriteMessage(websocket.TextMessage, b) }
			ft.mu.Lock()
			prev, ok := ft.seen[ctl.PatchID]
			ft.mu.Unlock()
			if !ok {
				var es []struct{ Old, New string }
				_ = json.Unmarshal(ctl.Edits, &es)
				after := body
				for _, e := range es {
					after = strings.Replace(after, e.Old, e.New, 1)
				}
				prev = [2]string{body, after}
				ft.applied.Add(1)
				ft.mu.Lock()
				ft.seen[ctl.PatchID] = prev
				ft.mu.Unlock()
			}
			send(collab.ControlMessage{Type: collab.ControlMessageApplierApplyStart, RequestID: ctl.RequestID})
			if !ok {
				_ = c.WriteMessage(websocket.BinaryMessage, []byte{0x00, 0x01, 0x02})
			}
			if ft.lateAck.Load() {
				time.Sleep(400 * time.Millisecond)
			}
			before, after := prev[0], prev[1]
			send(collab.ControlMessage{Type: collab.ControlMessageApplierAck, RequestID: ctl.RequestID, Before: &before, After: &after})
			ft.acked.Add(1)
		}
	}()
	return ft
}

func (ft *fakeTab) close() { _ = ft.conn.Close(); <-ft.done }

func waitPatchCapable(t *testing.T, srv *Server, itemID string) {
	t.Helper()
	deadline := time.Now().Add(applierProbeBudget)
	for !srv.collab.HasElectablePatchApplier(itemID) {
		if time.Now().After(deadline) {
			t.Fatal("no patch-capable applier became electable")
		}
		time.Sleep(applierProbePoll)
	}
}

// The unknown outcome resolves through the SAME connection: its tab re-acks
// the patch it applied, the retry answers 200, and it is applied once.
func TestTASK3585_UnknownResolvesThroughTheSameTab(t *testing.T) {
	restore := collab.SetApplierTimeoutsForTesting(150*time.Millisecond, 150*time.Millisecond)
	t.Cleanup(restore)
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	f := newPatchFixture(t, srv, "the fox ran\n")
	tab := dialFakeTab(t, ts.URL, f.item.ID, f.current(t).Content)
	t.Cleanup(tab.close)
	waitPatchCapable(t, srv, f.item.ID)

	body := map[string]any{"edits": edits("fox", "fox, then a dog,")}
	tab.lateAck.Store(true)
	code, out, _ := f.patchKey(t, "k-u", body)
	if e, _ := out["error"].(map[string]any); code != http.StatusConflict || e["code"] != "content_not_applied" {
		t.Fatalf("premise: the first attempt's outcome is unknown; got %d %v", code, out)
	}
	tab.lateAck.Store(false)
	// The tab's late ack has gone out (as a browser's would have) before the
	// caller retries.
	for deadline := time.Now().Add(5 * time.Second); tab.acked.Load() < 1; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the tab never sent its late ack")
		}
	}
	code, out, _ = f.patchKey(t, "k-u", body)
	if code != http.StatusOK || out["diff"] == nil || out["content_state"] != contentOutcomeAppliedPendingFlush {
		t.Fatalf("retry through the same tab: %d %v", code, out)
	}
	if n := tab.applied.Load(); n != 1 {
		t.Fatalf("the tab applied the patch %d times", n)
	}
	// And now the key replays.
	if code, _, hdr := f.patchKey(t, "k-u", body); code != http.StatusOK || hdr.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay after resolution: %d", code)
	}
}

// The lead's ruling: with that connection gone, the retry is NOT re-planned
// on another tab (an insert-shaped edit keeps its anchor); it answers
// patch_outcome_unknown with the current seq and a diff, and the other tab
// receives nothing.
func TestTASK3585_UnknownWithTheTabGoneIsNotReplanned(t *testing.T) {
	restore := collab.SetApplierTimeoutsForTesting(150*time.Millisecond, 150*time.Millisecond)
	t.Cleanup(restore)
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	f := newPatchFixture(t, srv, "the fox ran\n")
	a := dialFakeTab(t, ts.URL, f.item.ID, f.current(t).Content)
	waitPatchCapable(t, srv, f.item.ID)
	body := map[string]any{"edits": edits("fox", "fox, then a dog,")}
	a.lateAck.Store(true)
	if code, out, _ := f.patchKey(t, "k-g", body); code != http.StatusConflict {
		t.Fatalf("premise unknown: %d %v", code, out)
	}
	a.close()
	b := dialFakeTab(t, ts.URL, f.item.ID, "the fox, then a dog, ran\n") // B holds A's change
	t.Cleanup(b.close)
	waitPatchCapable(t, srv, f.item.ID)

	code, out, _ := f.patchKey(t, "k-g", body)
	e, _ := out["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	if code != http.StatusConflict || e["code"] != "patch_outcome_unknown" || d["seq"] == nil || d["diff"] == nil {
		t.Fatalf("retry with the tab gone: %d %v", code, out)
	}
	time.Sleep(150 * time.Millisecond)
	if n := b.asked.Load(); n != 0 {
		t.Fatalf("another tab was sent the patch %d time(s); it would insert it twice", n)
	}
}
