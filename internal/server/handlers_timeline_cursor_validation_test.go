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
// unchecked. Postgres refuses a text parameter that carries a NUL, and one
// that is not valid UTF-8 when the DATABASE encoding is UTF8, so a malformed
// cursor came back 500 — the server announcing its own failure for what is a
// client's bad input. SQLite accepts the same bytes and matches nothing, so
// the identical request was a 200 there.
//
// The UTF8 qualification is not pedantry inherited from BUG-2784's
// measurements: a SQL_ASCII Postgres ACCEPTS the invalid-UTF-8 bytes (see
// bindableText's comment for the table), so an unqualified "Postgres refuses
// it" would make the store-level premise below false on a supported
// deployment. The NUL half needs no qualification — it was refused under
// every encoding tested.
//
// The store-level test below pins that premise on Postgres; these pin the
// handler's answer, which is the same on both backends because validation
// happens before any query — since BUG-2784, at the transport, which is why
// these now expect `invalid_query` rather than `invalid_cursor`.

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
		// %FF is not valid UTF-8 in any encoding of it; %00 is a NUL. Both
		// are refused by a UTF8 Postgres (the NUL under every encoding
		// tested, the %FF only under UTF8 — see the header), and both are
		// reachable from a plain URL. The 400s below do not depend on that
		// distinction: since BUG-2784 the transport refuses both before any
		// backend is consulted, which is why these expectations hold on
		// SQLite too.
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
			// invalid_query, NOT invalid_cursor, since BUG-2784. The same
			// two byte classes are now refused for EVERY query parameter by
			// ValidateQuery at the root router, which answers before this
			// handler's own validCursorID check runs. The 400 and the
			// client-error contract this test names are unchanged; the code
			// is less specific, and that is the accepted cost of a rule that
			// covers an unbounded parameter surface no per-site validator
			// can. The handler's guard is kept as its own precondition —
			// see the comment at its call site — and its other two call
			// sites, over ids read from the item's fields blob, are not
			// reachable from the wire at all and keep this code.
			if env.Error.Code != "invalid_query" {
				t.Errorf("error code = %q, want %q", env.Error.Code, "invalid_query")
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

		// This half holds only under a UTF8 DATABASE encoding: a SQL_ASCII
		// Postgres accepts these bytes (BUG-2784 measured it; see
		// bindableText's table).
		//
		// UNDER TODAY'S FIXTURE THE SKIP BELOW CANNOT FIRE, and saying so is
		// the point of writing it. storetest.NewPostgres issues a bare
		// `CREATE DATABASE`, which inherits TEMPLATE1's encoding rather than
		// the encoding of the database PAD_TEST_POSTGRES_URL names — so the
		// fixture hands back a UTF8 database even when pointed at a
		// SQL_ASCII one. Verified: creating a database from inside a
		// SQL_ASCII database yields UTF8 while template1 is UTF8. A review
		// round read this test as able to FAIL on a SQL_ASCII deployment;
		// it cannot, for that reason.
		//
		// The read stays anyway, because the premise is real even where the
		// fixture currently forecloses it: if the fixture ever names the
		// operator's database, or template1 is not UTF8, this reports "the
		// premise does not apply here" instead of failing an assertion about
		// bytes with a confusing message about drivers.
		var enc string
		if err := p.DB().QueryRow("SHOW server_encoding").Scan(&enc); err != nil {
			t.Fatalf("could not read server_encoding, so this test cannot know whether its premise applies: %v", err)
		}
		if enc != "UTF8" {
			t.Skipf("server_encoding is %s, not UTF8 — this divergence is a property of the "+
				"UTF8 encoding, and the transport-level 400 (BUG-2784) does not depend on it either way", enc)
		}

		if _, err := p.ListDocumentActivityBeforeTime("no-such-doc", when, badID, 10); err == nil {
			t.Error("Postgres on a UTF8 database accepted an invalid-UTF-8 cursor: the 500 this validation prevents is no longer reachable, so either the driver changed or the premise moved")
		}
	})
}

// The other direction of the same rule: the server must never EMIT a cursor it
// would then refuse. A structured id comes from the item's fields blob, and a
// JSON NUL escape there reaches the timeline as a real NUL on SQLite
// (Postgres's jsonb refuses it at the door — a one-backend hazard). Handing
// that out as next_before_id would wedge paging on the item: the client sends
// it back and gets a 400 from the validation above.
//
// THE FIXTURE WRITES THE BLOB DIRECTLY, and it did not have to when this test
// was written. The original built the item through the API, on the premise —
// stated in this comment until BUG-2803 — that "nothing validates the fields
// blob on write". That premise is now false: decodeJSON refuses a request
// body whose strings decode to a NUL, including one nested inside a
// JSON-encoded `fields` string, so the API can no longer produce this row.
//
// The DEFENCE this test covers is still live, which is why the test is
// repaired rather than deleted. Rows in this shape can predate the rule, and
// the store has no such check of its own, so anything writing a blob directly
// — a migration, an import, a future non-HTTP writer — can still produce one.
// The timeline must keep refusing to hand out an unusable cursor for data it
// did not create. Writing the row through the store is what makes the test
// about the timeline's defence rather than about the request validator.
func TestTimeline_NeverEmitsACursorItWouldRefuse(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)

	// Three notes so the NUL-bearing one can be the LAST kept entry under a
	// truncating limit — that is what makes it the emitted next_before_id
	// rather than merely an entry id. The clean one is the control: it
	// distinguishes "replaced the unusable id" from "stopped using raw ids".
	notes := `[{"id":"note-PLACEHOLDER-bad","summary":"middle","created_at":"2026-04-02T10:00:01Z"},` +
		`{"id":"note-clean","summary":"newest note","created_at":"2026-04-02T10:00:02Z"},` +
		`{"id":"note-oldest","summary":"oldest","created_at":"2026-04-02T10:00:00Z"}]`
	item := timelineItemWithStructured(t, srv, ws, notes, "")

	// Swap the placeholder for the JSON NUL ESCAPE directly in the stored
	// blob — the six characters, not a raw NUL byte. The blob is stored as
	// JSON text and both backends hold a CHECK/type constraint that a raw NUL
	// violates; the NUL only comes into existence when Go decodes the blob,
	// which is precisely how the timeline ends up with one inside an entry id.
	// The API refuses this body (BUG-2803); the store does not, which is the
	// gap this test exists to cover.
	if _, err := srv.store.DB().Exec(
		`UPDATE items SET fields = REPLACE(fields, 'PLACEHOLDER', ?) WHERE id = ?`,
		string([]byte{'\\', 'u', '0', '0', '0', '0'}), item.ID,
	); err != nil {
		t.Fatalf("inject NUL into the stored fields blob: %v", err)
	}

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
