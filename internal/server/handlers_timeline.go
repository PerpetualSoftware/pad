package server

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/models"
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
	item, err := s.store.ResolveItem(workspaceID, itemSlug)
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
	// Defaults to now + a future-biased ID to include "just now" entries on first page.
	before := time.Now().UTC().Add(time.Minute)
	beforeID := "\xff" // sorts after all UUIDs on first page
	if v := r.URL.Query().Get("before"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			before = t
		} else if t, err := time.Parse(time.RFC3339, v); err == nil {
			before = t
		}
	}
	if v := r.URL.Query().Get("before_id"); v != "" {
		beforeID = v
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

	entries := buildTimeline(comments, activities, versions)

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

// buildTimeline merges comments, activities, and versions into a single chronological
// stream, applying deduplication and collapsing logic.
func buildTimeline(comments []models.Comment, activities []models.Activity, versions []models.Version) []models.TimelineEntry {
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

	// Sort chronologically (newest first), with ID as tie-breaker for same-second entries.
	// This must match the SQL ORDER BY (created_at DESC, id DESC) used by the cursor queries.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].ID > entries[j].ID
		}
		return entries[i].CreatedAt.After(entries[j].CreatedAt)
	})

	return entries
}
