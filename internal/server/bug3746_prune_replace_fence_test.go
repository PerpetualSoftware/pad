package server

import (
	"net/http"
	"testing"

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
	srv := testServer(t)
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
	if _, err := srv.store.AppendYjsUpdate(created.ID, []byte{0x00, 0x01}, "1"); err != nil {
		t.Fatal(err)
	}

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
