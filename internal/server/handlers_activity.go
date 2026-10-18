package server

import (
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func (s *Server) handleListWorkspaceActivity(w http.ResponseWriter, r *http.Request) {
	// Guests should not see workspace-level activity, which includes
	// audit events (member invites, role changes, etc.) with operational metadata.
	if !requireMinRole(w, r, "viewer") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	params := models.ActivityListParams{
		Action: r.URL.Query().Get("action"),
		Actor:  r.URL.Query().Get("actor"),
		Source: r.URL.Query().Get("source"),
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			params.Limit = l
		}
	}
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil {
			params.Offset = o
		}
	}
	var cursorOK bool
	if params.Before, params.BeforeID, cursorOK = activityCursorFromQuery(w, r, params.Offset); !cursorOK {
		return
	}
	// since=YYYY-MM-DD restricts to activity on or after that date. Applied
	// in the store query so LIMIT counts post-filter rows. Backs
	// `pad project activity --since` and the pad_project.activity MCP action.
	// An RFC3339 instant is accepted too (TASK-3615): the catch-up card's
	// "See everything" carries its exact window. The store normalises it to
	// UTC at second precision, so a fraction rounds the floor down and an
	// offset is converted, never read as a UTC wall time.
	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		parsed, err := time.Parse("2006-01-02", sinceStr)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, sinceStr)
		}
		// The store compares the cutoff as RFC3339 text, so a UTC instant past
		// year 9999 ("10000-…") would sort below every row and open the window
		// to everything. An offset can carry 9999-12-31 there (TASK-3615 PR B r5).
		if err != nil || parsed.UTC().Year() > 9999 {
			writeError(w, http.StatusBadRequest, "invalid_since", "`since` must be a date in YYYY-MM-DD format or an RFC3339 timestamp")
			return
		}
		params.Since = parsed
	}

	// Filter by collection visibility and item-level grants
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// collection=<slug> narrows to activity on that collection's items, in
	// the store query (TASK-2219): the web page used to filter the loaded
	// page client-side, which showed a few rows of one page as the total and
	// hid Load more. A slug that names no live collection, and one the caller
	// cannot see, both answer an empty list, so the filter is not an
	// existence oracle.
	if collSlug := r.URL.Query().Get("collection"); collSlug != "" {
		coll, err := s.store.GetCollectionBySlug(workspaceID, collSlug)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if coll == nil || (visibleIDs != nil && !slices.Contains(visibleIDs, coll.ID)) {
			writeJSON(w, http.StatusOK, []models.Activity{})
			return
		}
		params.CollectionID = coll.ID
	}

	// Visibility is applied in the store query, so LIMIT and the keyset
	// cursor count only rows this caller may see (BUG-3687). It used to be
	// applied here, after the LIMIT, which handed a restricted member short
	// or empty pages, and the web feed reads a short page as the end.
	if visibleIDs != nil {
		fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilter(r, workspaceID)
		if grantErr != nil {
			writeInternalError(w, grantErr)
			return
		}
		params.Visibility = &models.ActivityVisibility{
			CollectionIDs:     visibleIDs,
			FullCollectionIDs: fullCollIDs,
			GrantedItemIDs:    grantedItemIDs,
		}
	}

	activities, err := s.store.ListWorkspaceActivity(workspaceID, params)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if activities == nil {
		activities = []models.Activity{}
	}

	// Enrich activities with item titles and collection info
	s.enrichActivities(activities)

	writeJSON(w, http.StatusOK, activities)
}

func (s *Server) handleListDocumentActivity(w http.ResponseWriter, r *http.Request) {
	// Legacy documents are not covered by the grants model — block guests.
	if !requireMinRole(w, r, "viewer") {
		return
	}
	_, doc, ok := s.getWorkspaceDocument(w, r)
	if !ok {
		return
	}

	params := models.ActivityListParams{
		Action: r.URL.Query().Get("action"),
		Actor:  r.URL.Query().Get("actor"),
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			params.Limit = l
		}
	}
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil {
			params.Offset = o
		}
	}
	var cursorOK bool
	if params.Before, params.BeforeID, cursorOK = activityCursorFromQuery(w, r, params.Offset); !cursorOK {
		return
	}

	activities, err := s.store.ListDocumentActivity(doc.ID, params)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if activities == nil {
		activities = []models.Activity{}
	}

	// Enrich activities with item titles and collection info
	s.enrichActivities(activities)

	writeJSON(w, http.StatusOK, activities)
}

// enrichActivities populates ItemTitle, ItemSlug, and CollectionSlug
// on each activity by looking up the referenced item.
func (s *Server) enrichActivities(activities []models.Activity) {
	for i := range activities {
		if activities[i].DocumentID == "" {
			continue
		}
		// Include-deleted so an archived item's activity is enriched with its
		// real title/slug/collection instead of staying blank — a blank row
		// otherwise masquerades as workspace-level activity and bypasses the
		// collection-visibility filter applied by the caller.
		item, err := s.store.GetItemIncludeDeleted(activities[i].DocumentID)
		if err != nil || item == nil {
			continue
		}
		item.ComputeRef()
		activities[i].ItemTitle = item.Title
		activities[i].ItemSlug = item.Slug
		activities[i].ItemRef = item.Ref
		activities[i].CollectionSlug = item.CollectionSlug
	}
}
