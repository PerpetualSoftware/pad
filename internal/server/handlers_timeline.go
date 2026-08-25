package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// handleListItemTimeline returns a unified, chronological timeline for an item,
// newest first, with cursor-based pagination.
//
// Request: `limit=N` (default 50) and the cursor pair `before=<RFC3339>` +
// `before_id=<id>`, which select entries strictly older than that position —
// created_at first, id as the tie-break, because several entries can share a
// whole second.
//
// Response: when `has_more` is true the body also carries `next_before` and
// `next_before_id`, and a client MUST page with those two rather than deriving
// a cursor from the last entry it received. They are not the same value. This
// handler over-fetches per source and drops rows that cannot render, so a page
// can carry fewer entries than the rows it consumed — or none, with history
// still behind them — and it can deliberately resume at a position it has
// already covered when one source's window ran out before another's. A
// consumer that pages from its last visible entry therefore either cannot form
// a cursor at all or re-requests the same window forever (BUG-2765), and one
// that forwards only `next_before` without `next_before_id` can repeat or skip
// entries sharing that second. Both fields, or neither.
func (s *Server) handleListItemTimeline(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItemIncludeDeleted(workspaceID, itemSlug)
	if err != nil || item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	// Cursor: fetch entries before this (timestamp, id) pair.
	//
	// Three cases:
	//   1. Neither before nor before_id supplied (true first page) →
	//      beforeID = "" tells the store to drop the id predicate entirely.
	//   2. Both before and before_id supplied (normal cursor pagination) →
	//      passed straight through.
	//   3. before supplied without before_id (malformed/external client) →
	//      use a UUID-safe upper-bound sentinel ("g" sorts after any
	//      lowercase-hex UUID character in every reasonable collation) so
	//      same-second entries aren't silently dropped.
	//
	// Case 3 carries an ASSUMPTION, not a guarantee: "g" keeps same-second
	// entries only for ids drawn from the lowercase-hex UUID alphabet. Any
	// source whose ids can sort ABOVE "g" is silently dropped at the cursor
	// instant instead — and a source with ids on BOTH sides is split in half
	// on their first character. That is not hypothetical: the structured
	// kinds' `note-…` / `decision-…` ids straddle it exactly that way
	// (BUG-2301), which is what `sentinelBeforeID` below exists to handle.
	// If you add a source whose ids are not UUIDs, this is the line to check.
	//
	// The previous code defaulted beforeID to "\xff" in all three cases.
	// That worked on SQLite but Postgres rejects "\xff" as an invalid UTF-8
	// byte sequence (SQLSTATE 22021), causing every timeline load to 500.
	// See BUG-1086.
	before := time.Now().UTC().Add(time.Minute)
	beforeID := ""
	hasBefore := false
	// Whether beforeID below is the synthetic sentinel rather than a real id
	// the client sent. The structured sources need to know, because the
	// sentinel encodes "keep every entry at this instant" and only does so by
	// accident for ids drawn from the lowercase-hex UUID alphabet.
	sentinelBeforeID := false
	if v := r.URL.Query().Get("before"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			before = t
			hasBefore = true
		} else if t, err := time.Parse(time.RFC3339, v); err == nil {
			before = t
			hasBefore = true
		}
	}
	if v := r.URL.Query().Get("before_id"); v != "" {
		beforeID = v
	}
	if hasBefore && beforeID == "" {
		// > any UUID character lex-wise; valid UTF-8. Non-UUID ids are NOT
		// covered by that — see the assumption note above.
		beforeID = "g"
		sentinelBeforeID = true
	}

	// Over-fetch per source (3x limit) to compensate for entries removed by
	// buildTimeline's dedup/filtering (empty-metadata updates, read actions, etc.).
	perSource := limit * 3

	comments, err := s.store.ListCommentsBeforeTime(item.ID, before, beforeID, perSource)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Bulk-load reactions for fetched comments.
	if len(comments) > 0 {
		commentIDs := make([]string, len(comments))
		for i, c := range comments {
			commentIDs[i] = c.ID
		}
		reactionsMap, rerr := s.store.ListReactionsByComments(commentIDs)
		if rerr == nil && reactionsMap != nil {
			for i := range comments {
				if reactions, ok := reactionsMap[comments[i].ID]; ok {
					comments[i].Reactions = reactions
				}
			}
		}
	}

	activities, err := s.store.ListDocumentActivityBeforeTime(item.ID, before, beforeID, perSource)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	versions, err := s.store.ListItemVersionsBeforeTime(item.ID, before, beforeID, perSource)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Implementation notes and decision-log entries live inside the item's
	// own fields blob (hydrated by the store on resolve), not in a table, so
	// there is no cursor query to run — every entry is already in hand. They
	// still have to be filtered through the SAME (created_at, id) predicate
	// the SQL above uses, or paging would show them on every page instead of
	// exactly one (BUG-2301).
	notes, decisions := structuredTimelineEntries(item, before, beforeID, sentinelBeforeID)

	entries := buildTimeline(comments, activities, versions, notes, decisions)

	// Determine if there are more entries beyond this page, and where the
	// next one starts. The cursor is the server's to compute: it over-fetched
	// per source and dropped rows above, so the client cannot see the
	// position it must continue from (BUG-2765).
	// Two independent reasons the next page must not start further back than
	// a given position, and the cursor is the NEWEST of them:
	//
	//   - truncation: entries past `limit` were rendered and then cut, so they
	//     have to be re-delivered — the cursor cannot be older than the last
	//     entry KEPT.
	//   - an exhausted window: a source that filled its over-fetch has rows
	//     older than its tail that were never examined — the cursor cannot be
	//     older than that tail, or those rows fall between the two pages and
	//     are never fetched by either (codex round 1).
	//
	// Both can hold at once, and the second is not implied by the first: a
	// source whose rows all DROP contributes nothing to `entries`, so the
	// truncation cursor can easily sit older than its tail. Taking the newest
	// candidate re-covers ground the client dedups away; taking anything
	// older loses rows permanently.
	cursorAt, cursorID, hasMore := exhaustedWindowCursor(comments, activities, versions, perSource)
	if len(entries) > limit {
		entries = entries[:limit]
		hasMore = true
		last := entries[len(entries)-1]
		if cursorID == "" || last.CreatedAt.After(cursorAt) ||
			(last.CreatedAt.Equal(cursorAt) && last.ID > cursorID) {
			cursorAt, cursorID = last.CreatedAt, last.ID
		}
	}

	resp := models.TimelineResponse{
		Entries: entries,
		HasMore: hasMore,
	}
	if hasMore {
		resp.NextBefore = cursorAt.UTC().Format(time.RFC3339Nano)
		resp.NextBeforeID = cursorID
	}
	writeJSON(w, http.StatusOK, resp)
}

// structuredTimelineEntries turns the item's implementation notes and
// decision-log entries into timeline entries, applying the same cursor
// predicate the SQL sources use: keep an entry strictly older than the
// cursor, or at the cursor instant with a lower id.
//
// The two structured kinds are stored as JSON inside items.fields rather than
// as rows, which makes three things representable that a table would not:
//
//   - a missing created_at (both structs mark it omitempty). Such an entry has
//     no place of its own in a chronological feed, so it is anchored at the
//     item's creation instant — the earliest moment it could have existed.
//     Anchoring at Go's zero time instead would date the entry to year 1 and
//     sort it below everything real.
//   - a missing id. The id is the sort tie-breaker and the cursor's second
//     term, so a blank one gets a positional fallback, keeping the ordering
//     total and paging stable.
//   - a field that is not an array at all. models.ExtractItem* returns nil for
//     those, so they arrive here as an empty slice and simply contribute
//     nothing. See BUG-2627 for a live instance.
//   - duplicate ids. Nothing validates them on write, and a duplicate id is
//     not merely untidy: it collides in the client's keyed {#each}, and the
//     cursor cannot page past a pair of entries it cannot tell apart. Repeats
//     get the same positional suffix an absent id gets.
//
// `sentinelBeforeID` says the caller synthesized beforeID rather than
// receiving it, which means "keep every entry at this instant" — see the
// comparison note inside.
func structuredTimelineEntries(item *models.Item, before time.Time, beforeID string, sentinelBeforeID bool) ([]models.TimelineEntry, []models.TimelineEntry) {
	if item == nil {
		return nil, nil
	}

	// Compare in the SAME space the SQL sources do: RFC3339 text, whole
	// seconds. The store formats the cursor with time.Format(time.RFC3339)
	// and compares it against the stored text column, so a Go-side comparison
	// on full-precision time.Time is a DIFFERENT predicate — a structured
	// entry carrying sub-second precision (a hand-written created_at, or the
	// item's own createdAt used as the undated fallback) could then sit on a
	// page boundary that the SQL sources resolve the other way, dropping or
	// repeating entries around it. Formatting both sides removes the seam
	// instead of trying to compensate for it.
	beforeText := before.Format(time.RFC3339)

	keep := func(at time.Time, id string) bool {
		atText := at.Format(time.RFC3339)
		if atText < beforeText {
			return true
		}
		if atText != beforeText {
			return false
		}
		// At the cursor instant the id decides. The sentinel is not a real
		// id: it exists so same-second entries are NOT dropped, and it
		// achieves that for UUIDs only because every lowercase-hex character
		// sorts below "g". Structured ids are `note-…` / `decision-…`, so
		// comparing against it would drop every note ("n" > "g") while
		// keeping every decision ("d" < "g") — an arbitrary split. Honour
		// what the sentinel MEANS rather than how it happens to sort.
		if sentinelBeforeID {
			return true
		}
		return beforeID != "" && id < beforeID
	}

	// Assign a stable, unique entry id: the entry's own id when it has one
	// and no earlier entry claimed it, otherwise a positional fallback.
	// Notes and decisions share this map — they land in one merged stream and
	// a collision ACROSS the two kinds breaks the client exactly as one
	// within a kind does.
	usedIDs := make(map[string]bool, len(item.ImplementationNotes)+len(item.DecisionLog))
	entryID := func(raw, prefix string, i int) string {
		if raw != "" && !usedIDs[raw] {
			usedIDs[raw] = true
			return raw
		}
		id := fmt.Sprintf("%s-idx-%d", prefix, i)
		for usedIDs[id] {
			id += "x"
		}
		usedIDs[id] = true
		return id
	}

	// Parse an entry timestamp, falling back to the item's own creation
	// instant when it is absent or malformed, and TRUNCATE to whole seconds.
	//
	// Truncation is not cosmetic. The SQL sources store and compare
	// second-precision RFC3339 text, so a structured entry that kept
	// sub-second precision would be in a different space than every other
	// entry in the merged stream, in three places at once: the sort would
	// interleave it against same-second rows by a component they do not have;
	// the client echoes the last entry's created_at back as the next cursor,
	// where the store formats it down to the second and would then EXCLUDE
	// same-second rows that were still owed; and the filter's own comparison
	// would disagree with the SQL one. Landing the entry in the shared space
	// at the point it is built fixes all three at once — filtering in the
	// formatted space alone (which is what the first pass at this did) leaves
	// the sort and the emitted cursor wrong.
	stamp := func(raw string) time.Time {
		parsed := item.CreatedAt
		if raw != "" {
			if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				parsed = t
			} else if t, err := time.Parse(time.RFC3339, raw); err == nil {
				parsed = t
			}
		}
		return parsed.UTC().Truncate(time.Second)
	}

	var notes []models.TimelineEntry
	for i := range item.ImplementationNotes {
		n := item.ImplementationNotes[i]
		id := entryID(n.ID, "note", i)
		at := stamp(n.CreatedAt)
		if !keep(at, id) {
			continue
		}
		notes = append(notes, models.TimelineEntry{
			ID:        id,
			Kind:      "note",
			CreatedAt: at,
			Actor:     n.CreatedBy,
			Source:    "structured",
			Note:      &item.ImplementationNotes[i],
		})
	}

	var decisions []models.TimelineEntry
	for i := range item.DecisionLog {
		d := item.DecisionLog[i]
		id := entryID(d.ID, "decision", i)
		at := stamp(d.CreatedAt)
		if !keep(at, id) {
			continue
		}
		decisions = append(decisions, models.TimelineEntry{
			ID:        id,
			Kind:      "decision",
			CreatedAt: at,
			Actor:     d.CreatedBy,
			Source:    "structured",
			Decision:  &item.DecisionLog[i],
		})
	}

	return notes, decisions
}

// buildTimeline merges comments, activities, versions, implementation notes,
// and decision-log entries into a single chronological stream, applying
// deduplication and collapsing logic.
func buildTimeline(comments []models.Comment, activities []models.Activity, versions []models.Version, notes, decisions []models.TimelineEntry) []models.TimelineEntry {
	// Build a set of version timestamps (rounded to the second) for dedup.
	versionTimes := make(map[int64]bool, len(versions))
	for _, v := range versions {
		versionTimes[v.CreatedAt.Unix()] = true
	}

	// Activities the fetched comments link to, skipped below. This is NOT the
	// mechanism that keeps a comment-linked activity off the timeline —
	// ListDocumentActivityBeforeTime excludes those at the query, which is
	// the only place the check is exact: comments and activities arrive
	// through separately bounded windows, and a guard built from the
	// comments in hand cannot see an activity whose comment fell outside the
	// comment window (TASK-2760, codex round 2). What this set covers is the
	// read skew between the two queries: the sources are read at separate
	// instants with no shared snapshot (see TimelineEntry's doc), so a
	// comment fetched here and hard-deleted before the activity query runs
	// leaves its activity eligible again while the stale comment is still in
	// memory, and both would render once (codex round 4). Two guards, two
	// distinct failure modes; neither substitutes for the other. The same
	// skew runs the other way too: a comment created between the two reads
	// is in neither — its activity is excluded by the query, and the comment
	// missed the earlier read (codex round 5). Before the exclusion that
	// window showed a ghost "commented" card with no comment behind it; now
	// it shows nothing, and the next fetch (or the comment.created SSE
	// event) is consistent. Neither guard can close a gap that lives between
	// two reads with no shared snapshot; TimelineEntry's doc states the
	// class.
	commentActivityIDs := make(map[string]bool)
	for _, c := range comments {
		if c.ActivityID != "" {
			commentActivityIDs[c.ActivityID] = true
		}
	}

	var entries []models.TimelineEntry

	// Add comment entries (only top-level; replies are nested under parents).
	commentsByID := make(map[string]*models.Comment, len(comments))
	for i := range comments {
		commentsByID[comments[i].ID] = &comments[i]
	}
	for i := range comments {
		c := comments[i]
		// Nest replies under their parent if the parent was fetched on this page.
		// If the parent is on a different page, treat the reply as a top-level entry
		// so it doesn't silently vanish.
		if c.ParentID != "" {
			if parent, ok := commentsByID[c.ParentID]; ok {
				parent.Replies = append(parent.Replies, c)
				continue
			}
			// Parent not on this page — fall through and add as top-level.
		}
		entry := models.TimelineEntry{
			ID:        c.ID,
			Kind:      "comment",
			CreatedAt: c.CreatedAt,
			Actor:     c.CreatedBy,
			ActorName: c.Author,
			// Derived from the nested comment (see TimelineEntry.AgentName);
			// the store's join is the only writer of the value being copied.
			AgentName: c.AgentName,
			Source:    c.Source,
			Comment:   &comments[i],
		}
		entries = append(entries, entry)
	}

	// Add activity entries (with dedup: skip "updated" if a version exists at
	// same second, and skip activities a fetched comment links to — the
	// read-skew guard described at the top of the function).
	for i := range activities {
		a := activities[i]

		// Skip "read" and "searched" actions — not useful in item timeline.
		if a.Action == "read" || a.Action == "searched" {
			continue
		}

		if commentActivityIDs[a.ID] {
			continue
		}

		// Skip "updated" activities that coincide with a version snapshot.
		if a.Action == "updated" && versionTimes[a.CreatedAt.Unix()] {
			continue
		}

		// Collapse rapid empty-metadata "updated" entries (within 5 min).
		if a.Action == "updated" && (a.Metadata == "" || a.Metadata == "{}") {
			continue
		}

		entry := models.TimelineEntry{
			ID:        a.ID,
			Kind:      "activity",
			CreatedAt: a.CreatedAt,
			Actor:     a.Actor,
			ActorName: a.ActorName,
			Source:    a.Source,
			Activity:  &activities[i],
		}
		entries = append(entries, entry)
	}

	// Add version entries.
	for i := range versions {
		v := versions[i]
		entry := models.TimelineEntry{
			ID:        v.ID,
			Kind:      "version",
			CreatedAt: v.CreatedAt,
			Actor:     v.CreatedBy,
			Source:    v.Source,
			Version:   &versions[i],
		}
		entries = append(entries, entry)
	}

	// Add the structured kinds. They are already TimelineEntry-shaped and
	// cursor-filtered; the sort below is what places them in the stream.
	entries = append(entries, notes...)
	entries = append(entries, decisions...)

	// Sort chronologically (newest first), with ID as tie-breaker for same-second entries.
	// This must match the SQL ORDER BY (created_at DESC, id DESC) used by the cursor queries.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].ID > entries[j].ID
		}
		return entries[i].CreatedAt.After(entries[j].CreatedAt)
	})

	return collapseAutosaveBursts(entries)
}

// autosaveBurstWindow is how close two collab-snapshot versions must be (with no
// other event between them) to collapse into a single timeline entry. The web
// editor flushes a collab-snapshot every ~5s while a user types, so without this
// every editing session litters the item timeline with near-identical "Content
// updated" cards (BUG-1612). We keep the newest snapshot of each burst as the
// restore point and drop the rest.
const autosaveBurstWindow = 10 * time.Minute

// isAutosaveVersion reports whether an entry is a collab-snapshot version row
// (the web editor's 5s auto-flush). These are the only versions we collapse —
// manual web/CLI/skill saves stay individual.
func isAutosaveVersion(e models.TimelineEntry) bool {
	return e.Kind == "version" && e.Version != nil && e.Version.Source == "collab-snapshot"
}

// collapseAutosaveBursts walks the newest-first entries and drops a
// collab-snapshot version when the previous kept entry is also a collab-snapshot
// version within autosaveBurstWindow — i.e. an uninterrupted burst of autosaves
// collapses to its newest row. Any non-autosave entry between two autosaves
// breaks the run, so each distinct editing session still leaves one restore point.
func collapseAutosaveBursts(entries []models.TimelineEntry) []models.TimelineEntry {
	if len(entries) == 0 {
		return entries
	}
	kept := entries[:0:0]
	var lastAutosaveAt time.Time
	for _, e := range entries {
		if isAutosaveVersion(e) {
			if !lastAutosaveAt.IsZero() && lastAutosaveAt.Sub(e.CreatedAt) <= autosaveBurstWindow {
				// Same burst as the autosave we already kept — skip this older one.
				lastAutosaveAt = e.CreatedAt
				continue
			}
			lastAutosaveAt = e.CreatedAt
		} else {
			// A non-autosave event ends the current burst.
			lastAutosaveAt = time.Time{}
		}
		kept = append(kept, e)
	}
	return kept
}

// exhaustedWindowCursor returns the position the next timeline page must start
// strictly before, when the merged page fit inside `limit` but at least one
// source filled its over-fetch window.
//
// WHICH tail is the subtle part. A source that returned fewer than `perSource`
// rows is EXHAUSTED — there is nothing older than its tail to come back for —
// so it puts no constraint on the cursor. A source that filled its window has
// unexamined rows strictly older than its own tail. Resuming at the NEWEST
// such tail therefore skips nothing: every full source is picked up exactly
// where it stopped, and the sources whose tails are older simply get rows
// re-examined that this page already rendered. Choosing the oldest tail
// instead would step over the newer source's unexamined rows and lose them
// permanently, which is the failure this cursor exists to prevent — repeats
// are absorbed by the client's dedup-by-id, gaps are not recoverable.
//
// Progress is guaranteed because every candidate is a row this page FETCHED,
// and the store's cursor predicate is strict: any tail is strictly older than
// the cursor that produced it, so the next request cannot re-ask this one.
//
// The structured kinds (notes, decisions) are deliberately absent: they live
// in the item's fields blob, are filtered by the same cursor predicate, and
// arrive complete rather than windowed — there is never an unexamined
// remainder of them to page toward.
func exhaustedWindowCursor(
	comments []models.Comment,
	activities []models.Activity,
	versions []models.Version,
	perSource int,
) (time.Time, string, bool) {
	var at time.Time
	var id string
	found := false

	consider := func(t time.Time, rowID string, full bool) {
		if !full {
			return
		}
		if !found || t.After(at) || (t.Equal(at) && rowID > id) {
			at, id, found = t, rowID, true
		}
	}

	if n := len(comments); n > 0 {
		consider(comments[n-1].CreatedAt, comments[n-1].ID, n >= perSource)
	}
	if n := len(activities); n > 0 {
		consider(activities[n-1].CreatedAt, activities[n-1].ID, n >= perSource)
	}
	if n := len(versions); n > 0 {
		consider(versions[n-1].CreatedAt, versions[n-1].ID, n >= perSource)
	}

	return at, id, found
}
