package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-2798 / BUG-2796, at the route rather than at the validator.
//
// CONVE-19: models.ValidateDocumentTitle having a test proves the validator
// works, not that anything calls it. Document UPDATE in particular validated
// doc_type and status and did NOT validate title — the one field that drives
// the rename cascade — so the binding is the claim worth testing. These drive
// real requests through srv.ServeHTTP.

func createDocForTest(t *testing.T, srv *Server, wsSlug, title, content string) string {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/documents", map[string]interface{}{
		"title":    title,
		"content":  content,
		"doc_type": "notes",
		"status":   "active",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create document %q: got %d, want 201: %s", title, rr.Code, rr.Body.String())
	}
	var doc models.Document
	parseJSON(t, rr, &doc)
	return doc.ID
}

// TestDocumentTitleValidation_IsWiredOnBothWriteDoors covers create and update
// with the same table. The update legs are the ones that would have caught the
// original defect: before this fix a PATCH could set any title at all.
func TestDocumentTitleValidation_IsWiredOnBothWriteDoors(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	docID := createDocForTest(t, srv, slug, "Starting Title", "body")

	for _, tc := range []struct {
		name  string
		title string
		want  int
	}{
		{"over the length bound", strings.Repeat("a", models.MaxDocumentTitleRunes+1), http.StatusBadRequest},
		{"bug-2796 bracket repro", `A]] [[A`, http.StatusBadRequest},
		{"empty", "", http.StatusBadRequest},

		// Controls. Without these a handler that rejected every title — or
		// one that rejected any title containing punctuation — passes the
		// legs above.
		{"at the length bound", strings.Repeat("a", models.MaxDocumentTitleRunes), http.StatusOK},
		{"contains a literal pipe", `Alpha|Beta`, http.StatusOK},
		{"ordinary", "A Renamed Document", http.StatusOK},
	} {
		t.Run("update/"+tc.name, func(t *testing.T) {
			rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/documents/"+docID, map[string]interface{}{
				"title": tc.title,
			})
			if rr.Code != tc.want {
				t.Errorf("PATCH title=%.40q: got %d, want %d: %s", tc.title, rr.Code, tc.want, rr.Body.String())
			}
		})
	}

	for _, tc := range []struct {
		name  string
		title string
		want  int
	}{
		{"over the length bound", strings.Repeat("b", models.MaxDocumentTitleRunes+1), http.StatusBadRequest},
		{"bug-2796 bracket repro", `B]] [[B`, http.StatusBadRequest},
		{"ordinary", "Another Document", http.StatusCreated},
	} {
		t.Run("create/"+tc.name, func(t *testing.T) {
			rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/documents", map[string]interface{}{
				"title":    tc.title,
				"doc_type": "notes",
				"status":   "active",
			})
			if rr.Code != tc.want {
				t.Errorf("POST title=%.40q: got %d, want %d: %s", tc.title, rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

// TestRenameCascadeTooLarge_IsPermanentShapedNotRetryable pins the response
// SHAPE, which is the part a client acts on.
//
// A cascade refusal and a cascade CONTENTION failure both abort a rename and
// both come back from the same store call, but they mean opposite things to a
// caller: contention is 503 + Retry-After ("someone got there first"), and
// this is permanent ("this rename cannot be performed as asked"). Answering
// this one from the retryable family would tell a client to retry forever.
func TestRenameCascadeTooLarge_IsPermanentShapedNotRetryable(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	target := createDocForTest(t, srv, slug, "A", "the document being renamed")

	// Three linking documents, each projecting under the cap, together over
	// it — the same total-not-per-document shape the store test pins, driven
	// through real requests. Each body is ~135 KB, well inside the 2 MiB
	// request cap, which is the point: the hostile input is cheap to deliver.
	const newTitleLen = 255
	const linkers = 3
	perDoc := (store.MaxRenameCascadeRetainedBytes / linkers) + (store.MaxRenameCascadeRetainedBytes / (linkers * 4))
	occurrences := perDoc / (5 + 5 + (newTitleLen - 1))
	body := strings.Repeat("[[A]]", occurrences)

	for i := 0; i < linkers; i++ {
		createDocForTest(t, srv, slug, "Linker"+string(rune('a'+i)), body)
	}

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/documents/"+target, map[string]interface{}{
		"title": strings.Repeat("T", newTitleLen),
	})

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q; a permanent refusal must not invite a retry", got)
	}
	if b := rr.Body.String(); !strings.Contains(b, "rename_cascade_too_large") {
		t.Errorf("response lacks the error code a client would switch on: %s", b)
	}
	if b := rr.Body.String(); !strings.Contains(b, "maximum") {
		t.Errorf("response lacks the projection; the caller cannot tell how far over they are: %s", b)
	}
}

// TestRenameCascadeTooLarge_IsNotMisreportedAsATitleCollision pins codex round
// 3's P2 at the handler.
//
// The UNIQUE-constraint arm identifies its error by SUBSTRING, and the refusal
// error carries the caller's own title verbatim. So a document renamed to a
// title containing the words "UNIQUE constraint" came back as a 409 title
// collision — telling the caller to pick a different name for a rename that
// was actually refused for size, and that would fail identically under any
// name.
//
// The fix is ordering: sentinels this handler knows by identity are tested
// before any error is classified by what its text happens to contain. The test
// is here rather than in the store because the defect is entirely in the
// handler's classification order.
func TestRenameCascadeTooLarge_IsNotMisreportedAsATitleCollision(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	target := createDocForTest(t, srv, slug, "A", "the document being renamed")

	const linkers = 3
	perDoc := (store.MaxRenameCascadeRetainedBytes / linkers) + (store.MaxRenameCascadeRetainedBytes / (linkers * 4))
	// The title is padded to the bound and CONTAINS the substring the other
	// arm matches on.
	newTitle := "UNIQUE constraint " + strings.Repeat("t", models.MaxDocumentTitleRunes-len("UNIQUE constraint "))
	occurrences := perDoc / (5 + 5 + (len(newTitle) - 1))
	body := strings.Repeat("[[A]]", occurrences)

	for i := 0; i < linkers; i++ {
		createDocForTest(t, srv, slug, "Linker"+string(rune('a'+i)), body)
	}

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/documents/"+target, map[string]interface{}{
		"title": newTitle,
	})

	if rr.Code == http.StatusConflict {
		t.Fatalf("got 409 — the size refusal was classified by the title's own text as a name collision: %s", rr.Body.String())
	}
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413: %s", rr.Code, rr.Body.String())
	}
}
