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
