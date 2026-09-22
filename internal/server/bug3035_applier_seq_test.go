package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BUG-3035: `pad item edit` now round-trips the `seq` that seeded the editor as
// `expected_seq`, and the whole design rests on one server claim — that the seq
// guard refuses on the APPLIER path too, BEFORE the content reaches the live
// document. That is the path the bug is about (an open tab), and the only prior
// pin of refuse-before-apply used the weak `expected_updated_at` token
// (TestBUG2840HalfA_RefusedPatchLeavesTheDocumentUntouched).
//
// Two legs against one room. The ACCEPT leg is not decoration: it proves this
// room really routes a content PATCH through the applier (the 200 carries
// content_outcome=applied_pending_flush), so the refusal leg's 409 is a statement
// about that path rather than about a direct write that happened to refuse.
func TestBUG3035_StaleSeqRefusesOnTheApplierPathBeforeApplying(t *testing.T) {
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

	seeded, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if seeded.Seq < 1 {
		t.Fatalf("a live row must carry seq >= 1; got %d", seeded.Seq)
	}

	// ACCEPT leg: the seq the editor was seeded with is current, so the save lands
	// — through the applier.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug,
		map[string]interface{}{"expected_seq": seeded.Seq, "content": "first save"})
	if rr.Code != http.StatusOK {
		t.Fatalf("current seq must be accepted; got %d: %s", rr.Code, rr.Body.String())
	}
	var ok struct {
		Warnings map[string]any `json:"warnings"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &ok)
	if outcome, _ := ok.Warnings["content_outcome"].(string); outcome != contentOutcomeAppliedPendingFlush {
		t.Fatalf("the accepted save did not take the applier path (content_outcome=%q), so the "+
			"refusal below would say nothing about it", outcome)
	}

	// REFUSE leg: a second editor seeded from the SAME read saves after the first.
	// Its seq is now stale.
	beforeOps := countOpLog(t, srv, item.ID)
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug,
		map[string]interface{}{"expected_seq": seeded.Seq, "content": "second save, seeded stale"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("a stale seq on the applier path must be refused 409; got %d: %s", rr.Code, rr.Body.String())
	}
	var refusal struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &refusal)
	if refusal.Error.Code != "update_conflict" || refusal.Error.Details["conflict_type"] != "seq" {
		t.Fatalf("want update_conflict on the seq token; got %s", rr.Body.String())
	}
	// Nothing reached the document: op-log rows unchanged is what that reduces to
	// on a dumb relay (see the BUG-2840 test for why the server cannot read the
	// document itself).
	if afterOps := countOpLog(t, srv, item.ID); afterOps != beforeOps {
		t.Fatalf("the refused save left %d op-log row(s) behind; a tab joining later would replay "+
			"the content the caller was told was refused", afterOps-beforeOps)
	}
}
