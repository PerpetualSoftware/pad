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
}
