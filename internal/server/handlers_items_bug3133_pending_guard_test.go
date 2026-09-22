package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3133: expected_seq guards the ROW, and a tab's unflushed typing is not in
// the row. A content write that carries a token is refused while content-bearing
// op-log rows sit above the flush watermark; overwrite_pending_edits lifts it; a
// tokenless write proceeds, and on the direct path it reports how many pending
// rows its prune destroyed. These are the contract legs the lead's ruling names.

// pendingEditFrame is a well-formed sync update with a non-empty payload that no
// other row repeats, so the classifier marks it content-bearing — what a tab's
// keystroke looks like to the relay.
var pendingEditFrame = []byte{0x00, 0x02, 0x04, 0x01, 0x9A, 0x7C, 0x00}

type bug3133Fixture struct {
	srv  *Server
	slug string
	item models.Item
	path string
}

func newBug3133Fixture(t *testing.T) bug3133Fixture {
	t.Helper()
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Pending guard", `{"status":"open"}`)
	return bug3133Fixture{srv: srv, slug: slug, item: item,
		path: "/api/v1/workspaces/" + slug + "/items/" + item.Slug}
}

// seedPending leaves the item in the BUG-3000 state: an unflushed edit from a tab
// that has since closed.
func (f bug3133Fixture) seedPending(t *testing.T) {
	t.Helper()
	if _, err := f.srv.store.AppendYjsUpdate(f.item.ID, pendingEditFrame, "1"); err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}
	got, err := f.srv.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	// PREMISE: the seeded row is content-bearing, or every leg below measures
	// a clean item.
	if got.ContentState != models.ContentOutcomeAppliedPendingFlush {
		t.Fatalf("premise: seeded item must read pending; got %q", got.ContentState)
	}
}

func (f bug3133Fixture) seq(t *testing.T) int64 {
	t.Helper()
	got, err := f.srv.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got.Seq
}

func decodeErrorCode(t *testing.T, rr *httptest.ResponseRecorder) (string, map[string]any) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, rr.Body.String())
	}
	return body.Error.Code, body.Error.Details
}

func decodeWrite(t *testing.T, rr *httptest.ResponseRecorder) models.Item {
	t.Helper()
	var got models.Item
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode item: %v (%s)", err, rr.Body.String())
	}
	return got
}

// Leg 1 (direct path): token + pending → 409 content_pending_flush, and nothing
// moves: not the body, not the seq, not the op-log.
func TestPendingGuardRefusesTokenedContentWrite_Direct(t *testing.T) {
	for _, tok := range []string{"expected_seq", "expected_updated_at"} {
		t.Run(tok, func(t *testing.T) {
			f := newBug3133Fixture(t)
			f.seedPending(t)
			before, _ := f.srv.store.GetItem(f.item.ID)
			beforeOps := countOpLog(t, f.srv, f.item.ID)

			body := map[string]any{"content": "replacement body"}
			if tok == "expected_seq" {
				body["expected_seq"] = before.Seq
			} else {
				body["expected_updated_at"] = before.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
			}
			rr := doRequest(f.srv, "PATCH", f.path, body)
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
				t.Errorf("a refused write touched the op-log: %d rows → %d", beforeOps, got)
			}
		})
	}
}

// Leg 2: the same write with overwrite_pending_edits proceeds, the prune runs,
// and the warning names what it destroyed.
func TestPendingGuardOverrideProceedsAndReportsThePrune(t *testing.T) {
	f := newBug3133Fixture(t)
	f.seedPending(t)
	rr := doRequest(f.srv, "PATCH", f.path, map[string]any{
		"content":                 "replacement body",
		"expected_seq":            f.seq(t),
		"overwrite_pending_edits": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := decodeWrite(t, rr)
	if got.Warnings == nil || got.Warnings.PrunedPendingEdits != 1 {
		t.Fatalf("warnings = %+v, want pruned_pending_edits = 1", got.Warnings)
	}
	if n := countOpLog(t, f.srv, f.item.ID); n != 0 {
		t.Errorf("the direct write must prune the op-log; %d rows remain", n)
	}
	if stored, _ := f.srv.store.GetItem(f.item.ID); stored.Content != "replacement body" {
		t.Errorf("content = %q, want the replacement", stored.Content)
	}
}

// Leg 3: a tokenless direct write with pending rows proceeds (tokenless means
// replace) and carries the warning.
func TestPendingGuardTokenlessWriteProceedsWithWarning(t *testing.T) {
	f := newBug3133Fixture(t)
	f.seedPending(t)
	rr := doRequest(f.srv, "PATCH", f.path, map[string]any{"content": "replacement body"})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := decodeWrite(t, rr)
	if got.Warnings == nil || got.Warnings.PrunedPendingEdits != 1 {
		t.Fatalf("warnings = %+v, want pruned_pending_edits = 1", got.Warnings)
	}
}

// Leg 4: no pending rows → the write is unchanged — 200, and no warning key on
// the wire at all, so a clean write stays byte-identical to before.
func TestPendingGuardCleanItemIsUnchanged(t *testing.T) {
	for _, withToken := range []bool{true, false} {
		name := "tokenless"
		if withToken {
			name = "expected_seq"
		}
		t.Run(name, func(t *testing.T) {
			f := newBug3133Fixture(t)
			body := map[string]any{"content": "a new body"}
			if withToken {
				body["expected_seq"] = f.seq(t)
			}
			rr := doRequest(f.srv, "PATCH", f.path, body)
			if rr.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
			}
			var raw map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
				t.Fatal(err)
			}
			if _, present := raw["warnings"]; present {
				t.Errorf("a clean write must carry no warnings; got %v", raw["warnings"])
			}
		})
	}
}

// A stale token with pending rows answers update_conflict, not the new code: the
// token is the more basic failure, and a caller must re-read either way.
func TestPendingGuardStaleTokenStillAnswersUpdateConflict(t *testing.T) {
	f := newBug3133Fixture(t)
	f.seedPending(t)
	stale := f.seq(t)
	if rr := doRequest(f.srv, "PATCH", f.path, map[string]any{"fields_patch": map[string]any{"status": "in-progress"}}); rr.Code != http.StatusOK {
		t.Fatalf("bump seq: %d: %s", rr.Code, rr.Body.String())
	}
	rr := doRequest(f.srv, "PATCH", f.path, map[string]any{"content": "x", "expected_seq": stale})
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", rr.Code, rr.Body.String())
	}
	if code, _ := decodeErrorCode(t, rr); code != "update_conflict" {
		t.Fatalf("code = %q, want update_conflict", code)
	}
}

// A token-carrying write WITHOUT content is not guarded: the unflushed edits are
// body edits, and a fields write cannot replace them.
func TestPendingGuardIgnoresFieldOnlyWrites(t *testing.T) {
	f := newBug3133Fixture(t)
	f.seedPending(t)
	rr := doRequest(f.srv, "PATCH", f.path, map[string]any{
		"fields_patch": map[string]any{"status": "in-progress"},
		"expected_seq": f.seq(t),
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("a field-only write must not be refused; got %d: %s", rr.Code, rr.Body.String())
	}
}

// Applier path: with a writer tab connected, the refusal lands BEFORE the row
// write, so no applier_request goes out and the tab's document is untouched.
func TestPendingGuardRefusesOnTheApplierPath(t *testing.T) {
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
		"content":      "replacement body",
		"expected_seq": f.seq(t),
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", rr.Code, rr.Body.String())
	}
	if code, _ := decodeErrorCode(t, rr); code != "content_pending_flush" {
		t.Fatalf("code = %q, want content_pending_flush", code)
	}
	// applierEcho writes an op-log row for every applier_request it answers, so
	// an unchanged count is what proves no request went out.
	if got := countOpLog(t, f.srv, f.item.ID); got != beforeOps {
		t.Errorf("the refusal must land before the applier is asked; op-log %d → %d", beforeOps, got)
	}

	// Control: the override takes the applier path and succeeds.
	rr = doRequest(f.srv, "PATCH", f.path, map[string]any{
		"content":                 "replacement body",
		"expected_seq":            f.seq(t),
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
