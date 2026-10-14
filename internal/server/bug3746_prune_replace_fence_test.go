package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3746: a REST content replacement prunes the op-log and replaces
// items.content, but did not stamp the seed boundary a restore stamps. A tab
// that fetched the old body and joined afterwards with since=0 and its old
// content_seq was admitted, seeded the old body into the empty document, and
// flushed it over the new one. Join's fence (contentSeq < last restore seq →
// force_refresh) is unchanged and already tested for restores; this pins that
// a replacement now moves the boundary, durably and in memory.
func TestBUG3746_ContentReplacementMovesTheSeedBoundary(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":   "BUG-3746",
		"content": "A",
		"fields":  `{"status":"open"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)
	seqA := created.Seq

	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+created.Slug,
		map[string]interface{}{"content": "B"})
	if rr.Code != http.StatusOK {
		t.Fatalf("replace: %d %s", rr.Code, rr.Body.String())
	}
	var updated models.Item
	parseJSON(t, rr, &updated)
	if updated.Content != "B" || updated.Seq <= seqA {
		t.Fatalf("precondition: replacement did not land (content %q, seq %d after %d)", updated.Content, updated.Seq, seqA)
	}

	durable, ok, err := srv.store.ItemLastRestoreSeq(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || durable != updated.Seq {
		t.Fatalf("durable seed boundary = (%d, %v), want (%d, true): a tab seeded at seq %d would be admitted", durable, ok, updated.Seq, seqA)
	}
	if mem, ok := srv.collab.LastRestoreSeq(created.ID); !ok || mem != updated.Seq {
		t.Fatalf("in-memory seed boundary = (%d, %v), want (%d, true): Join reads it before the column", mem, ok, updated.Seq)
	}
}

// The patch route's twin. It prunes and replaces through its own
// PruneAndApply call (handlers_items_patch.go), so it is wired separately
// and needs its own leg (CONVE-19). The fixture seeds the body through the
// content route, which already stamps a boundary, so this asserts the
// boundary moves past the seed's seq to the patch's.
func TestBUG3746_PatchReplacementMovesTheSeedBoundary(t *testing.T) {
	srv := testServerWithCollab(t)
	f := newPatchFixture(t, srv, "alpha\nbeta\ngamma\n")
	seeded := f.current(t).Seq

	code, out := f.patch(t, map[string]any{"edits": edits("beta", "BETA")})
	if code != http.StatusOK {
		t.Fatalf("patch: %d %v", code, out)
	}
	after := f.current(t)
	if after.Seq <= seeded {
		t.Fatalf("precondition: the patch did not land (seq %d after %d)", after.Seq, seeded)
	}

	durable, ok, err := srv.store.ItemLastRestoreSeq(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || durable != after.Seq {
		t.Fatalf("durable seed boundary = (%d, %v), want (%d, true): a tab seeded before the patch (seq %d) would be admitted", durable, ok, after.Seq, seeded)
	}
	if mem, ok := srv.collab.LastRestoreSeq(f.item.ID); !ok || mem != after.Seq {
		t.Fatalf("in-memory seed boundary = (%d, %v), want (%d, true)", mem, ok, after.Seq)
	}
}

// joinSeededAt joins the item's room as a tab whose document was seeded from
// items.content at contentSeq (since=0), and returns the first control frame:
// op_log_cursor for an admitted tab, force_refresh for a refused seed.
func joinSeededAt(t *testing.T, srv *Server, itemID string, contentSeq int64) string {
	t.Helper()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	q := url.Values{}
	q.Set("schema_version", collab.DefaultSchemaVersion)
	q.Set("content_seq", fmt.Sprint(contentSeq))
	c, _, err := (&websocket.Dialer{HandshakeTimeout: 3 * time.Second}).Dial("ws://"+u.Host+"/api/v1/collab/"+itemID+"?"+q.Encode(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	return firstControl(t, c).Type
}

// The fence must not OVER-fence (Heron, GO-blocking on #2134): a tab seeded
// from the replacement itself, at the new seq, is admitted normally, or every
// fresh tab after a REST replace would be force-refreshed, possibly in a loop.
// A tab one generation older is refused, through an actual join. One leg per
// route (CONVE-19).
func TestBUG3746_ContentReplacementAdmitsTheNewSeedAndRefusesTheOld(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":   "BUG-3746 admit",
		"content": "A",
		"fields":  `{"status":"open"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+created.Slug,
		map[string]interface{}{"content": "B"})
	if rr.Code != http.StatusOK {
		t.Fatalf("replace: %d %s", rr.Code, rr.Body.String())
	}
	var updated models.Item
	parseJSON(t, rr, &updated)

	if got := joinSeededAt(t, srv, created.ID, updated.Seq); got != collab.ControlMessageOpLogCursor {
		t.Fatalf("a tab seeded from the replacement (seq %d) got %q, want %q: the fence over-fences", updated.Seq, got, collab.ControlMessageOpLogCursor)
	}
	if got := joinSeededAt(t, srv, created.ID, updated.Seq-1); got != collab.ControlMessageForceRefresh {
		t.Fatalf("a tab seeded before the replacement (seq %d) got %q, want %q", updated.Seq-1, got, collab.ControlMessageForceRefresh)
	}
}

func TestBUG3746_PatchReplacementAdmitsTheNewSeedAndRefusesTheOld(t *testing.T) {
	srv := testServerWithCollab(t)
	f := newPatchFixture(t, srv, "alpha\nbeta\ngamma\n")
	code, out := f.patch(t, map[string]any{"edits": edits("beta", "BETA")})
	if code != http.StatusOK {
		t.Fatalf("patch: %d %v", code, out)
	}
	after := f.current(t)

	if got := joinSeededAt(t, srv, f.item.ID, after.Seq); got != collab.ControlMessageOpLogCursor {
		t.Fatalf("a tab seeded from the patch (seq %d) got %q, want %q: the fence over-fences", after.Seq, got, collab.ControlMessageOpLogCursor)
	}
	if got := joinSeededAt(t, srv, f.item.ID, after.Seq-1); got != collab.ControlMessageForceRefresh {
		t.Fatalf("a tab seeded before the patch (seq %d) got %q, want %q", after.Seq-1, got, collab.ControlMessageForceRefresh)
	}
}
