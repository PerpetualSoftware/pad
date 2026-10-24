package server

import (
	"net/http"
	"strings"
	"testing"
)

// TASK-4062: every unguarded update the PLAN-3555 counter counts is also
// logged, one line per Inc(), with the counter's door and kind; a guarded
// write logs nothing; the line carries identifiers only, never the body or
// the title. Metrics are left unset on purpose: the counter has no scraper
// on a self-host box, so the log must not depend on it being wired.
func TestTASK4062_UnguardedWriteIsLoggedLikeItIsCounted(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	path := "/api/v1/workspaces/" + slug + "/items/" + item.Slug
	cli := map[string]string{"X-Pad-Client": "cli"}
	read, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Ref == "" {
		t.Fatal("premise: the item has no ref to log")
	}
	logs := captureLogs(t)

	const bodySentinel = "body-sentinel-4062"
	const titleSentinel = "Title sentinel 4062"
	patch := func(body map[string]any) {
		t.Helper()
		if rr := doRequestWithHeaders(srv, "PATCH", path, body, cli); rr.Code != http.StatusOK {
			t.Fatalf("PATCH %v: %d %s", body, rr.Code, rr.Body.String())
		}
	}
	lines := func() []string {
		t.Helper()
		var out []string
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, `msg="content write unguarded"`) {
				out = append(out, l)
			}
		}
		return out
	}

	patch(map[string]any{"content": bodySentinel})
	got := lines()
	if len(got) != 1 {
		t.Fatalf("an unguarded content write logged %d lines, want 1:\n%s", len(got), logs.String())
	}
	for _, want := range []string{" door=cli ", " kind=content ", " item_ref=" + read.Ref + " ", " item_id=" + item.ID + " ", " actor_kind=user "} {
		if !strings.Contains(got[0]+" ", want) {
			t.Fatalf("content line lacks %q: %s", want, got[0])
		}
	}

	now, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	patch(map[string]any{"content": "guarded", "expected_seq": now.Seq})
	if got := lines(); len(got) != 1 {
		t.Fatalf("a seq-guarded write was logged as unguarded: %d lines", len(got))
	}

	// Last: a rename moves the slug the path above is built from.
	patch(map[string]any{"title": titleSentinel})
	got = lines()
	if len(got) != 2 {
		t.Fatalf("an unguarded field-only write logged %d lines in total, want 2:\n%s", len(got), logs.String())
	}
	if !strings.Contains(got[1]+" ", " kind=fields ") || strings.Contains(got[1], "kind=content") {
		t.Fatalf("the field-only line does not say kind=fields: %s", got[1])
	}

	for _, sentinel := range []string{bodySentinel, titleSentinel, "Title", "sentinel"} {
		for _, l := range got {
			if strings.Contains(l, sentinel) {
				t.Fatalf("a log line carries written text %q: %s", sentinel, l)
			}
		}
	}
}
