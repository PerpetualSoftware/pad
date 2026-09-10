package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
)

// BUG-2995: a SUCCESSFUL content PATCH through the designated applier answers 200
// carrying the item's PREVIOUS content.
//
// This is the control the lead's ruling requires: a test that drives the applier
// path and goes red on the previous content appearing in a 200. It is written to
// fail against the code as it stands, which is what makes its green mean anything.
//
// Why the previous content and not simply "the wrong content": on the applier path
// the row write runs with Content nil (applierFirstWrite), so `updated` is the row
// as it stood BEFORE the request. A caller that PATCHes content and reads `content`
// back — the obvious way to confirm a write — sees its own update missing and can
// reasonably conclude the write did not take.
func TestBUG2995_ApplierPatchDoesNotEchoPreviousContent(t *testing.T) {
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	// Establish a distinctive PREVIOUS row content BEFORE any tab is connected, so
	// this write takes the direct path and actually lands in items.content. Without
	// it the "previous" value is the empty string, which a passing assertion could
	// not distinguish from a response that simply omitted content.
	const previous = "PREVIOUS content, written before any tab connected"
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug,
		map[string]interface{}{"content": previous})
	if rr.Code != http.StatusOK {
		t.Fatalf("seed PATCH failed: %d %s", rr.Code, rr.Body.String())
	}
	if got, err := srv.store.GetItem(item.ID); err != nil {
		t.Fatalf("GetItem: %v", err)
	} else if got.Content != previous {
		t.Fatalf("seed PATCH did not land in items.content: %q", got.Content)
	}

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

	// waitForApplierPath's probes go through the applier, so items.content is still
	// `previous` here — re-read rather than assume it, since the probe would have
	// landed in the row on any pass that took the direct path.
	rowBefore, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}

	const sent = "NEW content, sent through the applier"
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug,
		map[string]interface{}{"content": sent})
	if rr.Code != http.StatusOK {
		t.Fatalf("applier-path PATCH: want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal 200 body: %v", err)
	}
	got, present := body["content"].(string)

	// THE PROPERTY. A 200 to a content PATCH never carries a `content` value the
	// caller can read as "my write was dropped".
	if present && got == rowBefore.Content && got != sent {
		t.Errorf("BUG-2995: the 200 carries the PREVIOUS content.\n  sent:     %q\n  answered: %q\n"+
			"A caller that PATCHes content and reads `content` back sees its own update missing.",
			sent, got)
	}

	// The positive half. The negative assertion above passes for a response that
	// dropped `content` altogether or answered something else entirely, so it does
	// not by itself pin the shape the contract promised.
	if !present || got != sent {
		t.Errorf("want the 200 to carry the content as SENT; present=%v got=%q want=%q", present, got, sent)
	}

	// The marker is what makes the echo honest — without it the echoed value is an
	// implicit claim about the row, and the row does not hold it yet (and will not
	// hold it byte-identically once it does).
	warnings, _ := body["warnings"].(map[string]any)
	if warnings == nil {
		t.Fatalf("want a warnings object naming where the content is; got body keys %v", keysOf(body))
	}
	if outcome, _ := warnings["content_outcome"].(string); outcome != contentOutcomeAppliedPendingFlush {
		t.Errorf("warnings.content_outcome = %q, want %q", outcome, contentOutcomeAppliedPendingFlush)
	}

	// The row is deliberately still behind: this change does not make the flush
	// happen sooner, it stops the response lying about it. Asserted so that a
	// future change which "fixes" this by writing the row directly on the applier
	// path — reintroducing the lost-write hazard PLAN-2975 closed — fails here.
	rowAfter, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if rowAfter.Content == sent {
		t.Errorf("items.content took the applier-path write directly (%q); the row is supposed to catch "+
			"up on the next collab-snapshot flush, and writing it here is the lost-write hazard "+
			"PLAN-2975 closed", sent)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestBUG2995_DirectPathCarriesNoPendingMarker is the control leg. The marker means
// something only if it is absent when the content DID land in the row, so a change
// that stamps it unconditionally has to fail somewhere.
func TestBUG2995_DirectPathCarriesNoPendingMarker(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	// No tab is connected, so this takes the direct-write path and lands in the row.
	const sent = "content written with nobody connected"
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug,
		map[string]interface{}{"content": sent})
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH: want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	stored, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if stored.Content != sent {
		t.Fatalf("precondition: this test needs the direct path, but items.content is %q", stored.Content)
	}

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal 200 body: %v", err)
	}
	// The echo is applier-path-only. A change that overwrites `content` on every
	// path would blank it here, since there is no applied markdown to echo.
	if got, _ := body["content"].(string); got != sent {
		t.Errorf("direct-path 200 content = %q, want the stored value %q", got, sent)
	}
	if warnings, ok := body["warnings"].(map[string]any); ok {
		if outcome, _ := warnings["content_outcome"].(string); outcome != "" {
			t.Errorf("direct-path 200 carries content_outcome=%q; the content IS in the row, so there is "+
				"nothing pending and the marker must be absent", outcome)
		}
	}
}
