package server

import (
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/models"
)

// handleListItemTimeline returns a unified, chronological timeline for an item.
// It merges comments, activities, and versions into a single stream with deduplication.
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

	limit := 100
	offset := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	// Fetch all three data sources.
	comments, err := s.store.ListComments(item.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Bulk-load reactions for all comments.
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

	activities, err := s.store.ListDocumentActivity(item.ID, models.ActivityListParams{Limit: 10000})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	versions, err := s.store.ListItemVersions(item.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	entries := buildTimeline(comments, activities, versions)
	total := len(entries)

	// Apply pagination.
	if offset >= len(entries) {
		entries = nil
	} else {
		end := offset + limit
		if end > len(entries) {
			end = len(entries)
		}
		entries = entries[offset:end]
	}

	writeJSON(w, http.StatusOK, models.TimelineResponse{
		Entries: entries,
		Total:   total,
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
		// Skip replies — they'll be nested under their parent.
		if c.ParentID != "" {
			if parent, ok := commentsByID[c.ParentID]; ok {
				parent.Replies = append(parent.Replies, c)
			}
			continue
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

	// Sort chronologically (newest first).
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.After(entries[j].CreatedAt)
	})

	return entries
}

