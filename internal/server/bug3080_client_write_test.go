package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"
)

// BUG-3080: one browser tab's content writes to an item are ordered by the
// `client_write` counter it stamps on them. A write whose counter is below one
// already APPLIED for the same (item, tab) is refused 409 `superseded_write`
// and not written. The teardown flushes are the case: both are dispatched
// before the tab sees either result, so no version token can order them.

func patchContent(t *testing.T, srv *Server, slug, itemSlug, query string, body map[string]interface{}) (int, map[string]any) {
	t.Helper()
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+itemSlug+query, body)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func stamped(content, tab string, n int64) map[string]interface{} {
	return map[string]interface{}{"content": content, "client_write": map[string]interface{}{"tab": tab, "n": n}}
}

func contentOf(t *testing.T, srv *Server, id string) string {
	t.Helper()
	it, err := srv.store.GetItem(id)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	return it.Content
}

func TestBUG3080_OlderWriteLandingLastIsRefusedAndNotWritten(t *testing.T) {
	for _, path := range []struct{ name, query string }{
		{"plain content PATCH (raw saver)", ""},
		{"collab-snapshot flush", "?source=collab-snapshot"},
	} {
		t.Run(path.name, func(t *testing.T) {
			srv := testServer(t)
			slug := createWSWithCollections(t, srv)
			item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

			// The NEWER write lands first...
			if code, body := patchContent(t, srv, slug, item.Slug, path.query, stamped("newer", "tab-a", 2)); code != http.StatusOK {
				t.Fatalf("the newer write must apply; got %d %v", code, body)
			}
			// ...then the OLDER one arrives.
			code, body := patchContent(t, srv, slug, item.Slug, path.query, stamped("older", "tab-a", 1))
			if code != http.StatusConflict {
				t.Fatalf("the older write must be refused 409; got %d %v", code, body)
			}
			errObj, _ := body["error"].(map[string]any)
			if errObj["code"] != "superseded_write" {
				t.Fatalf("want code superseded_write; got %v", body)
			}
			details, _ := errObj["details"].(map[string]any)
			if details["superseded_by"] != float64(2) || details["n"] != float64(1) {
				t.Fatalf("the refusal must name the n it lost to; got %v", details)
			}
			if got := contentOf(t, srv, item.ID); got != "newer" {
				t.Fatalf("BUG-3080: the older write overwrote the newer; content=%q", got)
			}
		})
	}
}

func TestBUG3080_InOrderWritesBothApply(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	for n, content := range []string{"one", "two", "three"} {
		if code, body := patchContent(t, srv, slug, item.Slug, "", stamped(content, "tab-a", int64(n+1))); code != http.StatusOK {
			t.Fatalf("write %d must apply; got %d %v", n+1, code, body)
		}
	}
	// An EQUAL counter is the last applied write again, not an older one.
	if code, body := patchContent(t, srv, slug, item.Slug, "", stamped("three", "tab-a", 3)); code != http.StatusOK {
		t.Fatalf("a repeat of the last applied counter must apply; got %d %v", code, body)
	}
	if got := contentOf(t, srv, item.ID); got != "three" {
		t.Fatalf("content=%q", got)
	}
}

func TestBUG3080_TabsAreIndependent(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	if code, _ := patchContent(t, srv, slug, item.Slug, "", stamped("tab a at 5", "tab-a", 5)); code != http.StatusOK {
		t.Fatalf("tab a write must apply; got %d", code)
	}
	// Another tab's sequence starts from nothing: its n=1 is not "older".
	if code, body := patchContent(t, srv, slug, item.Slug, "", stamped("tab b at 1", "tab-b", 1)); code != http.StatusOK {
		t.Fatalf("another tab is another writer, not ordered against tab a; got %d %v", code, body)
	}
	// The mark is per ITEM too.
	other := createTaskWithFields(t, srv, slug, "Other", `{"status":"open"}`)
	if code, body := patchContent(t, srv, slug, other.Slug, "", stamped("other item", "tab-a", 1)); code != http.StatusOK {
		t.Fatalf("tab a's mark on one item must not order its writes to another; got %d %v", code, body)
	}
}

func TestBUG3080_UnstampedWriteIsUnaffectedByAnyMark(t *testing.T) {
	// The CLI and MCP never send client_write. A tab having a high mark on
	// the row must not change what they get.
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	if code, _ := patchContent(t, srv, slug, item.Slug, "", stamped("from the tab", "tab-a", 99)); code != http.StatusOK {
		t.Fatalf("stamped write must apply; got %d", code)
	}
	code, body := patchContent(t, srv, slug, item.Slug, "", map[string]interface{}{"content": "from the CLI"})
	if code != http.StatusOK {
		t.Fatalf("an unstamped write must never be refused by a mark; got %d %v", code, body)
	}
	if got := contentOf(t, srv, item.ID); got != "from the CLI" {
		t.Fatalf("content=%q", got)
	}
	// And it leaves the tab's mark where it was: its next write still orders.
	if code, _ := patchContent(t, srv, slug, item.Slug, "", stamped("tab older", "tab-a", 50)); code != http.StatusConflict {
		t.Fatalf("an unstamped write must not reset the tab's mark; got %d", code)
	}
}

func TestBUG3080_AFailedWriteDoesNotMoveTheMark(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	// Refused on something other than order: an expected_seq nobody issued.
	body := stamped("never lands", "tab-a", 7)
	body["expected_seq"] = 999999
	if code, out := patchContent(t, srv, slug, item.Slug, "", body); code != http.StatusConflict {
		t.Fatalf("the stale token must refuse; got %d %v", code, out)
	}
	// n=7 was never APPLIED, so a lower n is not superseded by it.
	if code, out := patchContent(t, srv, slug, item.Slug, "", stamped("lands", "tab-a", 3)); code != http.StatusOK {
		t.Fatalf("a failed write must not advance the mark; got %d %v", code, out)
	}
	if got := contentOf(t, srv, item.ID); got != "lands" {
		t.Fatalf("content=%q", got)
	}
}

func TestBUG3080_MalformedStampIsA400(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	for _, stamp := range []map[string]interface{}{
		{"tab": "", "n": 1},
		{"tab": "tab-a", "n": 0},
		{"tab": "tab-a", "n": -3},
		{"tab": string(make([]byte, 65)), "n": 1},
	} {
		code, out := patchContent(t, srv, slug, item.Slug, "", map[string]interface{}{"content": "x", "client_write": stamp})
		if code != http.StatusBadRequest {
			t.Fatalf("stamp %v: want 400; got %d %v", stamp, code, out)
		}
	}
}

// The check, the write and the mark are ONE step per (item, tab). The older
// write is held after passing its check; the newer one must WAIT for it rather
// than write first, or the older would then land last — the defect, inside
// the server.
func TestBUG3080_NewerWriteWaitsForAnOlderOneMidWrite(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	holdOlder := make(chan struct{})
	olderInside := make(chan struct{})
	var once sync.Once
	srv.afterClientWriteCheck = func(itemID string, n int64) {
		if n == 1 {
			once.Do(func() { close(olderInside) })
			<-holdOlder
		}
	}

	olderDone := make(chan int, 1)
	go func() {
		code, _ := patchContent(t, srv, slug, item.Slug, "", stamped("older", "tab-a", 1))
		olderDone <- code
	}()
	<-olderInside

	newerDone := make(chan int, 1)
	go func() {
		code, _ := patchContent(t, srv, slug, item.Slug, "", stamped("newer", "tab-a", 2))
		newerDone <- code
	}()
	select {
	case code := <-newerDone:
		t.Fatalf("the newer write finished (%d) while the older one was inside its write — "+
			"the check and the write are not one step", code)
	case <-time.After(300 * time.Millisecond):
	}

	close(holdOlder)
	if code := <-olderDone; code != http.StatusOK {
		t.Fatalf("older write: %d", code)
	}
	if code := <-newerDone; code != http.StatusOK {
		t.Fatalf("newer write: %d", code)
	}
	if got := contentOf(t, srv, item.ID); got != "newer" {
		t.Fatalf("content=%q, want the newer write", got)
	}
}

func TestBUG3080_MarksAgeOutAndStayBounded(t *testing.T) {
	m := newClientWriteMarks()
	clock := time.Unix(1_000_000, 0)
	m.now = func() time.Time { return clock }

	e, applied := m.acquire("item", "tab")
	if applied != 0 {
		t.Fatalf("a new key starts at 0; got %d", applied)
	}
	m.release(e, 4, true)
	e, applied = m.acquire("item", "tab")
	if applied != 4 {
		t.Fatalf("the mark must persist; got %d", applied)
	}
	m.release(e, 4, true)

	// Fill to the cap with other keys, then age everything.
	for i := 0; len(m.entries) < clientWriteMarkMaxEntries; i++ {
		e, _ := m.acquire("item", "t"+string(rune('a'+i%26))+time.Duration(i).String())
		m.release(e, 1, true)
	}
	clock = clock.Add(clientWriteMarkMaxAge + time.Second)
	e, _ = m.acquire("fresh", "tab")
	m.release(e, 1, true)
	if len(m.entries) != 1 {
		t.Fatalf("idle entries past the age must be dropped when the cap is hit; %d remain", len(m.entries))
	}

	// A HELD entry is never evicted, even over the cap.
	held, _ := m.acquire("held", "tab")
	for i := 0; len(m.entries) < clientWriteMarkMaxEntries; i++ {
		e, _ := m.acquire("x", time.Duration(i).String())
		m.release(e, 1, true)
	}
	e, _ = m.acquire("one-more", "tab")
	m.release(e, 1, true)
	if _, ok := m.entries[clientWriteKey{item: "held", tab: "tab"}]; !ok {
		t.Fatal("an entry a request holds was evicted")
	}
	if len(m.entries) > clientWriteMarkMaxEntries {
		t.Fatalf("the map grew past its cap with idle entries to evict: %d", len(m.entries))
	}
	m.release(held, 1, true)
}

func TestBUG3080_AStampOnANonContentWriteNeitherOrdersNorMarks(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	// A field-only PATCH carrying a high stamp (codex round 1). A field, not a
	// title: a rename moves the slug this test addresses the item by.
	code, out := patchContent(t, srv, slug, item.Slug, "", map[string]interface{}{
		"fields_patch": map[string]interface{}{"status": "done"}, "client_write": map[string]interface{}{"tab": "tab-a", "n": 50},
	})
	if code != http.StatusOK {
		t.Fatalf("field write: %d %v", code, out)
	}
	// This tab's next CONTENT write, at a lower n, is not superseded by it.
	if code, out := patchContent(t, srv, slug, item.Slug, "", stamped("real content", "tab-a", 2)); code != http.StatusOK {
		t.Fatalf("a non-content write must not raise the content mark; got %d %v", code, out)
	}
	if got := contentOf(t, srv, item.ID); got != "real content" {
		t.Fatalf("content=%q", got)
	}
}
