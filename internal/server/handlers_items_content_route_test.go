package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/gorilla/websocket"
)

// Route tests for PLAN-2975's write-first-apply-second ordering.
//
// The lexical guard in handlers_items_title_test.go proves the refusal ARMS are
// present, ordered and reachable in every block. These prove the ORDERING: that a
// refusal on the applier path leaves the collaborative document alone, and that an
// apply which fails after the row write says so instead of answering success.

// silentApplier accepts the applier_request and never acks, so the round-trip runs
// out its (shrunk) budget and ApplyExternalContent fails — the condition
// content_not_applied exists to answer. It deliberately does NOT emit an op, so a
// failure to apply leaves no durable trace either.
func silentApplier(t *testing.T, conn *websocket.Conn) func() {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	return func() { _ = conn.Close(); <-done }
}

func decodeErrorEnvelope(t *testing.T, body []byte) (code string, details map[string]any) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %s)", err, body)
	}
	return env.Error.Code, env.Error.Details
}

// TestApplierPathRefusalLeavesNoOpLogRow_OpenChildren is the SECOND refusal arm
// driven behaviourally.
//
// The measurement harness covers the optimistic-concurrency arm. This covers the
// open-children guard, which reaches the store through a different mechanism (a
// precheck inside the write transaction rather than a token comparison), so a reorder
// that happened to order one correctly and not the other would show up here.
//
// BOUNDARY, stated rather than left as an apparent gap: the other two arms
// (rename-cascade-too-large, invalid title) have no HTTP trigger on this path — the
// handlers' own pre-checks catch every title refusal reachable over the wire, and the
// store-sourced versions fire only on a concurrent rename inside the lock window,
// which needs a store seam that does not exist. Those two are covered lexically by
// TestUpdateItemErrorBlocksMapEveryStoreRefusal, and that is the whole coverage they
// have.
func TestApplierPathRefusalLeavesNoOpLogRow_OpenChildren(t *testing.T) {
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	slug := createWSWithCollections(t, srv)

	// The link is established at CREATE time via fields.parent, which is the shape
	// the guard's own tests use; a parent_id PATCH answers 200 and leaves no link,
	// so a hand-rolled version of this setup measures an unguarded parent.
	parent, _ := seedParentAndChildren(t, srv, slug, []string{"open"})

	conn, resp, err := dialCollab(t, ts.URL, parent.ID, nil, "")
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dialCollab: %v (%s)", err, status)
	}
	stop := applierEcho(t, conn)
	t.Cleanup(stop)
	waitForApplierPath(t, srv, slug, parent.Ref, parent.ID)

	before, err := srv.store.GetItem(parent.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	beforeOps := countOpLog(t, srv, parent.ID)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+parent.Ref,
		map[string]interface{}{
			"fields":  `{"status":"completed"}`,
			"content": "content the caller was told was not written",
		})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected the open-children guard to refuse with 409, got %d: %s", rr.Code, rr.Body.String())
	}
	if code, _ := decodeErrorEnvelope(t, rr.Body.Bytes()); code != "open_children" {
		t.Fatalf("want code open_children, got %q", code)
	}

	after, err := srv.store.GetItem(parent.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if after.Content != before.Content {
		t.Errorf("items.content moved on a refused PATCH: %q -> %q", before.Content, after.Content)
	}
	if afterOps := countOpLog(t, srv, parent.ID); afterOps != beforeOps {
		t.Errorf("the open-children refusal left %d new op-log row(s) (%d -> %d): the content reached "+
			"the live document even though the caller was told the write was refused",
			afterOps-beforeOps, beforeOps, afterOps)
	}
}

// TestApplyFailureAfterRowWriteAnswersContentNotApplied pins the hybrid outcome the
// reorder creates and the ruled answer to it.
//
// The pre-check is a HINT: it can say yes and the apply can still fail. When it does,
// the row write has already committed, so the response must say BOTH halves — the
// fields landed, the content did not — and must not be a 2xx a client can read as
// success.
func TestApplyFailureAfterRowWriteAnswersContentNotApplied(t *testing.T) {
	restore := collab.SetApplierTimeoutsForTesting(150*time.Millisecond, 150*time.Millisecond)
	t.Cleanup(restore)

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
	stop := silentApplier(t, conn)
	t.Cleanup(stop)

	// The conn must be electable, or this measures the direct-write path instead.
	deadline := time.Now().Add(3 * time.Second)
	for !srv.collab.HasElectableApplier(item.ID) {
		if time.Now().After(deadline) {
			t.Fatal("the conn never became electable; this test would have measured the direct path")
		}
		time.Sleep(2 * time.Millisecond)
	}

	before, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug,
		map[string]interface{}{
			"title":   "Renamed by the same request",
			"content": "content that never reaches the document",
		})
	if rr.Code != http.StatusConflict {
		t.Fatalf("an apply that fails after the row write must not answer %d — a 2xx here reads as "+
			"success while the content is not in the document. Body: %s", rr.Code, rr.Body.String())
	}
	code, details := decodeErrorEnvelope(t, rr.Body.Bytes())
	if code != "content_not_applied" {
		t.Fatalf("want code content_not_applied, got %q (body %s)", code, rr.Body.String())
	}
	if landed, _ := details["content_landed"].(bool); landed {
		t.Error("content_landed must be false: the whole point of the code is that it did not")
	}
	if _, ok := details["actual_updated_at"].(string); !ok {
		t.Error("actual_updated_at missing: without it a content-only retry trips OCC on a timestamp " +
			"this very request moved")
	}
	fields, _ := details["landed_fields"].([]any)
	var sawTitle bool
	for _, f := range fields {
		if s, _ := f.(string); s == "title" {
			sawTitle = true
		}
	}
	if !sawTitle {
		t.Errorf("landed_fields %v does not name the title this request DID store; the caller cannot "+
			"tell which half of its PATCH is done", fields)
	}

	// The row half really did land — otherwise the 409 would be honest by accident.
	after, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if after.Title == before.Title {
		t.Errorf("the row write did not land (title still %q), so this test proved nothing about the "+
			"hybrid outcome it exists to pin", after.Title)
	}
	if after.Content != before.Content {
		t.Errorf("items.content moved (%q -> %q) even though the apply failed", before.Content, after.Content)
	}
}

// TestSettleContentRouteBoundsTheStandoff covers the decision the end-to-end tests
// cannot reach.
//
// The standoff state — a room whose only writer has joined but not finished replaying
// — is not constructible from this package without a lever into the conn anchoring
// machinery, which PLAN-2975 fences off as its own unit. So the decision is tested
// where it lives, with the two I/O calls injected.
func TestSettleContentRouteBoundsTheStandoff(t *testing.T) {
	t.Run("elects an applier immediately when one exists", func(t *testing.T) {
		calls := 0
		out, err := settleContentRoute(
			func() bool { return true },
			func() error { calls++; return nil },
			time.Second, time.Millisecond,
		)
		if out != settleElectApplier || err != nil {
			t.Fatalf("want settleElectApplier/nil, got %v/%v", out, err)
		}
		if calls != 0 {
			t.Errorf("the direct write must not be attempted when an applier is electable; called %d times", calls)
		}
	})

	t.Run("writes directly when the room has no live writer", func(t *testing.T) {
		out, err := settleContentRoute(
			func() bool { return false },
			func() error { return nil },
			time.Second, time.Millisecond,
		)
		if out != settleDirectWrote || err != nil {
			t.Fatalf("want settleDirectWrote/nil, got %v/%v", out, err)
		}
	})

	t.Run("gives up on a room that never settles, having written nothing", func(t *testing.T) {
		attempts := 0
		start := time.Now()
		out, err := settleContentRoute(
			func() bool { return false },
			func() error { attempts++; return collab.ErrRoomActiveDuringPrune },
			60*time.Millisecond, 5*time.Millisecond,
		)
		if out != settleUnsettled || err != nil {
			t.Fatalf("want settleUnsettled/nil, got %v/%v", out, err)
		}
		if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
			t.Errorf("gave up after %s, before the budget expired: a room that would have settled is "+
				"refused early", elapsed)
		}
		if attempts < 2 {
			t.Errorf("only %d attempt(s): the budget must be spent RE-DECIDING, not sleeping once", attempts)
		}
	})

	t.Run("takes the applier path when the writer anchors inside the budget", func(t *testing.T) {
		attempts := 0
		out, err := settleContentRoute(
			func() bool { return attempts >= 2 },
			func() error { attempts++; return collab.ErrRoomActiveDuringPrune },
			time.Second, time.Millisecond,
		)
		if out != settleElectApplier || err != nil {
			t.Fatalf("a writer that anchors inside the budget must route to the applier, got %v/%v", out, err)
		}
	})

	t.Run("surfaces a direct-write failure instead of retrying it", func(t *testing.T) {
		boom := errors.New("store exploded")
		attempts := 0
		out, err := settleContentRoute(
			func() bool { return false },
			func() error { attempts++; return boom },
			time.Second, time.Millisecond,
		)
		if out != settleDirectFailed || !errors.Is(err, boom) {
			t.Fatalf("want settleDirectFailed/boom, got %v/%v", out, err)
		}
		if attempts != 1 {
			t.Errorf("a failure that is not the standoff must not be retried; attempted %d times", attempts)
		}
	})
}

// TestApplierSettleBudgetCoversTheMeasuredAnchoringWindow pins the CONSTANT, which
// every other test in this file bypasses by passing its own budget.
//
// Found by mutation: setting applierSettleBudget to 0 survived the whole suite. A
// zero budget makes settleContentRoute refuse on its first pass, so every room with a
// writer still anchoring answers room_settling instead of waiting the few milliseconds
// it needs — correct in the sense that nothing is written, and useless in the sense
// that the retryable refusal is the normal answer.
//
// The floor is the measurement the budget was sized from (TASK-2989, real store and
// real WS chain, n=5 per bucket): worst single observation 46.41ms at 5000 op-log
// rows. A budget below that refuses rooms the measurement says would have settled.
// The ceiling is judgement, not measurement: a PATCH that blocks for seconds is worse
// than one that asks the caller to retry.
func TestApplierSettleBudgetCoversTheMeasuredAnchoringWindow(t *testing.T) {
	const measuredWorstAnchor = 47 * time.Millisecond

	if applierSettleBudget < measuredWorstAnchor {
		t.Errorf("applierSettleBudget is %s, below the %s worst anchoring time measured for this "+
			"deployment: a room that the measurement says would have settled is refused instead",
			applierSettleBudget, measuredWorstAnchor)
	}
	if applierSettleBudget > 5*time.Second {
		t.Errorf("applierSettleBudget is %s: a content PATCH that blocks this long is worse than a "+
			"retryable refusal", applierSettleBudget)
	}
	if applierSettlePoll <= 0 || applierSettlePoll >= applierSettleBudget {
		t.Errorf("applierSettlePoll %s must be positive and smaller than the budget %s, or the budget "+
			"is spent sleeping rather than re-deciding", applierSettlePoll, applierSettleBudget)
	}
}
