package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2783: a structured entry's id can equal the id of a comment, activity or
// version on the SAME item, and then two entries in one timeline payload share
// an id — the failure the intra-structured dedupe already prevents, one
// source-boundary out.
//
// The client breaks two different ways, and they are worth separating because
// the first draft of this comment collapsed them into "one of the two is
// hidden", which is only half true:
//
//   - WITHIN one page: ItemTimeline.svelte renders
//     `{#each visibleEntries as entry (entry.id)}`. A keyed each with a
//     duplicate key is a Svelte ERROR, not a silent drop.
//   - ACROSS pages: the append path filters on `existingIds`, so the later
//     copy is silently discarded — that is where something is hidden.
//
// Note and decision ids come from the item's fields blob and nothing validates
// them on write: `pad item note` generates `note-<nanos>`, but an imported
// artifact or a hand-edited blob may carry any string, including a UUID that
// belongs to a comment on the same item.
//
// These tests drive the MERGED payload through the real endpoint, not
// structuredTimelineEntries directly. That is deliberate and is the reason the
// gap survived this long: the structured builder is CORRECT in isolation — it
// dedupes perfectly against the other structured entries — and a unit test of
// it vouches for the component rather than its binding to the three
// SQL-backed sources (team CONVE-19).

// patchItemFields overwrites the item's fields blob through the real PATCH
// path, so the entries are hydrated exactly as production hydrates them.
func patchItemFields(t *testing.T, srv *Server, wsSlug, itemSlug, fieldsJSON string) {
	t.Helper()
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+wsSlug+"/items/"+itemSlug, map[string]any{
		"fields": fieldsJSON,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch item fields = %d: %s", rr.Code, rr.Body.String())
	}
}

func entriesByKind(entries []models.TimelineEntry, kind string) []models.TimelineEntry {
	var out []models.TimelineEntry
	for _, e := range entries {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// TestTimeline_StructuredIDCollidingWithACommentHidesNothing is the regression
// test. Against unfixed code the note is emitted carrying the comment's id and
// the two entries collide.
//
// The assertion is deliberately specific about WHICH entry moves. Asserting
// only "two entries are present" would pass trivially — they are both in the
// payload even unfixed, since the collision is resolved by the CLIENT, not the
// server. Asserting only "the ids differ" would pass if the comment had been
// the one to yield, which would be a different bug: comment ids are real
// primary keys AND are what the client sends back as `before_id`, so moving
// one breaks paging. The unvalidated side is the side that must move.
func TestTimeline_StructuredIDCollidingWithACommentHidesNothing(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	item := timelineItemWithStructured(t, srv, ws, "", "")

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/"+item.Slug+"/comments",
		map[string]any{"body": "a comment whose id is about to be stolen"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create comment = %d: %s", rr.Code, rr.Body.String())
	}
	var comment models.Comment
	parseJSON(t, rr, &comment)
	if comment.ID == "" {
		t.Fatal("fixture: comment came back with no id")
	}

	// The collision: a note claiming the comment's id.
	notes, err := json.Marshal([]map[string]string{{
		"id":         comment.ID,
		"summary":    "note that stole the comment's id",
		"created_at": comment.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}})
	if err != nil {
		t.Fatalf("marshal notes: %v", err)
	}
	patchItemFields(t, srv, ws, item.Slug, `{"status":"open","implementation_notes":`+string(notes)+`}`)

	resp := fetchTimeline(t, srv, ws, item.Slug, "")

	comments := entriesByKind(resp.Entries, "comment")
	noteEntries := entriesByKind(resp.Entries, "note")
	if len(comments) != 1 || len(noteEntries) != 1 {
		t.Fatalf("fixture: expected exactly 1 comment and 1 note entry, got %d and %d (kinds: %v)",
			len(comments), len(noteEntries), kindsOf(resp.Entries))
	}

	// The comment keeps its own id — it is a primary key and a paging cursor.
	if comments[0].ID != comment.ID {
		t.Errorf("the comment entry must keep its own id; got %q, want %q", comments[0].ID, comment.ID)
	}
	// The note is the one that yields.
	if noteEntries[0].ID == comment.ID {
		t.Errorf("the note entry kept the colliding id %q; two entries in one payload share an id "+
			"and the client will hide one of them", noteEntries[0].ID)
	}

	// And no id is repeated anywhere in the page, which is the property the
	// client actually depends on.
	seen := map[string]int{}
	for _, e := range resp.Entries {
		seen[e.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("entry id %q appears %d times in one page", id, n)
		}
	}
}

// TestTimeline_StructuredIDCollidingWithAnActivityHidesNothing covers the
// second source. Activities are generated server-side for the item's own
// lifecycle, so a blob id can collide with one without anybody hand-editing
// anything exotic.
func TestTimeline_StructuredIDCollidingWithAnActivityHidesNothing(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	item := timelineItemWithStructured(t, srv, ws, "", "")

	// The create already produced an activity row; read its id back off the
	// timeline rather than guessing at one.
	before := fetchTimeline(t, srv, ws, item.Slug, "")
	acts := entriesByKind(before.Entries, "activity")
	if len(acts) == 0 {
		// NOT t.Skip. Creating an item writes an activity row, so an empty
		// list means the fixture stopped building what this test needs — and
		// a skip would report that as success forever.
		t.Fatalf("fixture: a freshly created item must have an activity entry to collide with; kinds: %v",
			kindsOf(before.Entries))
	}
	victim := acts[0].ID

	decisions, err := json.Marshal([]map[string]string{{
		"id":         victim,
		"decision":   "decision that stole an activity id",
		"created_at": acts[0].CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}})
	if err != nil {
		t.Fatalf("marshal decisions: %v", err)
	}
	patchItemFields(t, srv, ws, item.Slug, `{"status":"open","decision_log":`+string(decisions)+`}`)

	resp := fetchTimeline(t, srv, ws, item.Slug, "")

	decisionEntries := entriesByKind(resp.Entries, "decision")
	if len(decisionEntries) != 1 {
		t.Fatalf("expected exactly 1 decision entry, got %d (kinds: %v)",
			len(decisionEntries), kindsOf(resp.Entries))
	}
	if decisionEntries[0].ID == victim {
		t.Errorf("the decision entry kept the colliding activity id %q", decisionEntries[0].ID)
	}

	// The activity must still be present under its own id: the structured
	// entry yielding is only correct if the row it collided with survives.
	stillThere := false
	for _, e := range entriesByKind(resp.Entries, "activity") {
		if e.ID == victim {
			stillThere = true
		}
	}
	if !stillThere {
		t.Errorf("the activity that owned id %q is no longer in the payload", victim)
	}

	seen := map[string]int{}
	for _, e := range resp.Entries {
		seen[e.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("entry id %q appears %d times in one page", id, n)
		}
	}
}

// TestTimeline_StructuredIDEqualToAnotherEntrysFallback covers the OTHER half
// of the uniqueness rule: a raw id that collides not with another raw id but
// with the positional fallback a sibling was already given.
//
// The ORDER matters and my first fixture had it wrong, which is worth
// recording: a note with no id followed by a note whose blob id is
// "note-idx-0" does NOT reach the loop — the second note's raw id is already
// in usedIDs, so the `!usedIDs[raw]` guard sends it to the fallback branch and
// it gets `note-idx-1`, which is free. That fixture passed with the loop
// deleted, i.e. it proved nothing about the thing it was named for.
//
// To reach the loop the fallback NAME must already be taken when the fallback
// is computed: note 0 claims the literal string "note-idx-1", and note 1 has
// no id, so its positional fallback is `note-idx-1` — occupied. Only then does
// `for usedIDs[id] { id += "x" }` run.
//
// This is exotic but not unreachable: unlike the cross-source case, both ids
// here come from the same unvalidated fields blob, so a hand-edited or
// imported item can hold exactly this shape. (The cross-source fallback
// collision, by contrast, cannot happen today — every SQL source mints
// uuid.New() ids, including on import, and a UUID never equals `note-idx-N`.)
func TestTimeline_StructuredIDEqualToAnotherEntrysFallback(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	notes := `[{"id":"note-idx-1","summary":"claims a LATER entry's fallback name","created_at":"2026-04-02T10:00:00Z"},
	           {"summary":"no id, so its fallback is note-idx-1 — already taken","created_at":"2026-04-02T11:00:00Z"}]`
	item := timelineItemWithStructured(t, srv, ws, notes, "")

	resp := fetchTimeline(t, srv, ws, item.Slug, "")

	noteEntries := entriesByKind(resp.Entries, "note")
	if len(noteEntries) != 2 {
		t.Fatalf("expected both notes in the payload, got %d (kinds: %v)",
			len(noteEntries), kindsOf(resp.Entries))
	}
	if noteEntries[0].ID == noteEntries[1].ID {
		t.Errorf("both notes were emitted with id %q; one of them will be hidden by the client",
			noteEntries[0].ID)
	}

	seen := map[string]int{}
	for _, e := range resp.Entries {
		seen[e.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("entry id %q appears %d times in one page", id, n)
		}
	}
}

// TestTimeline_StructuredIDCollidingWithAVersionHidesNothing covers the third
// SQL source. Without it, a fix that only knew about comments and activities
// would pass the rest of this file — which is exactly what the first version
// of the fix did, since it enumerated sources by hand.
func TestTimeline_StructuredIDCollidingWithAVersionHidesNothing(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	item := timelineItemWithStructured(t, srv, ws, "", "")

	// An update creates a version row.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, map[string]any{
		"content": "a revision, so a version row exists",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update item = %d: %s", rr.Code, rr.Body.String())
	}

	before := fetchTimeline(t, srv, ws, item.Slug, "")
	versions := entriesByKind(before.Entries, "version")
	if len(versions) == 0 {
		t.Fatalf("fixture: an updated item must have a version entry to collide with; kinds: %v",
			kindsOf(before.Entries))
	}
	victim := versions[0].ID

	notes, err := json.Marshal([]map[string]string{{
		"id":         victim,
		"summary":    "note that stole a version id",
		"created_at": versions[0].CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}})
	if err != nil {
		t.Fatalf("marshal notes: %v", err)
	}
	patchItemFields(t, srv, ws, item.Slug, `{"status":"open","implementation_notes":`+string(notes)+`}`)

	resp := fetchTimeline(t, srv, ws, item.Slug, "")

	noteEntries := entriesByKind(resp.Entries, "note")
	if len(noteEntries) != 1 {
		t.Fatalf("expected exactly 1 note entry, got %d (kinds: %v)", len(noteEntries), kindsOf(resp.Entries))
	}
	if noteEntries[0].ID == victim {
		t.Errorf("the note entry kept the colliding version id %q", noteEntries[0].ID)
	}

	stillThere := false
	for _, e := range entriesByKind(resp.Entries, "version") {
		if e.ID == victim {
			stillThere = true
		}
	}
	if !stillThere {
		t.Errorf("the version that owned id %q is no longer in the payload", victim)
	}

	seen := map[string]int{}
	for _, e := range resp.Entries {
		seen[e.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("entry id %q appears %d times in one page", id, n)
		}
	}
}

// TestTimeline_StructuredIDIsIndependentOfThePageWindow is the regression test
// for the defect the FIRST version of this fix introduced, and the reason the
// fix is a shape test rather than a lookup.
//
// That version seeded the dedupe map from the ids fetched for the current
// page. The three SQL windows depend on the cursor, so a structured entry took
// its raw id on a page where the colliding row was absent and a positional id
// on one where it was present. That id is not merely a render key — it is the
// second term of the cursor predicate, so a window-dependent id makes an
// entry's own sort position depend on which page is being built.
//
// The assertion is that the same entry carries the SAME id on a first page and
// on a narrow page fetched with a cursor, which is a property of the item
// alone and cannot hold if the id consults the window.
func TestTimeline_StructuredIDIsIndependentOfThePageWindow(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	item := timelineItemWithStructured(t, srv, ws, "", "")

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/"+item.Slug+"/comments",
		map[string]any{"body": "the row whose id the note claims"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create comment = %d: %s", rr.Code, rr.Body.String())
	}
	var comment models.Comment
	parseJSON(t, rr, &comment)

	notes, err := json.Marshal([]map[string]string{{
		"id":         comment.ID,
		"summary":    "note claiming a row id",
		"created_at": item.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}})
	if err != nil {
		t.Fatalf("marshal notes: %v", err)
	}
	patchItemFields(t, srv, ws, item.Slug, `{"status":"open","implementation_notes":`+string(notes)+`}`)

	full := fetchTimeline(t, srv, ws, item.Slug, "")
	fullNotes := entriesByKind(full.Entries, "note")
	if len(fullNotes) != 1 {
		t.Fatalf("expected 1 note on the full page, got %d", len(fullNotes))
	}

	// A page whose window is narrow enough to exclude the colliding comment.
	narrow := fetchTimeline(t, srv, ws, item.Slug, "limit=1")
	narrowNotes := entriesByKind(narrow.Entries, "note")
	if len(narrowNotes) == 0 {
		// The note may legitimately fall outside a 1-entry page; page from the
		// cursor until it appears rather than asserting on an empty result.
		if !narrow.HasMore || narrow.NextBefore == "" {
			t.Fatalf("narrow page carried no note and no cursor to continue from; kinds: %v",
				kindsOf(narrow.Entries))
		}
		next := fetchTimeline(t, srv, ws, item.Slug,
			"before="+narrow.NextBefore+"&before_id="+narrow.NextBeforeID+"&limit=5")
		narrowNotes = entriesByKind(next.Entries, "note")
	}
	if len(narrowNotes) == 0 {
		// Not a skip: the note is older than the comment and the paged fetch
		// covers it, so its absence means the fixture stopped exercising the
		// window this test is named for.
		t.Fatalf("note did not surface on a paged fetch, so the window comparison never ran")
	}

	if narrowNotes[0].ID != fullNotes[0].ID {
		t.Errorf("the same note carried id %q on the full page and %q on a paged fetch; "+
			"a structured entry's id must be a function of the item, not of the page window",
			fullNotes[0].ID, narrowNotes[0].ID)
	}
}

// TestTimeline_DivertedIDIsStableAcrossBlobMutations pins the property the
// derived id exists for.
//
// A UUID-shaped raw id cannot be emitted as-is (it could equal a row id), but
// it must not be pushed onto the POSITIONAL fallback either: that fallback
// encodes the array index, so inserting an entry ahead of it renumbers it, and
// the entry id is the cursor's tie-breaker. An entry whose id changes under an
// unrelated insert can be skipped or re-shown across a page boundary.
//
// The assertion is that the same entry keeps the same id after another entry
// is inserted BEFORE it — which holds for a derived id and cannot hold for a
// positional one.
func TestTimeline_DivertedIDIsStableAcrossBlobMutations(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	// A note whose id is UUID-shaped, so it takes the diverted path.
	subject := "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	one := `[{"id":"` + subject + `","summary":"the subject","created_at":"2026-04-02T11:00:00Z"}]`
	item := timelineItemWithStructured(t, srv, ws, one, "")

	first := fetchTimeline(t, srv, ws, item.Slug, "")
	notes := entriesByKind(first.Entries, "note")
	if len(notes) != 1 {
		t.Fatalf("expected 1 note, got %d (kinds: %v)", len(notes), kindsOf(first.Entries))
	}
	before := notes[0].ID
	if before == subject {
		t.Fatalf("a UUID-shaped raw id must not be emitted as-is; got %q", before)
	}

	// Insert another note AHEAD of it. Under a positional id this renumbers
	// the subject from index 0 to index 1.
	two := `[{"summary":"inserted ahead, no id","created_at":"2026-04-02T10:00:00Z"},
	         {"id":"` + subject + `","summary":"the subject","created_at":"2026-04-02T11:00:00Z"}]`
	patchItemFields(t, srv, ws, item.Slug, `{"status":"open","implementation_notes":`+two+`}`)

	second := fetchTimeline(t, srv, ws, item.Slug, "")
	var after string
	for _, e := range entriesByKind(second.Entries, "note") {
		if e.Note != nil && e.Note.Summary == "the subject" {
			after = e.ID
		}
	}
	if after == "" {
		t.Fatalf("the subject note is missing after the insert; kinds: %v", kindsOf(second.Entries))
	}
	if after != before {
		t.Errorf("the subject note's id changed from %q to %q when an unrelated entry was inserted "+
			"ahead of it; the id is the cursor's tie-breaker, so it must not depend on array position",
			before, after)
	}
}
