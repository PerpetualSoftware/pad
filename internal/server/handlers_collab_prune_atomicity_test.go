package server

import (
	"net/http"
	"testing"
)

// BUG-2840 half B. On the no-room / no-applier path a content PATCH prunes the
// item's Yjs op-log and writes items.content. The prune used to run FIRST, in
// its own statement, justified by the claim that "any prior collab state is
// strictly older than the items.content the caller is about to write".
//
// That premise is false whenever the write then REFUSES: the ops are destroyed
// and nothing replaces them. It is not a hypothetical on this branch — it fires
// on ErrNoApplierAvailable, a room inside its grace TTL with zero connections,
// which is precisely the state where the op-log holds a closed tab's edits that
// never reached items.content. Four typed refusals can come out of that write
// (the open-children guard, the optimistic-concurrency conflict, the
// rename-cascade byte refusal, and the item-title refusal), so the window is a
// property of the ORDERING rather than of any one of them.
//
// The property, as the lead set it: a refused row write leaves no op that a
// later flush applies — i.e. a refusal must leave the op-log exactly as it
// found it, neither pruned nor added to.
//
// countOpLog reports how many op-log rows an item currently has.
func countOpLog(t *testing.T, srv *Server, itemID string) int {
	t.Helper()
	updates, err := srv.store.LoadYjsUpdatesSince(itemID, 0)
	if err != nil {
		t.Fatalf("LoadYjsUpdatesSince: %v", err)
	}
	return len(updates)
}

// seedOpLog appends rows standing in for a closed tab's unflushed edits —
// content that exists ONLY in the op-log, because items.content was never
// updated for it.
func seedOpLog(t *testing.T, srv *Server, itemID string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := srv.store.AppendYjsUpdate(itemID, []byte{0x00, byte(i)}, "1"); err != nil {
			t.Fatalf("AppendYjsUpdate: %v", err)
		}
	}
	if got := countOpLog(t, srv, itemID); got != n {
		t.Fatalf("seeded %d op-log rows, store reports %d", n, got)
	}
}

func TestCollabDirectWrite_RefusedWriteLeavesTheOpLogIntact(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	seedOpLog(t, srv, item.ID, 3)

	// A stale expected_updated_at makes the row write refuse deterministically
	// (TASK-2022). The PATCH carries CONTENT, so it routes through the collab
	// branch, and with no room and no applier it takes the direct-write path
	// that prunes.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"expected_updated_at": "2000-01-01T00:00:00Z",
		"content":             "content the caller was told was not written",
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 from the stale token, got %d: %s", rr.Code, rr.Body.String())
	}

	if got := countOpLog(t, srv, item.ID); got != 3 {
		t.Errorf("op-log has %d rows after a REFUSED write, want 3 — the prune must roll back with the write, "+
			"or a closed tab's unflushed edits are destroyed by a request that wrote nothing", got)
	}

	// The refusal must also be a refusal in the ordinary sense.
	after, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if after.Content == "content the caller was told was not written" {
		t.Error("items.content was written despite the 409")
	}
}

// TestCollabDirectWrite_SuccessfulWriteStillPrunes is the counterfactual, and
// it is what stops the test above from passing under a fix that simply deletes
// the prune. The prune exists so a future Join cannot replay superseded ops
// over the content just written; a refusal must roll it back, a success must
// keep it.
func TestCollabDirectWrite_SuccessfulWriteStillPrunes(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	seedOpLog(t, srv, item.ID, 3)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"content": "content the caller did write",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if got := countOpLog(t, srv, item.ID); got != 0 {
		t.Errorf("op-log has %d rows after a SUCCESSFUL write, want 0 — ops older than the content just written "+
			"would be replayed by the next Join and overwrite it", got)
	}

	after, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if after.Content != "content the caller did write" {
		t.Errorf("items.content = %q, want the written content", after.Content)
	}
}

// TestCollabDirectWrite_OpenChildrenRefusalAlsoLeavesTheOpLog covers a SECOND
// refusal from the same write, because the window is a property of the ordering
// rather than of one error (CONVE-18: the instance named is a sample). The
// optimistic-concurrency conflict above is refused before the guard runs; this
// one is refused by the guard itself, inside the transaction the prune now
// shares — a different arm reaching the same rollback.
func TestCollabDirectWrite_OpenChildrenRefusalAlsoLeavesTheOpLog(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)
	plan, _ := seedParentAndChildren(t, srv, slug, []string{"open"})

	seedOpLog(t, srv, plan.ID, 2)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
		"fields":  map[string]interface{}{"status": "completed"},
		"content": "content the caller was told was not written",
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 from the open-children guard, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := countOpLog(t, srv, plan.ID); got != 2 {
		t.Errorf("op-log has %d rows after a guard-refused write, want 2", got)
	}
}
