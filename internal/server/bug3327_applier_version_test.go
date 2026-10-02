package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BUG-3327: a body edit that takes the designated-applier path (a tab has the
// item open) wrote NO version row: the row write runs with Content nil, and
// the store's version write lives only on the content path. An agent's edit
// made while someone had the item open was therefore missing from History.
// It must leave a version of the body it replaced, attributed to the writer,
// resolvable BEFORE the tab's flush lands in items.content.
func TestBUG3327_ApplierPathBodyEditLeavesAVersion(t *testing.T) {
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	// Long and nearly identical bodies, so a reverse patch is smaller than
	// the full body and the store would choose it: the mutant this test must
	// catch is the applier path writing a patch against a stale row.
	shared := strings.Repeat("A paragraph that both bodies share, line after line.\n", 40)
	previous := shared + "PREVIOUS ending, written before any tab connected"
	next := shared + "NEW ending, sent by an agent through the applier"
	if rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug,
		map[string]interface{}{"content": previous}); rr.Code != http.StatusOK {
		t.Fatalf("seed PATCH: %d %s", rr.Code, rr.Body.String())
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

	before, err := srv.store.ListItemVersions(item.ID)
	if err != nil {
		t.Fatal(err)
	}

	// An AGENT's edit while the user's tab is open: the case History lost.
	rr := doRequestWithHeaders(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug,
		map[string]interface{}{"content": next},
		map[string]string{"X-Pad-Agent": "test-agent"})
	if rr.Code != http.StatusOK {
		t.Fatalf("applier-path PATCH: %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "applied_pending_flush") {
		t.Fatalf("the PATCH did not take the applier path (no content_outcome): %s", rr.Body.String())
	}

	after, err := srv.store.ListItemVersions(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("versions %d -> %d: the applier-path edit left no version row", len(before), len(after))
	}

	// Resolve against the row as it stands NOW, before any flush: the row
	// still holds `previous`, and the new version must still read as it.
	row, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := srv.store.ListItemVersionsResolved(item.ID, row.Content)
	if err != nil {
		t.Fatal(err)
	}
	newest := resolved[0]
	if newest.Content != previous {
		t.Errorf("newest version resolves to %q, want the replaced body %q", newest.Content, previous)
	}
	if newest.CreatedBy != "agent" {
		t.Errorf("newest version created_by = %q, want agent", newest.CreatedBy)
	}

	// The tab's flush lands the new body in the row (codex review). It must
	// not store the replaced body a second time, which would put the agent's
	// change in History under the tab user's row; and the chain must still
	// resolve, with the agent's row's change reading previous -> next.
	if rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"?source=collab-snapshot",
		map[string]interface{}{"content": next}); rr.Code != http.StatusOK {
		t.Fatalf("flush PATCH: %d %s", rr.Code, rr.Body.String())
	}
	flushed, err := srv.store.ListItemVersions(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(flushed) != len(after) {
		t.Errorf("versions %d -> %d across the flush: the replaced body was stored twice", len(after), len(flushed))
	}
	row, err = srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Content != next {
		t.Fatalf("flush did not land: row = %q", row.Content)
	}
	resolved, err = srv.store.ListItemVersionsResolved(item.ID, row.Content)
	if err != nil {
		t.Fatal(err)
	}
	if resolved[0].Content != previous || resolved[0].CreatedBy != "agent" {
		t.Errorf("after the flush, newest version = %q by %q, want the replaced body by agent", resolved[0].Content, resolved[0].CreatedBy)
	}
	d, err := srv.store.GetItemVersionDiff(item.ID, resolved[0].ID, row.Content)
	if err != nil {
		t.Fatal(err)
	}
	if d.Before != previous || d.After != next {
		t.Errorf("the agent's version records %q -> %q, want previous -> next", d.Before, d.After)
	}
	// Older versions still resolve: the seed write's version is the body
	// before `previous`, which was empty.
	if last := resolved[len(resolved)-1]; last.Content != "" {
		t.Errorf("oldest version resolves to %q, want the empty initial body", last.Content)
	}
}

// The flush dedupe is limited to flushes (codex review, round 2): a user's
// A->B (versioned) then B->A (throttled, same writer) leaves the newest row
// holding A, and a DIFFERENT writer's A->C must still get its own row rather
// than have its change read as the first writer's.
func TestBUG3327_DedupeSkipsOnlyFlushes(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	patch := func(content string, headers map[string]string) {
		t.Helper()
		rr := doRequestWithHeaders(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug,
			map[string]interface{}{"content": content}, headers)
		if rr.Code != http.StatusOK {
			t.Fatalf("PATCH %q: %d %s", content, rr.Code, rr.Body.String())
		}
	}
	agent := map[string]string{"X-Pad-Agent": "test-agent"}
	patch("body A", nil)   // the user writes A
	patch("body B", agent) // a different writer: versions A as a full body
	patch("body A", agent) // same writer within the hour: throttled, no row
	before, err := srv.store.ListItemVersions(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 || before[0].IsDiff || before[0].Content != "body A" {
		t.Fatalf("fixture: the newest version must be a full-body \"body A\", got %+v", before)
	}
	patch("body C", nil) // the user again: a different writer from the newest row
	after, err := srv.store.ListItemVersions(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("versions %d -> %d: the user's A->C was deduped against the agent's row", len(before), len(after))
	}
}
