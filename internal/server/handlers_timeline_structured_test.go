package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-2301: `pad item note` / `pad item decide` wrote structured entries that
// no web surface rendered — the renderer shipped in c61f4cda and was deleted
// the next day as collateral of the unified-timeline PR (998716ae). These
// tests cover the replacement: the two kinds are merged server-side by the
// timeline endpoint, so the existing cursor pagination carries them.

// timelineItemWithStructured creates an item whose fields blob carries the
// given raw implementation_notes / decision_log JSON, going through the real
// HTTP create path so the entries are hydrated exactly as production does it.
// Pass raw JSON so a test can express shapes the Go structs allow but the CLI
// never writes (missing ids, missing timestamps, a non-array field).
func timelineItemWithStructured(t *testing.T, srv *Server, wsSlug, notesJSON, decisionsJSON string) *models.Item {
	t.Helper()

	fields := `{"status":"open"`
	if notesJSON != "" {
		fields += `,"implementation_notes":` + notesJSON
	}
	if decisionsJSON != "" {
		fields += `,"decision_log":` + decisionsJSON
	}
	fields += `}`

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections/tasks/items", map[string]any{
		"title":  "structured subject",
		"source": "cli",
		"fields": fields,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item = %d: %s", rr.Code, rr.Body.String())
	}
	var it models.Item
	parseJSON(t, rr, &it)
	return &it
}

func fetchTimeline(t *testing.T, srv *Server, wsSlug, itemSlug, query string) models.TimelineResponse {
	t.Helper()
	path := "/api/v1/workspaces/" + wsSlug + "/items/" + itemSlug + "/timeline"
	if query != "" {
		path += "?" + query
	}
	rr := doRequest(srv, "GET", path, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET timeline = %d: %s", rr.Code, rr.Body.String())
	}
	var resp models.TimelineResponse
	parseJSON(t, rr, &resp)
	return resp
}

func kindsOf(entries []models.TimelineEntry) map[string]int {
	out := map[string]int{}
	for _, e := range entries {
		out[e.Kind]++
	}
	return out
}

// The headline regression: notes and decisions written into the fields blob
// must reach the timeline endpoint's response. Before this fix the endpoint
// merged only comments, activities and versions, so the response carried zero
// entries for either kind no matter how many the item held.
func TestItemTimeline_SurfacesNotesAndDecisions(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)

	item := timelineItemWithStructured(t, srv, ws,
		`[{"id":"note-1","summary":"first note","details":"body one","created_at":"2026-04-02T10:00:00Z","created_by":"agent"},
		  {"id":"note-2","summary":"second note","created_at":"2026-04-02T12:00:00Z","created_by":"user"}]`,
		`[{"id":"decision-1","decision":"use reserved field keys","rationale":"no new table","created_at":"2026-04-02T11:00:00Z","created_by":"user"}]`)

	resp := fetchTimeline(t, srv, ws, item.Slug, "")
	counts := kindsOf(resp.Entries)
	if counts["note"] != 2 {
		t.Errorf("expected 2 note entries in the timeline, got %d (kinds: %v)", counts["note"], counts)
	}
	if counts["decision"] != 1 {
		t.Errorf("expected 1 decision entry in the timeline, got %d (kinds: %v)", counts["decision"], counts)
	}

	// The payload has to travel, not just the kind label — the card renders
	// summary/details/decision/rationale, so an entry with a nil body would
	// render blank and still satisfy a count-only assertion.
	var sawNoteBody, sawDecisionBody bool
	for _, e := range resp.Entries {
		if e.Kind == "note" && e.Note != nil && e.Note.Summary == "first note" && e.Note.Details == "body one" {
			sawNoteBody = true
		}
		if e.Kind == "decision" && e.Decision != nil && e.Decision.Decision == "use reserved field keys" &&
			e.Decision.Rationale == "no new table" {
			sawDecisionBody = true
		}
	}
	if !sawNoteBody {
		t.Error("note entry reached the timeline without its summary/details payload")
	}
	if !sawDecisionBody {
		t.Error("decision entry reached the timeline without its decision/rationale payload")
	}

	// Chronological placement among the other kinds is the point of merging
	// server-side rather than appending client-side: newest first.
	var order []string
	for _, e := range resp.Entries {
		if e.Kind == "note" || e.Kind == "decision" {
			order = append(order, e.ID)
		}
	}
	want := []string{"note-2", "decision-1", "note-1"}
	if len(order) != len(want) {
		t.Fatalf("structured entry order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("structured entry order = %v, want %v (newest first)", order, want)
		}
	}
}

// Actor attribution has to survive the trip, because these entries are the one
// place created_by is client-declared (BUG-2542) and the UI labels them with it.
func TestItemTimeline_StructuredEntriesCarryActor(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)

	item := timelineItemWithStructured(t, srv, ws,
		`[{"id":"note-1","summary":"agent wrote this","created_at":"2026-04-02T10:00:00Z","created_by":"agent"}]`,
		`[{"id":"decision-1","decision":"human decided this","created_at":"2026-04-02T11:00:00Z","created_by":"user"}]`)

	resp := fetchTimeline(t, srv, ws, item.Slug, "")
	// Count as well as check: a loop that only asserts on entries it finds
	// passes vacuously when the merge drops them, which is the very failure
	// this file exists to catch (CONVE-12).
	var checked int
	for _, e := range resp.Entries {
		switch e.Kind {
		case "note":
			checked++
			if e.Actor != "agent" {
				t.Errorf("note actor = %q, want %q", e.Actor, "agent")
			}
		case "decision":
			checked++
			if e.Actor != "user" {
				t.Errorf("decision actor = %q, want %q", e.Actor, "user")
			}
		}
	}
	if checked != 2 {
		t.Fatalf("asserted on %d structured entries, want 2 — the assertions above "+
			"are vacuous unless both entries are present", checked)
	}
}

// Paging correctness. Notes and decisions do not come from a cursor query —
// they arrive whole on the item — so without an explicit filter they would
// reappear on EVERY page. This drives the same (created_at, id) predicate the
// SQL sources use, through the real handler.
func TestItemTimeline_StructuredEntriesRespectCursor(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)

	item := timelineItemWithStructured(t, srv, ws,
		`[{"id":"note-old","summary":"older","created_at":"2026-04-02T10:00:00Z","created_by":"user"},
		  {"id":"note-new","summary":"newer","created_at":"2026-04-02T12:00:00Z","created_by":"user"}]`, "")

	// Cursor at 11:00 — strictly between the two notes.
	resp := fetchTimeline(t, srv, ws, item.Slug, "before=2026-04-02T11:00:00Z")
	var got []string
	for _, e := range resp.Entries {
		if e.Kind == "note" {
			got = append(got, e.ID)
		}
	}
	if len(got) != 1 || got[0] != "note-old" {
		t.Fatalf("notes past a 11:00 cursor = %v, want [note-old] — an unfiltered "+
			"structured source repeats every entry on every page", got)
	}
}

// The same-instant half of the cursor predicate: at the cursor timestamp, the
// id decides. An entry whose id sorts at or above before_id was already shown
// on the previous page and must not repeat; one below it is still owed.
func TestItemTimeline_StructuredCursorTieBreaksOnID(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)

	item := timelineItemWithStructured(t, srv, ws,
		`[{"id":"note-a","summary":"sorts below the cursor id","created_at":"2026-04-02T10:00:00Z","created_by":"user"},
		  {"id":"note-z","summary":"sorts above the cursor id","created_at":"2026-04-02T10:00:00Z","created_by":"user"}]`, "")

	resp := fetchTimeline(t, srv, ws, item.Slug,
		"before=2026-04-02T10:00:00Z&before_id=note-m")
	var got []string
	for _, e := range resp.Entries {
		if e.Kind == "note" {
			got = append(got, e.ID)
		}
	}
	if len(got) != 1 || got[0] != "note-a" {
		t.Fatalf("same-instant cursor with before_id=note-m returned %v, want [note-a]", got)
	}
}

// Hostile shapes. Both entry structs mark id and created_at omitempty, and the
// fields blob is writable by hand (`pad item update --field`), so absence is
// representable even though today's live data has none of it. A live workspace
// does carry the third case — a double-encoded implementation_notes STRING
// rather than an array (BUG-2627) — which must contribute nothing rather than
// erroring the whole timeline.
func TestItemTimeline_StructuredEntriesTolerateHostileShapes(t *testing.T) {
	t.Run("missing created_at anchors at the item, not 1970", func(t *testing.T) {
		srv := testServer(t)
		ws := createTestWorkspaceViaAPI(t, srv)
		item := timelineItemWithStructured(t, srv, ws,
			`[{"id":"note-undated","summary":"no timestamp","created_by":"user"}]`, "")

		resp := fetchTimeline(t, srv, ws, item.Slug, "")
		var found *models.TimelineEntry
		for i := range resp.Entries {
			if resp.Entries[i].ID == "note-undated" {
				found = &resp.Entries[i]
			}
		}
		if found == nil {
			t.Fatal("undated note dropped from the timeline entirely")
		}
		if found.CreatedAt.Year() < 2000 {
			t.Errorf("undated note anchored at %s — a zero-time fallback renders as 1970 "+
				"and sorts below every real entry", found.CreatedAt)
		}
		if !found.CreatedAt.Equal(item.CreatedAt.UTC()) {
			t.Errorf("undated note anchored at %s, want the item's own created_at %s",
				found.CreatedAt, item.CreatedAt.UTC())
		}
	})

	t.Run("unparseable created_at falls back the same way", func(t *testing.T) {
		srv := testServer(t)
		ws := createTestWorkspaceViaAPI(t, srv)
		item := timelineItemWithStructured(t, srv, ws,
			`[{"id":"note-bad-ts","summary":"garbage timestamp","created_at":"not-a-timestamp","created_by":"user"}]`, "")

		resp := fetchTimeline(t, srv, ws, item.Slug, "")
		var found bool
		for _, e := range resp.Entries {
			if e.ID == "note-bad-ts" {
				found = true
				if e.CreatedAt.Year() < 2000 {
					t.Errorf("note with an unparseable timestamp anchored at %s", e.CreatedAt)
				}
			}
		}
		if !found {
			t.Error("note with an unparseable timestamp dropped from the timeline")
		}
	})

	t.Run("missing id still yields a distinct, stable entry id", func(t *testing.T) {
		srv := testServer(t)
		ws := createTestWorkspaceViaAPI(t, srv)
		item := timelineItemWithStructured(t, srv, ws,
			`[{"summary":"first idless","created_at":"2026-04-02T10:00:00Z","created_by":"user"},
			  {"summary":"second idless","created_at":"2026-04-02T11:00:00Z","created_by":"user"}]`, "")

		resp := fetchTimeline(t, srv, ws, item.Slug, "")
		seen := map[string]bool{}
		notes := 0
		for _, e := range resp.Entries {
			if e.Kind != "note" {
				continue
			}
			notes++
			if e.ID == "" {
				t.Error("id-less note produced a blank timeline entry id — the sort " +
					"tie-breaker and the paging cursor both key on it")
			}
			if seen[e.ID] {
				t.Errorf("two notes collided on timeline entry id %q", e.ID)
			}
			seen[e.ID] = true
		}
		if notes != 2 {
			t.Errorf("expected both id-less notes, got %d", notes)
		}
	})

	t.Run("non-array field contributes nothing and does not break the timeline", func(t *testing.T) {
		srv := testServer(t)
		ws := createTestWorkspaceViaAPI(t, srv)
		// A JSON-encoded string holding the array, i.e. double-encoded —
		// the exact live shape found on one docapp item (BUG-2627).
		doubled, err := json.Marshal(`[{"id":"note-1","summary":"invisible","created_at":"2026-04-02T10:00:00Z"}]`)
		if err != nil {
			t.Fatalf("marshal doubled payload: %v", err)
		}
		item := timelineItemWithStructured(t, srv, ws, string(doubled), "")

		resp := fetchTimeline(t, srv, ws, item.Slug, "")
		if n := kindsOf(resp.Entries)["note"]; n != 0 {
			t.Errorf("double-encoded implementation_notes produced %d note entries, want 0", n)
		}
	})
}

// An item with neither field must produce exactly the timeline it produced
// before this change — the control leg for "the new sources are additive".
func TestItemTimeline_NoStructuredEntriesIsUnchanged(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := timelineItemWithStructured(t, srv, ws, "", "")

	resp := fetchTimeline(t, srv, ws, item.Slug, "")
	counts := kindsOf(resp.Entries)
	if counts["note"] != 0 || counts["decision"] != 0 {
		t.Errorf("item with no structured fields produced %d notes / %d decisions",
			counts["note"], counts["decision"])
	}
	if len(resp.Entries) == 0 {
		t.Error("expected the pre-existing kinds (creation activity/version) to still be there")
	}
}

// walkTimelinePages pages the whole feed one entry at a time, following the
// (created_at, before_id) cursor exactly as the web client does, and returns
// the ids it saw in order.
//
// This is the end-to-end statement about the structured filter: every entry
// must appear EXACTLY once across the full walk. A too-loose predicate repeats
// the in-blob entries on every page; a too-tight one drops them at a boundary.
// Neither is visible from a single-page assertion.
func walkTimelinePages(t *testing.T, srv *Server, wsSlug, itemSlug string) []string {
	t.Helper()
	var seen []string
	query := "limit=1"
	for range 40 { // bounded: the fixtures below are far smaller
		resp := fetchTimeline(t, srv, wsSlug, itemSlug, query)
		if len(resp.Entries) == 0 {
			return seen
		}
		last := resp.Entries[len(resp.Entries)-1]
		for _, e := range resp.Entries {
			seen = append(seen, e.ID)
		}
		if !resp.HasMore {
			return seen
		}
		query = "limit=1&before=" + last.CreatedAt.UTC().Format(time.RFC3339Nano) +
			"&before_id=" + last.ID
	}
	t.Fatal("timeline paging did not terminate within 40 pages — a cursor that " +
		"fails to advance repeats a page forever")
	return nil
}

func assertEachEntryOnce(t *testing.T, seen []string, want ...string) {
	t.Helper()
	counts := map[string]int{}
	for _, id := range seen {
		counts[id]++
	}
	for _, id := range want {
		switch counts[id] {
		case 1: // exactly once, as required
		case 0:
			t.Errorf("%q never appeared across the paged walk (saw %v)", id, seen)
		default:
			t.Errorf("%q appeared %d times across the paged walk (saw %v)", id, counts[id], seen)
		}
	}
}

// The structured entries are filtered in Go against a parsed time, while the
// comment/activity/version sources are filtered in SQL against a FORMATTED
// STRING. That is a real seam between two comparison semantics, and the
// timeline endpoint has a Postgres-specific paging history (BUG-1086), so the
// paging invariant is asserted on both drivers rather than assumed portable.
func TestItemTimeline_StructuredPagingIsExactlyOnce(t *testing.T) {
	run := func(t *testing.T, srv *Server, wsSlug string) {
		t.Helper()
		item := timelineItemWithStructured(t, srv, wsSlug,
			`[{"id":"note-1","summary":"n1","created_at":"2026-04-02T10:00:00Z","created_by":"user"},
			  {"id":"note-2","summary":"n2","created_at":"2026-04-02T12:00:00Z","created_by":"user"}]`,
			`[{"id":"decision-1","decision":"d1","created_at":"2026-04-02T11:00:00Z","created_by":"user"}]`)

		seen := walkTimelinePages(t, srv, wsSlug, item.Slug)
		assertEachEntryOnce(t, seen, "note-1", "note-2", "decision-1")
	}

	t.Run("sqlite", func(t *testing.T) {
		srv := testServer(t)
		run(t, srv, createTestWorkspaceViaAPI(t, srv))
	})

	t.Run("postgres", func(t *testing.T) {
		// Skips unless PAD_TEST_POSTGRES_URL is set (make test-pg).
		srv, _ := testServerPostgres(t)
		// Assert the driver rather than trusting the helper: without this the
		// leg would silently re-run the SQLite case and report a PG pass.
		if got := srv.store.D().Driver(); got != store.DriverPostgres {
			t.Fatalf("expected a Postgres store, got %s", got)
		}
		run(t, srv, createTestWorkspaceViaAPI(t, srv))
	})
}

// Unit-level coverage of the cursor predicate itself, including the nil-item
// guard the handler can't reach but callers could.
func TestStructuredTimelineEntries_CursorPredicate(t *testing.T) {
	at := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return ts
	}

	item := &models.Item{
		CreatedAt: at("2026-04-01T00:00:00Z"),
		ImplementationNotes: []models.ItemImplementationNote{
			{ID: "note-1", Summary: "older", CreatedAt: "2026-04-02T10:00:00Z"},
			{ID: "note-2", Summary: "newer", CreatedAt: "2026-04-02T12:00:00Z"},
		},
		DecisionLog: []models.ItemDecisionLogEntry{
			{ID: "decision-1", Decision: "mid", CreatedAt: "2026-04-02T11:00:00Z"},
		},
	}

	t.Run("first page keeps everything older than now", func(t *testing.T) {
		notes, decisions := structuredTimelineEntries(item, at("2026-05-01T00:00:00Z"), "")
		if len(notes) != 2 || len(decisions) != 1 {
			t.Fatalf("got %d notes / %d decisions, want 2 / 1", len(notes), len(decisions))
		}
	})

	t.Run("cursor drops everything at or newer than it", func(t *testing.T) {
		notes, decisions := structuredTimelineEntries(item, at("2026-04-02T11:00:00Z"), "")
		if len(notes) != 1 || notes[0].ID != "note-1" {
			t.Errorf("notes = %+v, want only note-1", notes)
		}
		if len(decisions) != 0 {
			t.Errorf("decision at exactly the cursor with no before_id should be excluded, got %+v", decisions)
		}
	})

	t.Run("nil item is not a panic", func(t *testing.T) {
		notes, decisions := structuredTimelineEntries(nil, at("2026-05-01T00:00:00Z"), "")
		if notes != nil || decisions != nil {
			t.Errorf("nil item returned %v / %v", notes, decisions)
		}
	})
}
