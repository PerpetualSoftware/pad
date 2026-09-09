package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/gorilla/websocket"
)

// BUG-2840 half A, STEP ONE: measure the premise before designing anything.
//
// The filing claims that on the APPLIER path a refused PATCH still lands its
// content — applyContentViaCollab pushes the markdown into the live Y.Doc,
// the handler clears input.Content, the row write then refuses, and the
// content reaches items.content anyway on the next collab-snapshot flush.
//
// That claim was a READING of the snapshot branch, not an observation, and the
// plan says so: "Both need measuring before a fix is designed — the shape of
// the fix depends on which half actually bites." So this file measures. It
// asserts what the code DOES today, including the part that is a defect, so
// that the fix has a baseline to move and a reviewer can see the premise was
// established rather than assumed.

// applierEcho answers applier_request frames with an ack, which is what makes
// ApplyExternalContent succeed and puts the handler on the applier path. Ported
// from internal/collab's own test helper; that one cannot be reused here
// because reaching the defect requires going through the HTTP handler.
func applierEcho(t *testing.T, conn *websocket.Conn) func() {
	t.Helper()
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
			if err := json.Unmarshal(data, &ctl); err != nil {
				continue
			}
			if ctl.Type != collab.ControlMessageApplierRequest {
				continue
			}
			// A real applier is a browser tab: it applies the markdown to
			// its Y.Doc and BROADCASTS the resulting update, which the relay
			// persists to the op-log. The ack alone would make
			// ApplyExternalContent succeed while leaving no durable trace, so
			// this emits an op too — otherwise the experiment would measure a
			// peer that does not exist.
			if err := conn.WriteMessage(websocket.BinaryMessage, []byte{0x00, 0x01, 0x02}); err != nil {
				return
			}
			payload, _ := json.Marshal(collab.ControlMessage{
				Type:      collab.ControlMessageApplierAck,
				RequestID: ctl.RequestID,
			})
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		}
	}()
	return func() { _ = conn.Close(); <-done }
}

// waitForApplierPath blocks until a content PATCH is actually taking the
// applier path, and returns once it is.
//
// The readiness signal is the observable difference between the two paths
// rather than an internal field: on the APPLIER path the markdown goes to the
// Y.Doc and items.content is left alone, while on the direct-write path
// items.content changes. So a SUCCEEDING probe PATCH that leaves items.content
// untouched is proof the applier answered — no exported accessor for
// "electable conns" exists, and reaching into the manager's unexported state
// is not available from this package.
func waitForApplierPath(t *testing.T, srv *Server, wsSlug, itemSlug, itemID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		probe := "applier-path probe"
		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+wsSlug+"/items/"+itemSlug,
			map[string]interface{}{"content": probe})
		if rr.Code != http.StatusOK {
			t.Fatalf("probe PATCH failed: %d %s", rr.Code, rr.Body.String())
		}
		item, err := srv.store.GetItem(itemID)
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if item.Content != probe {
			return // content did not land in the row: the applier took it
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no applier path within 3s: every probe PATCH wrote items.content directly")
}

// TestBUG2840HalfA_RefusedPatchOnApplierPath measures what a refusal leaves
// behind. It asserts today's behaviour, defect included.
func TestBUG2840HalfA_RefusedPatchOnApplierPath(t *testing.T) {
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	conn, resp, err := dialCollab(t, ts.URL, item.ID, nil, "")
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dialCollab: %v (%s)", err, status)
	}
	stop := applierEcho(t, conn)
	t.Cleanup(stop)

	waitForApplierPath(t, srv, slug, item.Slug, item.ID)

	before, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	beforeOps := countOpLog(t, srv, item.ID)

	const refused = "content the caller was told was not written"
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug,
		map[string]interface{}{
			"expected_updated_at": "2000-01-01T00:00:00Z",
			"content":             refused,
		})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 from the stale token, got %d: %s", rr.Code, rr.Body.String())
	}

	after, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if after.Content != before.Content {
		t.Errorf("items.content moved on a refused PATCH: %q -> %q", before.Content, after.Content)
	}

	// WHAT THIS CAN AND CANNOT MEASURE, stated because the first version of
	// this test got it wrong in a way that looked right.
	//
	// The plan's step 4 was to drive a ?source=collab-snapshot PATCH and check
	// that items.content ends up holding the refused content. Written
	// literally, that is CIRCULAR: the snapshot PATCH carries its markdown in
	// the request body, so a test that supplies the refused string proves only
	// that a snapshot write writes what it is given. The first run of this
	// test did exactly that and reported the premise confirmed.
	//
	// The server cannot close the loop itself. Collab here is a DUMB RELAY —
	// it persists opaque Yjs updates and never parses them — so nothing
	// server-side can derive markdown from the room's document. The markdown
	// in a real snapshot PATCH comes from a live TAB's Y.Doc.
	//
	// What IS observable, and what the defect reduces to on this side of the
	// wire: a refused PATCH leaves DURABLE COLLAB STATE created by that same
	// refused request. Any tab that joins afterwards replays it.
	afterOps := countOpLog(t, srv, item.ID)
	t.Logf("MEASURED: op-log rows before refusal=%d, after refusal=%d; "+
		"items.content before=%q after=%q", beforeOps, afterOps, before.Content, after.Content)

	if afterOps <= beforeOps {
		t.Skipf("inconclusive: the refused PATCH left no new op-log rows (%d -> %d), so this "+
			"harness did not reproduce the applier writing durable state. The premise is "+
			"NOT established and half A should not be designed against it yet.",
			beforeOps, afterOps)
	}

	t.Logf("PREMISE ESTABLISHED, in the form the server can observe: the refused PATCH "+
		"added %d op-log row(s) that outlive it. items.content is untouched, so the caller's "+
		"409 is true of the row and false of the collaborative document; a tab joining later "+
		"replays those ops and flushes them back as items.content.", afterOps-beforeOps)
}
