package server

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2765. The timeline over-fetches per source and drops rows that cannot
// render, so a page can carry fewer entries than the rows it consumed — or
// none — while `has_more` is true. The client derived its cursor from the last
// RENDERED entry, which fails in two ways: with no entries it cannot form one
// at all, and with a fully-dropped later window it re-sends the same one
// forever. Both are fixed by the server returning the position it actually
// reached.
//
// `read` activities are the cheapest row that always drops. limit=1 makes the
// per-source window 3, so three of them fill it.

func seedReadActivities(t *testing.T, srv *Server, itemID string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := srv.store.CreateActivity(models.Activity{
			DocumentID: itemID,
			Action:     "read",
			Actor:      "user",
			Source:     "web",
		}); err != nil {
			t.Fatalf("seed read activity %d: %v", i, err)
		}
	}
}

func seedUpdateActivity(t *testing.T, srv *Server, itemID, changes string) string {
	t.Helper()
	id, err := srv.store.CreateActivity(models.Activity{
		DocumentID: itemID,
		Action:     "updated",
		Actor:      "user",
		Source:     "web",
		Metadata:   `{"changes":"` + changes + `"}`,
	})
	if err != nil {
		t.Fatalf("seed updated activity: %v", err)
	}
	return id
}

// timelinePage fetches one page, by cursor when one is supplied.
func timelinePage(t *testing.T, srv *Server, ws, slug, before, beforeID string) models.TimelineResponse {
	t.Helper()
	q := "limit=1"
	if before != "" {
		q += "&before=" + before + "&before_id=" + beforeID
	}
	return fetchTimeline(t, srv, ws, slug, q)
}

// A window holding only droppable rows must still tell the client where to go.
func TestTimeline_EmptyPageCarriesAUsableCursor(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := timelineItemWithStructured(t, srv, ws, "", "")

	// Strictly newer than the item's own `created` activity: created_at is
	// whole-second text, and rows sharing a second are ordered by id, which
	// is a random UUID. Without the gap the read rows might not be the newest
	// and the window under test would not be the one described.
	time.Sleep(1100 * time.Millisecond)
	seedReadActivities(t, srv, item.ID, 3)

	first := timelinePage(t, srv, ws, item.Slug, "", "")
	if len(first.Entries) != 0 {
		t.Fatalf("expected a fully-dropped first page, got %d entries", len(first.Entries))
	}
	if !first.HasMore {
		t.Fatal("has_more = false, but the item's own created activity is still behind the read rows")
	}
	if first.NextBefore == "" || first.NextBeforeID == "" {
		t.Fatalf("empty page returned no cursor: next_before=%q next_before_id=%q — the client cannot advance",
			first.NextBefore, first.NextBeforeID)
	}

	second := timelinePage(t, srv, ws, item.Slug, first.NextBefore, first.NextBeforeID)
	if len(second.Entries) == 0 {
		t.Fatalf("cursor %s/%s did not reach the renderable history behind the dropped window",
			first.NextBefore, first.NextBeforeID)
	}
}

// The advance failure, which is the instance the filing did not name: a
// fully-dropped window in the MIDDLE of the history. Paging must terminate and
// yield every renderable entry exactly once.
func TestTimeline_PagingTraversesADroppedWindow(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := timelineItemWithStructured(t, srv, ws, "", "")

	oldest := seedUpdateActivity(t, srv, item.ID, "status: open → in-progress")
	time.Sleep(1100 * time.Millisecond)
	seedReadActivities(t, srv, item.ID, 3)
	time.Sleep(1100 * time.Millisecond)
	newest := seedUpdateActivity(t, srv, item.ID, "priority: low → high")

	seen := map[string]int{}
	before, beforeID := "", ""
	// A page can legitimately be empty, so the loop is bounded by requests
	// rather than by pages-with-entries. Six is generous for four rows at
	// limit=1; hitting it means the cursor stopped advancing, which is the
	// wedge itself.
	for i := 0; i < 6; i++ {
		page := timelinePage(t, srv, ws, item.Slug, before, beforeID)
		for _, e := range page.Entries {
			seen[e.ID]++
		}
		if !page.HasMore {
			break
		}
		if page.NextBefore == before && page.NextBeforeID == beforeID {
			t.Fatalf("cursor did not advance past %s/%s — paging is wedged", before, beforeID)
		}
		before, beforeID = page.NextBefore, page.NextBeforeID
		if i == 5 {
			t.Fatal("paging did not terminate within 6 requests")
		}
	}

	for _, tc := range []struct {
		name, id string
	}{
		{"newest renderable", newest},
		{"oldest renderable", oldest},
	} {
		switch seen[tc.id] {
		case 1:
			// exactly once, as required
		case 0:
			t.Errorf("%s activity %s was never returned — paging stepped over it", tc.name, tc.id)
		default:
			t.Errorf("%s activity %s returned %d times", tc.name, tc.id, seen[tc.id])
		}
	}
}

// Control: when nothing is dropped, the cursor is the last RENDERED entry.
// Without this leg, "always resume from the oldest row the window touched"
// passes both tests above while silently skipping the entries a truncated page
// cut off.
func TestTimeline_TruncatedPageResumesAtItsLastEntry(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := timelineItemWithStructured(t, srv, ws, "", "")

	time.Sleep(1100 * time.Millisecond)
	seedUpdateActivity(t, srv, item.ID, "status: open → in-progress")
	seedUpdateActivity(t, srv, item.ID, "priority: low → high")

	page := timelinePage(t, srv, ws, item.Slug, "", "")
	if len(page.Entries) != 1 || !page.HasMore {
		t.Fatalf("want a truncated 1-entry page with more behind it, got %d entries has_more=%v",
			len(page.Entries), page.HasMore)
	}
	last := page.Entries[len(page.Entries)-1]
	if page.NextBeforeID != last.ID {
		t.Errorf("next_before_id = %q, want the last rendered entry %q — resuming anywhere older drops what truncation cut",
			page.NextBeforeID, last.ID)
	}
	if got, want := page.NextBefore, last.CreatedAt.UTC().Format(time.RFC3339Nano); got != want {
		t.Errorf("next_before = %q, want %q", got, want)
	}
}

// The tail-selection rule, pinned at the unit level because the handler cannot
// cheaply reach the case: a source that FILLS its window and whose rows RENDER
// puts perSource = 3×limit entries on the page, which takes the truncation
// branch instead. Two full sources therefore only meet here when both are
// dominated by droppable rows, and comments — the natural second source — never
// drop. The handler tests above cover the single-full-source path end to end;
// this covers the choice between two.
//
// The choice is not symmetric. Resuming at the NEWEST full tail re-examines
// rows the older-tailed source already rendered, which the client's dedup
// absorbs. Resuming at the oldest steps OVER the newer source's unexamined
// rows, and nothing ever comes back for them.
func TestExhaustedWindowCursor_PicksTheNewestFullTail(t *testing.T) {
	t.Parallel()
	at := func(sec int) time.Time { return time.Date(2026, 8, 25, 12, 0, sec, 0, time.UTC) }
	full := func(n int, tailAt time.Time, tailID string) []models.Comment {
		out := make([]models.Comment, n)
		out[n-1] = models.Comment{ID: tailID, CreatedAt: tailAt}
		return out
	}
	acts := func(n int, tailAt time.Time, tailID string) []models.Activity {
		out := make([]models.Activity, n)
		out[n-1] = models.Activity{ID: tailID, CreatedAt: tailAt}
		return out
	}
	vers := func(n int, tailAt time.Time, tailID string) []models.Version {
		out := make([]models.Version, n)
		out[n-1] = models.Version{ID: tailID, CreatedAt: tailAt}
		return out
	}

	const perSource = 3

	for _, tc := range []struct {
		name       string
		comments   []models.Comment
		activities []models.Activity
		versions   []models.Version
		wantID     string
		wantOK     bool
	}{
		{
			name:       "two full sources — the newer tail wins",
			comments:   full(3, at(10), "c-old"),
			activities: acts(3, at(20), "a-new"),
			wantID:     "a-new", wantOK: true,
		},
		{
			name:       "order of the arguments does not decide it",
			comments:   full(3, at(30), "c-new"),
			activities: acts(3, at(20), "a-old"),
			wantID:     "c-new", wantOK: true,
		},
		{
			// The whole point of the `full` flag: a short source has nothing
			// older to come back for, so its newer tail must not drag the
			// cursor forward past the full source's unexamined rows.
			name:       "a short source does not constrain the cursor",
			comments:   full(1, at(40), "c-short"),
			activities: acts(3, at(20), "a-full"),
			wantID:     "a-full", wantOK: true,
		},
		{
			name:       "same instant — the id breaks the tie the way the query orders it",
			comments:   full(3, at(20), "b-comment"),
			activities: acts(3, at(20), "a-activity"),
			wantID:     "b-comment", wantOK: true,
		},
		{
			name:       "three sources, versions newest",
			comments:   full(3, at(10), "c"),
			activities: acts(3, at(20), "a"),
			versions:   vers(3, at(30), "v"),
			wantID:     "v", wantOK: true,
		},
		{
			name:       "nothing full — no cursor, and no has_more either",
			comments:   full(1, at(10), "c"),
			activities: acts(2, at(20), "a"),
			wantOK:     false,
		},
		{
			name:   "no rows at all",
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, id, ok := exhaustedWindowCursor(tc.comments, tc.activities, tc.versions, perSource)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && id != tc.wantID {
				t.Errorf("cursor id = %q, want %q", id, tc.wantID)
			}
		})
	}
}

// Codex round 1: truncation and an exhausted window are INDEPENDENT reasons to
// bound the cursor, and the second is not implied by the first. A source whose
// rows all drop contributes nothing to the page, so the truncation cursor can
// sit older than that source's tail — and every unexamined row between the two
// falls in a gap neither page fetches.
//
// The fixture puts one renderable activity in exactly that gap:
//
//	t0  item created            (activity, renderable, oldest)
//	t1  two comments            (render; two entries at limit=1 forces truncation)
//	t2  one `updated` activity  (renderable — the row at risk)
//	t3  three `read` activities (fill the activity window, all dropped)
//
// The activity window is the three reads, so t2 is never examined; the
// truncation cursor is a comment at t1, older than the reads' tail at t3.
// Resuming there steps straight over t2.
func TestTimeline_TruncationDoesNotStepOverAnExhaustedWindow(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := timelineItemWithStructured(t, srv, ws, "", "")

	time.Sleep(1100 * time.Millisecond)
	for i, body := range []string{"first", "second"} {
		if _, err := srv.store.CreateComment(item.WorkspaceID, item.ID, "", models.CommentCreate{
			Body: body, Author: "tester", CreatedBy: "user", Source: "web",
		}); err != nil {
			t.Fatalf("seed comment %d: %v", i, err)
		}
	}

	time.Sleep(1100 * time.Millisecond)
	atRisk := seedUpdateActivity(t, srv, item.ID, "status: open → in-progress")

	time.Sleep(1100 * time.Millisecond)
	seedReadActivities(t, srv, item.ID, 3)

	seen := map[string]int{}
	before, beforeID := "", ""
	for i := 0; ; i++ {
		if i == 8 {
			t.Fatal("paging did not terminate within 8 requests")
		}
		page := timelinePage(t, srv, ws, item.Slug, before, beforeID)
		for _, e := range page.Entries {
			seen[e.ID]++
		}
		if !page.HasMore {
			break
		}
		if page.NextBefore == before && page.NextBeforeID == beforeID {
			t.Fatalf("cursor did not advance past %s/%s", before, beforeID)
		}
		before, beforeID = page.NextBefore, page.NextBeforeID
	}

	if seen[atRisk] != 1 {
		t.Errorf("the activity between the truncation point and the dropped window was returned %d times, want 1 — paging stepped over it",
			seen[atRisk])
	}
}
