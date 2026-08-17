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

// handleListItemTimeline returns a unified, chronological timeline for an item.
// It uses cursor-based pagination: pass `before=<RFC3339>` to get entries older
// than that timestamp, and `limit=N` to control page size (default 50).
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
		beforeID = "g" // > any UUID character lex-wise; valid UTF-8
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

	// Determine if there are more entries beyond this page.
	hasMore := false
	if len(entries) > limit {
		entries = entries[:limit]
		hasMore = true
	} else {
		// Even if merged result is <= limit, there may be more in individual sources.
		// If any source returned its full limit, there's likely more data.
		hasMore = len(comments) >= perSource || len(activities) >= perSource || len(versions) >= perSource
	}

	writeJSON(w, http.StatusOK, models.TimelineResponse{
		Entries: entries,
		HasMore: hasMore,
	})
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
//     Anchoring at the zero time instead would render as 1970 in the UI.
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

	// Build a set of activity IDs that are linked to comments (to show as combined cards).
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
			Source:    c.Source,
			Comment:   &comments[i],
		}
		entries = append(entries, entry)
	}

	// Add activity entries (with dedup: skip "updated" if a version exists at same second,
	// and skip activities that are linked to a comment since they'll be shown as combined cards).
	for i := range activities {
		a := activities[i]

		// Skip "read" and "searched" actions — not useful in item timeline.
		if a.Action == "read" || a.Action == "searched" {
			continue
		}

		// Skip activities that already have a linked comment — they're shown via the comment card.
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
