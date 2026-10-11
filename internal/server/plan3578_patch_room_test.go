package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/metrics"
	"github.com/gorilla/websocket"
	dto "github.com/prometheus/client_model/go"
)

// PLAN-3578 step 2: a patch to an item open in a patch-capable editor goes to
// that tab, which applies it and reports the canonical before/after; a tab
// refusal comes back typed and counted.

// patchTab is a fake patch-capable editor. refuse, when set, is the reason it
// refuses with; otherwise it applies the edits to `body` as text and acks.
func patchTab(t *testing.T, baseURL, itemID string, body string, refuse string) func() {
	t.Helper()
	u, _ := url.Parse(baseURL)
	conn, resp, err := websocket.DefaultDialer.Dial("ws://"+u.Host+"/api/v1/collab/"+itemID+"?applier_bracket=1&patch_capable=1", nil)
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dial: %v (%s)", err, status)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			var ctl collab.ControlMessage
			if json.Unmarshal(data, &ctl) != nil || ctl.Type != collab.ControlMessageApplierRequest || ctl.PatchID == "" {
				continue
			}
			send := func(m collab.ControlMessage) {
				b, _ := json.Marshal(m)
				_ = conn.WriteMessage(websocket.TextMessage, b)
			}
			if refuse != "" {
				send(collab.ControlMessage{Type: collab.ControlMessageApplierRefuse, RequestID: ctl.RequestID, Reason: refuse, Line: 3})
				continue
			}
			var edits []struct{ Old, New string }
			_ = json.Unmarshal(ctl.Edits, &edits)
			after := body
			for _, e := range edits {
				after = strings.Replace(after, e.Old, e.New, 1)
			}
			before := body
			send(collab.ControlMessage{Type: collab.ControlMessageApplierApplyStart, RequestID: ctl.RequestID})
			_ = conn.WriteMessage(websocket.BinaryMessage, []byte{0x00, 0x01, 0x02})
			send(collab.ControlMessage{Type: collab.ControlMessageApplierAck, RequestID: ctl.RequestID, Before: &before, After: &after})
		}
	}()
	return func() { _ = conn.Close(); <-done }
}

func patchRoomFixture(t *testing.T, refuse string) (*patchFixture, *metrics.Metrics) {
	t.Helper()
	srv := testServerWithCollab(t)
	m := metrics.New()
	srv.SetMetrics(m)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	f := newPatchFixture(t, srv, "alpha\nbeta\n")
	stop := patchTab(t, ts.URL, f.item.ID, f.current(t).Content, refuse)
	t.Cleanup(stop)
	// Readiness, not an assertion: the capability flag lands at registration.
	deadline := time.Now().Add(applierProbeBudget)
	for !srv.collab.HasElectablePatchApplier(f.item.ID) {
		if time.Now().After(deadline) {
			t.Fatal("no patch-capable applier became electable")
		}
		time.Sleep(applierProbePoll)
	}
	return f, m
}

func TestPLAN3578_PatchThroughAnOpenEditor(t *testing.T) {
	f, _ := patchRoomFixture(t, "")
	before := f.current(t)
	code, out := f.patch(t, map[string]any{"edits": edits("beta", "BETA")})
	if code != http.StatusOK {
		t.Fatalf("patch: %d %v", code, out)
	}
	if d, _ := out["diff"].(string); !strings.Contains(d, "-beta") || !strings.Contains(d, "+BETA") {
		t.Fatalf("diff = %q", d)
	}
	if out["content_state"] != contentOutcomeAppliedPendingFlush {
		t.Fatalf("an applier-path patch must say the row catches up later: %v", out)
	}
	if v, _ := out["version_id"].(string); v == "" {
		t.Fatalf("the patch wrote no version row attributed to the request: %v", out)
	}
	if after := f.current(t); after.Seq == before.Seq {
		t.Fatal("the patch's row write did not move the seq, so it cannot join the chain")
	}
	// PLAN-3578 step 3: the room path records the patch for undo too.
	if rec, err := f.srv.store.GetItemPatch(f.item.WorkspaceID, out["patch_id"].(string)); err != nil || rec == nil || rec.Edits == "" {
		t.Fatalf("no undo record for an applier-path patch: %+v %v", rec, err)
	}
}

func TestPLAN3578_TabRefusalIsTypedAndCounted(t *testing.T) {
	f, m := patchRoomFixture(t, "unstable_roundtrip")
	body := f.current(t).Content
	code, out := f.patch(t, map[string]any{"edits": edits("beta", "BETA")})
	e, _ := out["error"].(map[string]any)
	if code != http.StatusConflict || e["code"] != "patch_unstable_roundtrip" {
		t.Fatalf("got %d %v", code, out)
	}
	if d, _ := e["details"].(map[string]any); d["line"] != float64(3) {
		t.Fatalf("details = %v", e["details"])
	}
	if f.current(t).Content != body {
		t.Fatal("a refused patch changed the stored body")
	}
	var c dto.Metric
	_ = m.PatchRefusalsTotal.WithLabelValues("unstable_roundtrip").Write(&c)
	if c.GetCounter().GetValue() != 1 {
		t.Fatalf("refusal not counted: %v", c.GetCounter().GetValue())
	}
}

// The property the capability filter carries, and its failure mode: an older
// tab reads an edits-only applier_request as markdown "" and would replace
// the document with nothing. A room whose ONLY live tab is not
// patch-capable must answer patch_room_open, send that tab no
// applier_request frame at all, and leave items.content alone.
func TestPLAN3578_NonCapableTabIsNeverSentAPatch(t *testing.T) {
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	f := newPatchFixture(t, srv, "alpha\nbeta\n")
	body := f.current(t).Content

	// An older tab: bracket-capable, NOT patch-capable. It records every
	// applier_request it receives and answers none of them.
	u, _ := url.Parse(ts.URL)
	conn, resp, err := websocket.DefaultDialer.Dial("ws://"+u.Host+"/api/v1/collab/"+f.item.ID+"?applier_bracket=1", nil)
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dial: %v (%s)", err, status)
	}
	var requests atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var ctl collab.ControlMessage
			if mt == websocket.TextMessage && json.Unmarshal(data, &ctl) == nil && ctl.Type == collab.ControlMessageApplierRequest {
				requests.Add(1)
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close(); <-done })
	waitForApplierPath(t, srv, f.slug, f.item.Slug, f.item.ID)
	if srv.collab.HasElectablePatchApplier(f.item.ID) {
		t.Fatal("premise: an older tab must not count as a patch applier")
	}

	code, out := f.patch(t, map[string]any{"edits": edits("beta", "BETA")})
	e, _ := out["error"].(map[string]any)
	if code != http.StatusConflict || e["code"] != "patch_room_open" {
		t.Fatalf("(1) want 409 patch_room_open, got %d %v", code, out)
	}
	// Give any frame that WAS sent time to arrive before counting.
	time.Sleep(200 * time.Millisecond)
	if n := requests.Load(); n != 0 {
		t.Fatalf("(2) the non-capable tab received %d applier_request frame(s); an older tab would apply an empty body", n)
	}
	if got := f.current(t).Content; got != body {
		t.Fatalf("(3) items.content changed: %q", got)
	}
}
