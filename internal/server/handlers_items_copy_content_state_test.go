package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3032: a cross-workspace copy carries items.content, which can be BEHIND
// the source item's live collaborative document. The copy WARNS rather than
// refuses — the source survives, so the real text is still in workspace A's
// op-log and the remedy is to copy again once a tab has flushed — whereas
// `pad db migrate-to-pg`, which abandons its source database, refuses.
//
// NOTE ON BACKEND: testServer builds on storetest.NewSQLite unconditionally, so
// this test is SQLite-only whatever gate it runs under. The predicate's Postgres
// leg lives in internal/store's TestExportWorkspaceMarksOnlyItemsWhoseDocumentIsAhead
// and TestListItemsPendingContentFlushNamesOnlyStaleItems, which parameterise
// over both backends.
func TestCopyEndpoint_WarnsWhenTheCopiedBodyIsBehindTheLiveDocument(t *testing.T) {
	t.Run("a current source is not marked", func(t *testing.T) {
		// THE CONTROL, first: the field is populated from the store result
		// unconditionally, so a source whose row is current must produce no key
		// at all. Without this leg a handler that hardcoded the marker would
		// satisfy the assertion below.
		f := newCopyPreflightFixture(t)
		rr := f.callCopy(f.owner, reqOpts{}, f.resolvableBody())
		if rr.Code != http.StatusCreated {
			t.Fatalf("copy: expected 201, got %d: %s", rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "source_content_state") {
			t.Errorf("a copy of a CURRENT item emitted a source_content_state key:\n%s", rr.Body.String())
		}
	})

	t.Run("a source whose document is ahead is marked", func(t *testing.T) {
		f := newCopyPreflightFixture(t)

		// An op-log row above the (NULL) flush watermark: the source item's
		// document is ahead of the row the copy is about to read.
		if _, err := f.srv.store.AppendYjsUpdate(f.source.ID, []byte{1, 2, 3}, "1"); err != nil {
			t.Fatalf("AppendYjsUpdate: %v", err)
		}

		res := f.copyOK(f.resolvableBody())
		if res.Warnings.SourceContentState != models.ContentOutcomeAppliedPendingFlush {
			t.Fatalf("warnings.source_content_state = %q, want %q: this copy carried a body that "+
				"was behind the source's live document, and no other field in the response says so",
				res.Warnings.SourceContentState, models.ContentOutcomeAppliedPendingFlush)
		}

		// The copy still SUCCEEDED and still landed the body. The warning
		// describes the copy; it does not withhold it — the asymmetry with the
		// migration gate is the whole point of this item.
		if res.Item == nil {
			t.Fatal("the warning suppressed the copy; a copy must warn, not refuse")
		}
		dst, err := f.srv.store.GetItem(res.Item.ID)
		if err != nil || dst == nil {
			t.Fatalf("GetItem(destination): %v (nil=%v)", err, dst == nil)
		}
		if dst.Content != "body" {
			t.Errorf("destination content = %q, want the source row's body %q", dst.Content, "body")
		}

		// And the DESTINATION is not itself marked: its op-log is empty, so
		// nothing there is ahead of it. Stamping the state onto the copy would
		// be a claim about a document the destination workspace does not have.
		if dst.ContentState != "" {
			t.Errorf("the destination item is marked %q; its op-log is empty, so the mark would "+
				"assert a pending flush that cannot exist there", dst.ContentState)
		}
	})
}
