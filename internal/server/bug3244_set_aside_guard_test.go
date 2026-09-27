package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3244 ruling 2: rows a schema rebuild set aside refuse EVERY content
// write, token or not, unless it sends overwrite_pending_edits, which discards
// them and counts them in warnings.pruned_pending_edits. The refusal must not
// offer "open the item" as a remedy, because no tab can restore them.
//
// The set-aside state is made with the rebuild's own store call; the rebuild
// reaching it from a bumped join is TestBUG3244SchemaRebuildKeepsUnflushedEditsMarked.

func (f bug3133Fixture) seedSetAside(t *testing.T) {
	t.Helper()
	f.seedPending(t)
	moved, _, err := f.srv.store.SetAsideAndClearOpLog(f.item.ID)
	if err != nil {
		t.Fatalf("SetAsideAndClearOpLog: %v", err)
	}
	if moved != 1 {
		t.Fatalf("premise: want 1 row set aside, got %d", moved)
	}
	if got, _ := f.srv.store.GetItem(f.item.ID); got.ContentState != models.ContentStateSetAside {
		t.Fatalf("premise: content_state = %q, want %q", got.ContentState, models.ContentStateSetAside)
	}
}

func (f bug3133Fixture) setAsideRows(t *testing.T) int {
	t.Helper()
	rows, err := f.srv.store.ListYjsSetAside(f.item.ID)
	if err != nil {
		t.Fatalf("ListYjsSetAside: %v", err)
	}
	return len(rows)
}

func assertSetAsideRefusal(t *testing.T, f bug3133Fixture, before *models.Item, rrCode int, body string, code string, details map[string]any) {
	t.Helper()
	if rrCode != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", rrCode, body)
	}
	if code != "content_pending_flush" {
		t.Fatalf("code = %q, want content_pending_flush", code)
	}
	if details["set_aside_rows"] != float64(1) {
		t.Errorf("set_aside_rows = %v, want 1", details["set_aside_rows"])
	}
	if strings.Contains(body, "browser") || strings.Contains(body, "Wait for the open editor") {
		t.Errorf("the refusal offers opening the item, which cannot restore set-aside edits: %s", body)
	}
	after, _ := f.srv.store.GetItem(f.item.ID)
	if after.Content != before.Content || after.Seq != before.Seq {
		t.Errorf("a refused write moved the row: content %q→%q seq %d→%d", before.Content, after.Content, before.Seq, after.Seq)
	}
	if n := f.setAsideRows(t); n != 1 {
		t.Errorf("a refused write changed the set-aside rows: %d, want 1", n)
	}
}

func TestSetAsideRefusesTokenlessContentWrite(t *testing.T) {
	// Control: the same write to an item with nothing set aside lands, as it
	// did before BUG-3244. Without it the refusal below could be anything.
	t.Run("control: nothing set aside", func(t *testing.T) {
		f := newBug3133Fixture(t)
		rr := doRequest(f.srv, "PATCH", f.path, map[string]any{"content": "replacement body"})
		if rr.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("set aside: refused", func(t *testing.T) {
		f := newBug3133Fixture(t)
		f.seedSetAside(t)
		before, _ := f.srv.store.GetItem(f.item.ID)
		rr := doRequest(f.srv, "PATCH", f.path, map[string]any{"content": "replacement body"})
		code, details := "", map[string]any(nil)
		if rr.Code == http.StatusConflict {
			code, details = decodeErrorCode(t, rr)
		}
		assertSetAsideRefusal(t, f, before, rr.Code, rr.Body.String(), code, details)
	})
}

func TestSetAsideRefusesTokenedContentWrite(t *testing.T) {
	f := newBug3133Fixture(t)
	f.seedSetAside(t)
	before, _ := f.srv.store.GetItem(f.item.ID)
	rr := doRequest(f.srv, "PATCH", f.path, map[string]any{"content": "replacement body", "expected_seq": before.Seq})
	code, details := "", map[string]any(nil)
	if rr.Code == http.StatusConflict {
		code, details = decodeErrorCode(t, rr)
	}
	assertSetAsideRefusal(t, f, before, rr.Code, rr.Body.String(), code, details)
}

func TestSetAsideOverwriteDiscardsAndReports(t *testing.T) {
	f := newBug3133Fixture(t)
	f.seedSetAside(t)
	rr := doRequest(f.srv, "PATCH", f.path, map[string]any{"content": "replacement body", "overwrite_pending_edits": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := decodeWrite(t, rr)
	if got.Warnings == nil || got.Warnings.PrunedPendingEdits != 1 {
		t.Fatalf("warnings.pruned_pending_edits = %+v, want 1 (the discarded set-aside row)", got.Warnings)
	}
	if n := f.setAsideRows(t); n != 0 {
		t.Fatalf("set-aside rows after the override = %d, want 0", n)
	}
	after, _ := f.srv.store.GetItem(f.item.ID)
	if after.Content != "replacement body" || after.ContentState != "" {
		t.Fatalf("after the override: content %q, content_state %q", after.Content, after.ContentState)
	}
}

// The tab's own flush writes the new-era document; it is not replacing the
// set-aside edits (the body never held them), so it is not refused, and it
// must not clear them either.
func TestSetAsideSurvivesCollabSnapshotFlush(t *testing.T) {
	f := newBug3133Fixture(t)
	f.seedSetAside(t)
	rr := doRequest(f.srv, "PATCH", f.path+"?source=collab-snapshot", map[string]any{"content": "flushed body"})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if n := f.setAsideRows(t); n != 1 {
		t.Fatalf("set-aside rows after a flush = %d, want 1", n)
	}
	after, _ := f.srv.store.GetItem(f.item.ID)
	if after.ContentState != models.ContentStateSetAside {
		t.Fatalf("content_state after a flush = %q, want %q", after.ContentState, models.ContentStateSetAside)
	}
}

// A caught-up tab stamps the flush watermark (BUG-3124 unit B). That clears
// the unflushed-rows state by design; it must not clear the set-aside state,
// which no tab can have caught up with.
func TestSetAsideSurvivesWatermarkStamp(t *testing.T) {
	f := newBug3133Fixture(t)
	f.seedSetAside(t)
	if _, err := f.srv.store.AppendYjsUpdate(f.item.ID, bug3244Frame, "1"); err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}
	max, _, err := f.srv.store.MaxOpLogID(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.SetItemContentFlushedOpLogIDForTesting(f.item.ID, max); err != nil {
		t.Fatal(err)
	}
	after, _ := f.srv.store.GetItem(f.item.ID)
	if after.ContentState != models.ContentStateSetAside {
		t.Fatalf("content_state after the watermark caught up = %q, want %q", after.ContentState, models.ContentStateSetAside)
	}
}

func TestSetAsideRefusesRestoreUntilOverwrite(t *testing.T) {
	f := newBug3133Fixture(t)
	// Each write under a different source, so the per-(actor, source) version
	// throttle cannot swallow one (the BUG-3031 test's recipe).
	for _, step := range []struct{ content, source string }{{"first body", "web"}, {"second body", "skill"}, {"third body", "cli"}} {
		body := step.content
		if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{Content: &body, LastModifiedBy: "user", Source: step.source}); err != nil {
			t.Fatal(err)
		}
	}
	versions, err := f.srv.store.ListItemVersionsResolved(f.item.ID, "third body")
	if err != nil {
		t.Fatal(err)
	}
	var firstID string
	for _, v := range versions {
		if v.Content == "first body" {
			firstID = v.ID
		}
	}
	if firstID == "" {
		t.Fatalf("premise: no version resolving to the first body among %d", len(versions))
	}
	f.seedSetAside(t)
	restore := f.path + "/versions/" + firstID + "/restore"

	rr := doRequest(f.srv, "POST", restore, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", rr.Code, rr.Body.String())
	}
	code, details := decodeErrorCode(t, rr)
	if code != "content_pending_flush" || details["set_aside_rows"] != float64(1) {
		t.Fatalf("refusal = %q %v, want content_pending_flush with set_aside_rows 1", code, details)
	}
	if strings.Contains(rr.Body.String(), "browser") {
		t.Errorf("the restore refusal offers opening the item: %s", rr.Body.String())
	}
	if n := f.setAsideRows(t); n != 1 {
		t.Fatalf("a refused restore changed the set-aside rows: %d", n)
	}

	rr = doRequest(f.srv, "POST", restore, map[string]any{"overwrite_pending_edits": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("override: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if n := f.setAsideRows(t); n != 0 {
		t.Fatalf("set-aside rows after the override restore = %d, want 0", n)
	}
	// BUG-3230 U2 names how many edits a restore deleted; the set-aside rows
	// it discarded are counted there, as a content write counts them.
	var restored models.Item
	if err := json.Unmarshal(rr.Body.Bytes(), &restored); err != nil {
		t.Fatalf("decode restore response: %v", err)
	}
	if restored.Warnings == nil || restored.Warnings.PrunedPendingEdits != 1 {
		t.Fatalf("restore warnings.pruned_pending_edits = %+v, want 1 (the discarded set-aside row)", restored.Warnings)
	}
}

// Ruling 3: the rows are readable as raw updates and an explicit discard
// clears the state without writing the body.
func TestSetAsideReadAndDiscardEndpoints(t *testing.T) {
	f := newBug3133Fixture(t)
	f.seedSetAside(t)
	before, _ := f.srv.store.GetItem(f.item.ID)

	rr := doRequest(f.srv, "GET", f.path+"/collab-set-aside", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Ref      string               `json:"ref"`
		SetAside []models.YjsSetAside `json:"set_aside"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.SetAside) != 1 || string(got.SetAside[0].UpdateData) != string(pendingEditFrame) {
		t.Fatalf("GET set_aside = %+v, want the one seeded frame verbatim", got.SetAside)
	}

	rr = doRequest(f.srv, "DELETE", f.path+"/collab-set-aside", nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"discarded":1`) {
		t.Fatalf("DELETE: want 200 discarded 1, got %d: %s", rr.Code, rr.Body.String())
	}
	after, _ := f.srv.store.GetItem(f.item.ID)
	if after.ContentState != "" || after.Content != before.Content || after.Seq != before.Seq {
		t.Fatalf("after discard: content_state %q, content %q→%q, seq %d→%d",
			after.ContentState, before.Content, after.Content, before.Seq, after.Seq)
	}
	rr = doRequest(f.srv, "GET", f.path+"/collab-set-aside", nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"set_aside":[]`) {
		t.Fatalf("GET after discard: %d %s", rr.Code, rr.Body.String())
	}
}
