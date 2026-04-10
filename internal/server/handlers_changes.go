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
