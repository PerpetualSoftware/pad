package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3032: an imported bundle can carry item bodies that were already behind
// their source's live collaborative document when the bundle was written. The
// import ACCEPTS them — refusing would make a backup unrestorable for a reason
// the operator cannot fix from here — and reports the count.
//
// The count comes from the bundle's OWN marker. This side has no op-log to
// evaluate the predicate against, which is also why the destination rows are not
// stamped: nothing in this workspace is ahead of them.
func staleBundle(t *testing.T, states ...string) []byte {
	t.Helper()
	exp := models.WorkspaceExport{
		Version:   1,
		Workspace: models.WorkspaceExportMeta{Name: "Restored", Slug: "restored"},
		Collections: []models.CollectionExport{{
			ID: "col-1", Name: "Tasks", Slug: "tasks", Schema: "{}", Settings: "{}",
		}},
	}
	for i, st := range states {
		exp.Items = append(exp.Items, models.ItemExport{
			ID:           "item-" + string(rune('a'+i)),
			CollectionID: "col-1",
			Title:        "Subject " + string(rune('a'+i)),
			Slug:         "subject-" + string(rune('a'+i)),
			Content:      "body", Fields: "{}", Tags: "[]", ItemNumber: i + 1,
			ContentState: st,
		})
	}
	b, err := json.Marshal(exp)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	return b
}

func TestImportReportsHowManyBodiesWereStaleWhenTheBundleWasWritten(t *testing.T) {
	t.Run("a clean bundle sets no header", func(t *testing.T) {
		// THE CONTROL, first. A tally that counted every item, or that set the
		// header unconditionally, would satisfy the assertion below.
		srv := testServer(t)
		rr := importWithQuery(t, srv, "", staleBundle(t, "", ""))
		if rr.Code != http.StatusCreated {
			t.Fatalf("import returned %d, want 201: %s", rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get(StaleBodyImportHeader); got != "" {
			t.Errorf("%s = %q on a bundle with nothing stale; a clean import's response must be "+
				"unchanged", StaleBodyImportHeader, got)
		}
	})

	t.Run("a bundle with stale bodies reports the count", func(t *testing.T) {
		body := staleBundle(t, models.ContentOutcomeAppliedPendingFlush, "",
			models.ContentOutcomeAppliedPendingFlush)
		// The fixture must actually carry the marker, or everything below passes
		// while measuring nothing.
		if !bytes.Contains(body, []byte(`"content_state":"`+models.ContentOutcomeAppliedPendingFlush+`"`)) {
			t.Fatalf("fixture does not carry the marker: %s", body)
		}

		srv := testServer(t)
		rr := importWithQuery(t, srv, "", body)
		if rr.Code != http.StatusCreated {
			t.Fatalf("import returned %d, want 201: %s", rr.Code, rr.Body.String())
		}
		// Two of three, not three: the count is of MARKED items, so a tally that
		// reported len(Items) would read as working on a fully-stale bundle.
		if got := rr.Header().Get(StaleBodyImportHeader); got != "2" {
			t.Errorf("%s = %q, want \"2\"", StaleBodyImportHeader, got)
		}

		// The import SUCCEEDED and the bodies landed. The count is an advisory
		// about provenance, not a refusal — a restore that refused here would be
		// unfixable from the destination.
		var ws models.Workspace
		if err := json.Unmarshal(rr.Body.Bytes(), &ws); err != nil {
			t.Fatalf("parse import response: %v", err)
		}
		items, err := srv.store.ListItems(ws.ID, models.ItemListParams{})
		if err != nil {
			t.Fatalf("ListItems: %v", err)
		}
		if len(items) != 3 {
			t.Fatalf("imported %d items, want 3", len(items))
		}
		// And no destination row is marked: this workspace's op-log is empty, so
		// a mark here would assert a pending flush that can never clear.
		for _, it := range items {
			if it.ContentState != "" {
				t.Errorf("imported item %s is marked %q; nothing in this workspace is ahead of it",
					it.Ref, it.ContentState)
			}
		}
	})
}
