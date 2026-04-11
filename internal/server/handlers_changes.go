package server

import (
	"net/http"
	"strconv"
	"time"
)

// ChangesResponse is the response for GET /workspaces/{ws}/changes?since=<unix_ms>.
type ChangesResponse struct {
	// Updated items (with full item data).
	Updated []interface{} `json:"updated"`
	// IDs of items that were deleted since the requested timestamp.
	Deleted []string `json:"deleted"`
	// Server timestamp at the time of this response (unix ms).
	// Clients should use this as the `since` value for the next sync.
	ServerTime int64 `json:"server_time"`
	// Whether the collection metadata (counts, schemas) may have changed.
	// True if any items were updated/deleted, signaling the client should
	// also refresh collection metadata.
	CollectionsChanged bool `json:"collections_changed"`
}

// handleGetChanges returns items modified since a given timestamp.
// GET /api/v1/workspaces/{ws}/changes?since=<unix_milliseconds>
//
// This is the incremental sync endpoint used by the frontend when the
// tab regains focus. Instead of re-fetching everything, the client sends
// the timestamp of its last successful sync and gets back only the delta.
func (s *Server) handleGetChanges(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	sinceStr := r.URL.Query().Get("since")
	if sinceStr == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "since query parameter is required (unix milliseconds)")
		return
	}

	sinceMs, err := strconv.ParseInt(sinceStr, 10, 64)
	if err != nil || sinceMs < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "since must be a valid unix timestamp in milliseconds")
		return
	}

	since := time.UnixMilli(sinceMs)
	serverTime := time.Now().UnixMilli()

	updated, deletedIDs, err := s.store.ItemsModifiedSince(workspaceID, since)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Filter by collection visibility so restricted members only see
	// changes from collections they have access to.
	visibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
	if visErr != nil {
		writeInternalError(w, visErr)
		return
	}
	if visibleIDs != nil {
		filtered := updated[:0]
		allowedDeleted := deletedIDs[:0]
		visibleSet := make(map[string]bool, len(visibleIDs))
		for _, id := range visibleIDs {
			visibleSet[id] = true
		}
		for _, item := range updated {
			if visibleSet[item.CollectionID] {
				filtered = append(filtered, item)
			}
		}
		updated = filtered
		// For deleted items, re-query with collection filter since we
		// only have IDs. Fetch their collection_ids from the soft-deleted rows.
		if len(deletedIDs) > 0 {
			delItems, delErr := s.store.GetDeletedItemsWithCollection(workspaceID, deletedIDs)
			if delErr != nil {
				writeInternalError(w, delErr)
				return
			}
			for _, item := range delItems {
				if visibleSet[item.CollectionID] {
					allowedDeleted = append(allowedDeleted, item.ID)
				}
			}
			deletedIDs = allowedDeleted
		}
	}

	// Convert to interface slice for JSON marshaling.
	updatedItems := make([]interface{}, len(updated))
	for i, item := range updated {
		updatedItems[i] = item
	}
	if updatedItems == nil {
		updatedItems = []interface{}{}
	}
	if deletedIDs == nil {
		deletedIDs = []string{}
	}

	resp := ChangesResponse{
		Updated:            updatedItems,
		Deleted:            deletedIDs,
		ServerTime:         serverTime,
		CollectionsChanged: len(updated) > 0 || len(deletedIDs) > 0,
	}

	writeJSON(w, http.StatusOK, resp)
}
