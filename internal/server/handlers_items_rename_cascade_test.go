package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TestItemRenameCascadeTooLarge_IsPermanentShapedNotRetryable pins the STATUS
// and shape of the item rename cascade refusal (BUG-2804, codex R1 P2).
//
// The store's refusal is a deliberate, permanent decline: the request was
// understood, and retrying it unchanged will be declined identically until the
// workspace's content changes. Without an errors.As arm in handleUpdateItem it
// fell through to writeInternalError and reached the client as a 500 — which
// says the server broke and implies a retry might help. Both halves of that are
// false.
//
// The exact status is pinned rather than "not 500", per the BUG-2803 round-4
// lesson: a test asserting only the absence of the wrong answer passes on any
// of several other wrong answers.
//
// Mirrors TestRenameCascadeTooLarge_IsPermanentShapedNotRetryable on the
// document side, deliberately — the two paths are separate implementations
// with separate sentinels, so parity is a property that has to be tested, not
// inherited.
func TestItemRenameCascadeTooLarge_IsPermanentShapedNotRetryable(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title": "Old",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create target: %d: %s", rr.Code, rr.Body.String())
	}
	var target models.Item
	parseJSON(t, rr, &target)

	// Bodies each well inside the 2 MiB request cap, together over the
	// cascade bound — the same total-not-per-item shape the store test pins,
	// driven through real HTTP requests. That the hostile input is CHEAP to
	// deliver is the point.
	const body = 1 << 20
	perLinker := int64(body) * 2
	linkers := int(store.MaxItemRenameCascadeBytes/perLinker) + 2
	linkerBody := "[[Old]]" + strings.Repeat("x", body-len("[[Old]]"))

	for i := 0; i < linkers; i++ {
		rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
			"title":   fmt.Sprintf("Linker %d", i),
			"content": linkerBody,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create linker %d: %d: %s", i, rr.Code, rr.Body.String())
		}
	}

	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+target.Slug, map[string]interface{}{
		"title": "New",
	})

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413 — a deliberate refusal must not surface as a server fault: %s",
			rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q; a permanent refusal must not invite a retry", got)
	}

	respBody := rr.Body.String()
	if !strings.Contains(respBody, "rename_cascade_too_large") {
		t.Errorf("response lacks the error code a client would switch on: %s", respBody)
	}
	// The actual CAP, not the word "maximum" — a message reading "maximum
	// exceeded" would satisfy a word check and tell the caller nothing about
	// how far over they are. Same reasoning as the document twin's codex
	// round 4.
	if !strings.Contains(respBody, fmt.Sprint(int64(store.MaxItemRenameCascadeBytes))) {
		t.Errorf("response does not state the limit %d: %s", int64(store.MaxItemRenameCascadeBytes), respBody)
	}
	// The internal call path must NOT be published. The store wraps its
	// sentinel on the way up; splicing err.Error() would leak those prefixes
	// to the client, which is why the handler composes from typed fields.
	if strings.Contains(respBody, "store:") || strings.Contains(respBody, "cascade rename:") {
		t.Errorf("response leaks the internal error chain: %s", respBody)
	}

	// And the rename did not take effect.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+target.Slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("re-read target: %d: %s", rr.Code, rr.Body.String())
	}
	var after models.Item
	parseJSON(t, rr, &after)
	if after.Title != "Old" {
		t.Errorf("title = %q after a refused rename, want %q", after.Title, "Old")
	}
}

// TestItemRename_OrdinaryCascadeStillSucceeds is the counterfactual for the
// test above: without it, a handler arm that answered 413 for EVERY rename
// would pass every assertion there.
func TestItemRename_OrdinaryCascadeStillSucceeds(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title": "Old",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create target: %d: %s", rr.Code, rr.Body.String())
	}
	var target models.Item
	parseJSON(t, rr, &target)

	for i := 0; i < 3; i++ {
		rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
			"title":   fmt.Sprintf("Linker %d", i),
			"content": "see [[Old]] for details",
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create linker %d: %d: %s", i, rr.Code, rr.Body.String())
		}
	}

	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+target.Slug, map[string]interface{}{
		"title": "New",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("ordinary rename: got %d, want 200: %s", rr.Code, rr.Body.String())
	}

	// The cascade actually ran — the linkers point at the new title.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections/tasks/items", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list items: %d", rr.Code)
	}
	if n := strings.Count(rr.Body.String(), "[[New]]"); n != 3 {
		t.Errorf("found %d rewritten linkers, want 3: the cascade did not run", n)
	}
}

// TestItemRenameCascadeTooLarge_MappedOnTheCollabSnapshotPath closes codex R2's
// second finding.
//
// handleUpdateItem reaches store.UpdateItemWithParentLink from THREE places,
// each with its own error block: the plain path, the collab-snapshot callback,
// and the collab-edit callback. R1 mapped the first. This pins the second, on
// which the identical store refusal was still answering 500.
//
// Why the miss is worth naming: the CONVE-18 sweep I ran after R1 asked "do
// other HANDLERS reach this?" and correctly answered no. It never asked
// "does THIS handler reach it more than once?" — so the population was scoped
// to the wrong axis, and a grep for the store call inside one function would
// have found all three immediately.
func TestItemRenameCascadeTooLarge_MappedOnTheCollabSnapshotPath(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title": "Old",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create target: %d: %s", rr.Code, rr.Body.String())
	}
	var target models.Item
	parseJSON(t, rr, &target)

	const body = 1 << 20
	perLinker := int64(body) * 2
	linkers := int(store.MaxItemRenameCascadeBytes/perLinker) + 2
	linkerBody := "[[Old]]" + strings.Repeat("x", body-len("[[Old]]"))
	for i := 0; i < linkers; i++ {
		rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
			"title":   fmt.Sprintf("Linker %d", i),
			"content": linkerBody,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create linker %d: %d: %s", i, rr.Code, rr.Body.String())
		}
	}

	// ?source=collab-snapshot with content routes through the collab-snapshot
	// callback rather than the plain path.
	rr = doRequest(srv, "PATCH",
		"/api/v1/workspaces/"+slug+"/items/"+target.Slug+"?source=collab-snapshot",
		map[string]interface{}{
			"title":         "New",
			"content":       "rewritten by the editor",
			"op_log_cursor": 0,
		})

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("collab-snapshot path: got %d, want 413 — the same store refusal the plain path "+
			"answers 413 for: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "rename_cascade_too_large") {
		t.Errorf("response lacks the error code: %s", rr.Body.String())
	}
}

// TestItemRenameCascadeTooLarge_CollabEditPathDoesNotRunTheCascadeTwice closes
// the other half of codex R2's second finding.
//
// The collab-edit path treats most callback errors as recoverable and falls
// through to a direct write — graceful degradation for applier timeouts. A
// cascade refusal is NOT recoverable: it is deterministic, so the fall-through
// re-reads every linking body, re-charges the projection, and refuses
// identically. The caller waits twice for one answer.
//
// STATUS CANNOT PIN THIS. Both behaviours end in 413 — the fall-through reaches
// the plain path's arm — so a status assertion passes either way. The
// observable that discriminates is how much WORK the cascade did, counted via
// the store's build observer.
func TestItemRenameCascadeTooLarge_CollabEditPathDoesNotRunTheCascadeTwice(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title": "Old",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create target: %d: %s", rr.Code, rr.Body.String())
	}
	var target models.Item
	parseJSON(t, rr, &target)

	// Body size chosen so perLinker does NOT divide the cap evenly, and the
	// assertion below is a TOTAL rather than an exact count: the scan charges
	// the same budget before the rewrite loop runs (BUG-2804 / codex R4), so
	// the admitted count is a fixture detail that shifts when that charge
	// changes. Pinning it exactly made this test fail for a reason unrelated to
	// the property it exists for.
	const body = 768 << 10 // 0.75 MiB
	perLinker := int64(body) * 2
	linkers := int(store.MaxItemRenameCascadeBytes/perLinker) + 3
	linkerBody := "[[Old]]" + strings.Repeat("x", body-len("[[Old]]"))
	for i := 0; i < linkers; i++ {
		rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
			"title":   fmt.Sprintf("Linker %d", i),
			"content": linkerBody,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create linker %d: %d: %s", i, rr.Code, rr.Body.String())
		}
	}

	var built int
	srv.store.SetCascadeBuildObserver(func(int) { built++ })
	defer srv.store.SetCascadeBuildObserver(nil)

	// Title + content with collab enabled and no collab-snapshot marker routes
	// through the collab-edit callback.
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+target.Slug, map[string]interface{}{
		"title":   "New",
		"content": "rewritten by the editor",
	})

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("collab-edit path: got %d, want 413: %s", rr.Code, rr.Body.String())
	}
	if built == 0 {
		t.Fatalf("cascade built nothing — the fixture refused before the rewrite loop ran, so this " +
			"test is not exercising the fall-through at all")
	}
	// ONE cascade's worth of work fits inside the budget by construction. TWO
	// cascades — the fall-through re-deriving the same refusal — is about twice
	// the cap, so the total is what discriminates without pinning a count.
	if total := int64(built) * perLinker; total > store.MaxItemRenameCascadeBytes {
		t.Errorf("cascade built %d bodies totalling %d charged bytes for ONE request, over the %d "+
			"cap — a second full cascade ran, which means the deterministic refusal fell through "+
			"to the direct write and was re-derived from scratch",
			built, total, int64(store.MaxItemRenameCascadeBytes))
	}
	t.Logf("built %d bodies (%d charged bytes) for one refused request; cap %d",
		built, int64(built)*perLinker, int64(store.MaxItemRenameCascadeBytes))
}
