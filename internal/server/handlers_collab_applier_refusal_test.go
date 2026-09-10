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

// BUG-2840 half A: this file MEASURED the defect, and now asserts the property that
// replaced it (PLAN-2975 unit 2).
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

// waitForApplierPath blocks until a content PATCH would actually take the applier
// path, and returns once it would.
//
// It asks the routing predicate the handler itself uses — RoomManager.HasElectableApplier,
// added by TASK-2987 — rather than probing with real PATCHes.
//
// THE PROBE LOOP THIS REPLACES, and why it had to go (BUG-2995). It PATCHed every
// few milliseconds and inferred the route from whether items.content changed, with a
// comment explaining that no exported accessor for electable conns existed. That was
// true when it was written and stopped being true at TASK-2987, which added the
// accessor precisely so the handler could learn the route without taking it.
//
// Leaving it as a write loop was not merely wasteful. Those writes go through the
// ordinary API rate limiter (600/min, burst 60) and the bucket is shared across the
// package, so each caller of this helper drains it for the tests that follow. With
// one caller it stayed under the burst; a second caller (BUG-2995's) pushed a THIRD
// caller — TestApplierPathRefusalLeavesNoOpLogRow_OpenChildren, which was green on
// main and had nothing to do with the change — over the edge in CI, where it spent a
// full 15s budget being answered 429 and failed. A readiness helper that consumes a
// shared, exhaustible resource makes unrelated tests fail as a function of how many
// other tests ran first, which is the worst kind of flake to diagnose from a log.
//
// Asking the predicate directly costs nothing, cannot be rate-limited, and answers
// the exact question instead of inferring it from a side effect.
func waitForApplierPath(t *testing.T, srv *Server, wsSlug, itemSlug, itemID string) {
	t.Helper()
	if srv.collab == nil {
		t.Fatal("waitForApplierPath: server has no collab manager; the applier path cannot exist")
	}
	deadline := time.Now().Add(applierProbeBudget)
	for time.Now().Before(deadline) {
		if srv.collab.HasElectableApplier(itemID) {
			return
		}
		time.Sleep(applierProbePoll)
	}
	t.Fatalf("no electable applier for item %s within %s: a content PATCH would take the "+
		"direct-write path, so this test cannot measure applier-path behaviour", itemID,
		applierProbeBudget)
}

// Probe pacing. The budget is a READINESS wait, not an assertion — every millisecond
// of it is spent only while the room has no electable conn, and a passing run returns
// in a few polls. A too-short budget does not catch a product bug; it invents a flaky
// test (BUG-2995: election lost the race in 2 runs of 5 against 3s).
const (
	applierProbePoll   = 5 * time.Millisecond
	applierProbeBudget = 15 * time.Second
)

// TestBUG2840HalfA_RefusedPatchLeavesTheDocumentUntouched asserts the property the
// reorder buys: a refused PATCH on the applier path changes NOTHING — not the row,
// and not the collaborative document.
//
// Until PLAN-2975 unit 2 this same test asserted the DEFECT (that the refused request
// left op-log rows behind) and skipped if it could not reproduce it. The inversion is
// deliberate and is the only honest way to reuse it: leaving the old assertion in
// place would have turned the fix into a SKIP, and a skip reads as a pass in the
// summary line.
func TestBUG2840HalfA_RefusedPatchLeavesTheDocumentUntouched(t *testing.T) {
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
	t.Logf("op-log rows before refusal=%d, after refusal=%d; items.content before=%q after=%q",
		beforeOps, afterOps, before.Content, after.Content)

	// THE PROPERTY. Before the reorder the refused request pushed its markdown into
	// the live Y.Doc first, and the relay persisted the resulting update — so the
	// refusal left DURABLE collab state that any later joiner replays and flushes
	// back into items.content. The caller's 409 was true of the row and false of the
	// document.
	//
	// Now the row write runs FIRST and refuses before ApplyExternalContent is ever
	// called, so there is nothing to persist. Op-log rows unchanged is what "the
	// document did not move" reduces to on this side of the wire: the server cannot
	// read the document itself, because collab here is a dumb relay that never parses
	// the opaque Yjs updates it stores.
	if afterOps != beforeOps {
		t.Errorf("a refused PATCH left %d new op-log row(s) behind (%d -> %d). The refusal is true of "+
			"the row and false of the collaborative document: a tab joining later replays those ops "+
			"and flushes them back as items.content, landing the content the caller was told was "+
			"rejected.", afterOps-beforeOps, beforeOps, afterOps)
	}
}
