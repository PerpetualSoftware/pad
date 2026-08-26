package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2783: a structured entry's id can equal the id of a comment, activity or
// version on the SAME item, and then two entries in one timeline payload share
// an id. The client keys its {#each} on entry id and dedupes appended pages by
// id, so one of the two is hidden — the same failure the intra-structured
// dedupe already prevents, one source-boundary out.
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
// it vouches for the component rather than its binding to the other four
// sources (team CONVE-19).

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
		t.Skip("no activity entry on a freshly created item; nothing to collide with")
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
