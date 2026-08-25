package server

import (
	"encoding/json"
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
				t.Fatalf("GET timeline before_id=%s = %d, want %d: %s",
					tc.rawID, rr.Code, tc.want, rr.Body.String())
			}
			if tc.want != http.StatusBadRequest {
				return
			}
			// The status alone would pass for a bare 400 or the wrong code,
			// and clients branch on the code, not the number.
			var env struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode error envelope: %v (body %s)", err, rr.Body.String())
			}
			if env.Error.Code != "invalid_cursor" {
				t.Errorf("error code = %q, want %q", env.Error.Code, "invalid_cursor")
			}
			if env.Error.Message == "" {
				t.Error("error message is empty — the envelope has to say what was wrong")
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

// CHARACTERIZATION, not a regression test: it passes with or without this
// unit's change, because it describes the BACKENDS rather than the handler.
// That is the point — it is the premise the 400 rests on, and if it stops
// holding the 400 needs revisiting rather than silently guarding nothing.
//
// The SQLite half runs everywhere; the Postgres half needs
// PAD_TEST_POSTGRES_URL and skips without it, so an unconfigured run proves
// only that SQLite still accepts the bytes.
func TestListBeforeTime_InvalidUTF8CursorIsADialectDivergence(t *testing.T) {
	const badID = "note-\xff-1"
	when := time.Now().UTC().Add(time.Minute)

	t.Run("sqlite accepts it", func(t *testing.T) {
		s := storetest.NewSQLite(t)
		if _, err := s.ListDocumentActivityBeforeTime("no-such-doc", when, badID, 10); err != nil {
			t.Errorf("SQLite rejected the cursor (%v) — the handler's 400 is still right, but this test's reasoning about WHY needs updating", err)
		}
	})

	t.Run("postgres refuses it", func(t *testing.T) {
		p := storetest.NewPostgres(t) // skips unless PAD_TEST_POSTGRES_URL is set
		if _, err := p.ListDocumentActivityBeforeTime("no-such-doc", when, badID, 10); err == nil {
			t.Error("Postgres accepted an invalid-UTF-8 cursor: the 500 this validation prevents is no longer reachable, so either the driver changed or the premise moved")
		}
	})
}

// The other direction of the same rule: the server must never EMIT a cursor it
// would then refuse. A structured id comes from the item's fields blob, which
// nothing validates on write, so a JSON \u0000 escape reaches the timeline as
// a real NUL on SQLite (Postgres's jsonb refuses it at the door — a
// one-backend hazard). Handing that out as next_before_id would wedge paging
// on the item: the client sends it back and gets a 400 from the validation
// above.
func TestTimeline_NeverEmitsACursorItWouldRefuse(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)

	// Three notes so the NUL-bearing one can be the LAST kept entry under a
	// truncating limit — that is what makes it the emitted next_before_id
	// rather than merely an entry id. The clean one is the control: it
	// distinguishes "replaced the unusable id" from "stopped using raw ids".
	notes := `[{"id":"note-\u0000-bad","summary":"middle","created_at":"2026-04-02T10:00:01Z"},` +
		`{"id":"note-clean","summary":"newest note","created_at":"2026-04-02T10:00:02Z"},` +
		`{"id":"note-oldest","summary":"oldest","created_at":"2026-04-02T10:00:00Z"}]`
	item := timelineItemWithStructured(t, srv, ws, notes, "")

	// The item's own `created` activity plus three notes; limit=3 truncates,
	// so the cursor is the third entry — the NUL-bearing note.
	resp := fetchTimeline(t, srv, ws, item.Slug, "limit=3")
	var sawClean, sawFallback bool
	for _, e := range resp.Entries {
		if !validCursorID(e.ID) {
			t.Errorf("entry %q is not a usable cursor — a client sending it back would be refused", e.ID)
		}
		switch e.ID {
		case "note-clean":
			sawClean = true
		case "note-idx-0":
			sawFallback = true
		}
	}

	// The cursor itself, which is what the client actually sends back.
	if !resp.HasMore || resp.NextBeforeID == "" {
		t.Fatalf("expected a truncated page with a cursor, got has_more=%v next_before_id=%q",
			resp.HasMore, resp.NextBeforeID)
	}
	if !validCursorID(resp.NextBeforeID) {
		t.Errorf("the server emitted next_before_id=%q, which its own validation refuses", resp.NextBeforeID)
	}
	// And it round-trips: the follow-up page is a 200, not the 400 this
	// unit's other half returns for an unusable cursor.
	rr := timelineRaw(t, srv, ws, item.Slug,
		"limit=3&before="+resp.NextBefore+"&before_id="+resp.NextBeforeID)
	if rr.Code != http.StatusOK {
		t.Errorf("paging with the server's own cursor = %d, want 200: %s", rr.Code, rr.Body.String())
	}

	if !sawClean {
		t.Error("the clean id was not used verbatim — the fallback is for unusable ids only")
	}
	if !sawFallback {
		t.Errorf("the NUL-bearing id did not take the positional fallback; entries: %+v", resp.Entries)
	}
}

// A cursor is a PAIR. `before_id` alone could never match anything — the id is
// the tie-break at the cursor instant, and `before` defaults to a moment no row
// shares — so it was accepted and silently ignored while the caller paged from
// the beginning believing otherwise (codex round 4).
func TestTimeline_OneSidedCursorIsRejected(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := timelineItemWithStructured(t, srv, ws, "", "")
	now := time.Now().UTC().Format(time.RFC3339)

	for _, tc := range []struct {
		name, query string
		want        int
	}{
		{
			name:  "before_id without before",
			query: "before_id=0e2f3b1c-1111-4111-8111-111111111111",
			want:  http.StatusBadRequest,
		},
		{
			// Deliberately supported: the sentinel exists for exactly this
			// external-client shape, so rejecting the pair symmetrically would
			// break a case the handler goes out of its way to serve.
			name:  "before without before_id (control)",
			query: "before=" + now,
			want:  http.StatusOK,
		},
		{
			name:  "both (control)",
			query: "before=" + now + "&before_id=0e2f3b1c-1111-4111-8111-111111111111",
			want:  http.StatusOK,
		},
		{
			name:  "neither (control)",
			query: "limit=5",
			want:  http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rr := timelineRaw(t, srv, ws, item.Slug, tc.query)
			if rr.Code != tc.want {
				t.Errorf("GET timeline?%s = %d, want %d: %s", tc.query, rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}
