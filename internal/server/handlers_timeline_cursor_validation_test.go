package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-2774. `before_id` went from the query string into the cursor predicate
// unchecked. Postgres refuses a text parameter that is not valid UTF-8 or that
// carries a NUL, so a malformed cursor came back 500 — the server announcing
// its own failure for what is a client's bad input. SQLite accepts the same
// bytes and matches nothing, so the identical request was a 200 there.
//
// The store-level test below pins that premise on Postgres; these pin the
// handler's answer, which is the same on both backends because validation
// happens before any query.

func timelineRaw(t *testing.T, srv *Server, ws, slug, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET",
		"/api/v1/workspaces/"+ws+"/items/"+slug+"/timeline?"+rawQuery, nil)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func TestTimeline_MalformedBeforeIDIsClientError(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := timelineItemWithStructured(t, srv, ws, "", "")
	cursor := "before=" + time.Now().UTC().Format(time.RFC3339) + "&before_id="

	for _, tc := range []struct {
		name  string
		rawID string
		want  int
	}{
		// %FF is not valid UTF-8 in any encoding of it; %00 is a NUL. Both are
		// what Postgres refuses, and both are reachable from a plain URL.
		{name: "invalid utf-8", rawID: "%FF", want: http.StatusBadRequest},
		{name: "embedded NUL", rawID: "note%001", want: http.StatusBadRequest},
		{name: "invalid utf-8 mid-string", rawID: "note-%FF-1", want: http.StatusBadRequest},
		// Controls. Without them "reject every before_id" passes the legs
		// above, and paging would be dead rather than merely honest.
		{name: "a uuid (control)", rawID: "0e2f3b1c-1111-4111-8111-111111111111", want: http.StatusOK},
		{name: "a structured id (control)", rawID: "note-1775153870894988317", want: http.StatusOK},
		// Not a shape this server emits, but valid UTF-8 with no NUL, so the
		// database has no objection and neither does the handler. The rule is
		// about what can be BOUND, not about what looks like an id — the
		// deliberate absence of a length or format bound (see validCursorID).
		{name: "long unicode id (control)", rawID: "n%C3%B8te-" + longRunOfAs(300), want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rr := timelineRaw(t, srv, ws, item.Slug, cursor+tc.rawID)
			if rr.Code != tc.want {
				t.Errorf("GET timeline before_id=%s = %d, want %d: %s",
					tc.rawID, rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func longRunOfAs(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

// The premise the 400 exists to prevent, pinned where it is real: on Postgres
// the query itself FAILS on such a parameter, which is what surfaced as a 500.
// Skipped on SQLite, where the same call succeeds and returns nothing — the
// divergence is the point, so both halves are asserted in the one place that
// can see both.
func TestListBeforeTime_InvalidUTF8CursorIsADialectDivergence(t *testing.T) {
	pg := storetest.NewPostgres(t) // skips unless PAD_TEST_POSTGRES_URL is set
	sqlite := storetest.NewSQLite(t)

	const badID = "note-\xff-1"
	when := time.Now().UTC().Add(time.Minute)

	if _, err := sqlite.ListDocumentActivityBeforeTime("no-such-doc", when, badID, 10); err != nil {
		t.Errorf("SQLite rejected the cursor (%v) — if this backend has started refusing it too, the handler's 400 is still right but this test's reasoning needs updating", err)
	}
	if _, err := pg.ListDocumentActivityBeforeTime("no-such-doc", when, badID, 10); err == nil {
		t.Error("Postgres accepted an invalid-UTF-8 cursor: the 500 this validation prevents is no longer reachable, so either the driver changed or the premise moved")
	}
}
