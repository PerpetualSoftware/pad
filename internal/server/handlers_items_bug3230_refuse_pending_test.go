package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3230 U0: refuse_pending_edits asks for the BUG-3133 refusal without a
// version token. The item pane's raw-markdown saves send it: they cannot carry a
// token (overlapping debounced saves would refuse each other, and BUG-3080 keeps
// tokens off teardown writes), and without it a raw save pruned another tab's
// unflushed edits from the op-log silently.

// Direct path: the flag alone refuses, and nothing moves.
func TestRefusePendingEditsRefusesWithoutAToken_Direct(t *testing.T) {
	f := newBug3133Fixture(t)
	f.seedPending(t)
	before, _ := f.srv.store.GetItem(f.item.ID)
	beforeOps := countOpLog(t, f.srv, f.item.ID)

	rr := doRequest(f.srv, "PATCH", f.path, map[string]any{
		"content":              "raw save",
		"refuse_pending_edits": true,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", rr.Code, rr.Body.String())
	}
	code, details := decodeErrorCode(t, rr)
	if code != "content_pending_flush" {
		t.Fatalf("code = %q, want content_pending_flush", code)
	}
	if details["pending_rows"] != float64(1) {
		t.Errorf("pending_rows = %v, want 1", details["pending_rows"])
	}
	after, _ := f.srv.store.GetItem(f.item.ID)
	if after.Content != before.Content || after.Seq != before.Seq {
		t.Errorf("a refused write moved the row: content %q→%q seq %d→%d",
			before.Content, after.Content, before.Seq, after.Seq)
	}
	if got := countOpLog(t, f.srv, f.item.ID); got != beforeOps {
		t.Errorf("a refused write pruned the op-log: %d rows → %d", beforeOps, got)
	}
}

// The control that makes the leg above mean something: the SAME write without
// the flag is the pre-U0 raw save, and it deletes the other tab's edit.
func TestRefusePendingEditsControl_WithoutTheFlagTheEditIsPruned(t *testing.T) {
	f := newBug3133Fixture(t)
	f.seedPending(t)
	rr := doRequest(f.srv, "PATCH", f.path, map[string]any{"content": "raw save"})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if n := countOpLog(t, f.srv, f.item.ID); n != 0 {
		t.Fatalf("premise: a tokenless direct write prunes; %d rows remain", n)
	}
}

// overwrite_pending_edits lifts it: the user chose to replace the edits.
func TestRefusePendingEditsOverrideProceeds(t *testing.T) {
	f := newBug3133Fixture(t)
	f.seedPending(t)
	rr := doRequest(f.srv, "PATCH", f.path, map[string]any{
		"content":                 "raw save",
		"refuse_pending_edits":    true,
		"overwrite_pending_edits": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := decodeWrite(t, rr)
	if got.Warnings == nil || got.Warnings.PrunedPendingEdits != 1 {
		t.Fatalf("warnings = %+v, want pruned_pending_edits = 1", got.Warnings)
	}
	if stored, _ := f.srv.store.GetItem(f.item.ID); stored.Content != "raw save" {
		t.Errorf("content = %q, want the raw save", stored.Content)
	}
}

// Nothing pending: the flag changes nothing, and no warning key reaches the wire.
func TestRefusePendingEditsCleanItemIsUnchanged(t *testing.T) {
	f := newBug3133Fixture(t)
	rr := doRequest(f.srv, "PATCH", f.path, map[string]any{
		"content":              "raw save",
		"refuse_pending_edits": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := decodeWrite(t, rr); got.Warnings != nil {
		t.Errorf("a clean write must carry no warnings; got %+v", got.Warnings)
	}
}

// No row version is compared: two raw saves sent from one stale read both land.
// This is why the pane can use the flag where it cannot use expected_seq.
func TestRefusePendingEditsComparesNoRowVersion(t *testing.T) {
	f := newBug3133Fixture(t)
	for _, body := range []string{"first raw save", "second raw save"} {
		rr := doRequest(f.srv, "PATCH", f.path, map[string]any{
			"content":              body,
			"refuse_pending_edits": true,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("%q: want 200, got %d: %s", body, rr.Code, rr.Body.String())
		}
	}
	if stored, _ := f.srv.store.GetItem(f.item.ID); stored.Content != "second raw save" {
		t.Errorf("content = %q, want the second save", stored.Content)
	}
}

// A field-only write is not guarded by the flag either: the edits are body edits.
func TestRefusePendingEditsIgnoresFieldOnlyWrites(t *testing.T) {
	f := newBug3133Fixture(t)
	f.seedPending(t)
	rr := doRequest(f.srv, "PATCH", f.path, map[string]any{
		"fields_patch":         map[string]any{"status": "in-progress"},
		"refuse_pending_edits": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("a field-only write must not be refused; got %d: %s", rr.Code, rr.Body.String())
	}
}

// Applier path: a writer tab is connected. The refusal lands before the applier
// is asked, so the tab's document is untouched.
func TestRefusePendingEditsRefusesOnTheApplierPath(t *testing.T) {
	f := newBug3133Fixture(t)
	ts := httptest.NewServer(f.srv)
	t.Cleanup(ts.Close)
	conn, resp, err := dialCollab(t, ts.URL, f.item.ID, nil, "")
	if err != nil {
		t.Fatalf("dialCollab: %v (%v)", err, resp)
	}
	stop := applierEcho(t, conn)
	t.Cleanup(stop)
	waitForApplierPath(t, f.srv, f.slug, f.item.Slug, f.item.ID)

	f.seedPending(t)
	beforeOps := countOpLog(t, f.srv, f.item.ID)
	rr := doRequest(f.srv, "PATCH", f.path, map[string]any{
		"content":              "raw save",
		"refuse_pending_edits": true,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", rr.Code, rr.Body.String())
	}
	if code, _ := decodeErrorCode(t, rr); code != "content_pending_flush" {
		t.Fatalf("code = %q, want content_pending_flush", code)
	}
	// applierEcho writes an op-log row for every applier_request it answers.
	if got := countOpLog(t, f.srv, f.item.ID); got != beforeOps {
		t.Errorf("the refusal must land before the applier is asked; op-log %d → %d", beforeOps, got)
	}

	rr = doRequest(f.srv, "PATCH", f.path, map[string]any{
		"content":                 "raw save",
		"refuse_pending_edits":    true,
		"overwrite_pending_edits": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("override: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := decodeWrite(t, rr)
	if got.Warnings == nil || got.Warnings.ContentOutcome != models.ContentOutcomeAppliedPendingFlush {
		t.Fatalf("override must go through the applier; warnings = %+v", got.Warnings)
	}
}
