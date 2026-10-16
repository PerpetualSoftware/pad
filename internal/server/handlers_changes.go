package server

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// ChangesResponse is the response for GET /workspaces/{ws}/changes?since=<unix_ms>.
type ChangesResponse struct {
	// Updated items (with full item data).
	Updated []interface{} `json:"updated"`
	// IDs of items that were deleted since the requested timestamp.
	Deleted []string `json:"deleted"`
	// Server timestamp at the time of the FIRST page's request (unix ms); every
	// page of one walk repeats it. Clients should use this as the `since` value
	// for the next sync, once the walk has no more pages.
	ServerTime int64 `json:"server_time"`
	// Whether the collection metadata (counts, schemas) may have changed.
	// True if any items were updated/deleted, signaling the client should
	// also refresh collection metadata.
	CollectionsChanged bool `json:"collections_changed"`
	// HasMore says the walk continues: ask again with `cursor=NextCursor`
	// (BUG-3791). A caller that stops early has NOT seen every change since
	// `since`, and must not advance its own cursor to ServerTime.
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// changesCursor is the opaque /changes continuation: the workspace it walks,
// the walk's `since` and first-page server time, and the last row the previous
// page returned.
type changesCursor struct {
	WorkspaceID  string
	SinceMs      int64
	ServerTimeMs int64
	After        store.ChangesPosition
}

const changesCursorPrefix = "c1."

func (c changesCursor) encode() string {
	raw := fmt.Sprintf("%s.%d.%d.%d.%s", c.WorkspaceID, c.SinceMs, c.ServerTimeMs, c.After.Seq, c.After.ID)
	return changesCursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeChangesCursor(s string) (changesCursor, bool) {
	var c changesCursor
	body, ok := strings.CutPrefix(s, changesCursorPrefix)
	if !ok {
		return c, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return c, false
	}
	parts := strings.SplitN(string(raw), ".", 5)
	if len(parts) != 5 || parts[0] == "" || parts[4] == "" {
		return c, false
	}
	nums := make([]int64, 3)
	for i := range nums {
		v, err := strconv.ParseInt(parts[i+1], 10, 64)
		if err != nil || v < 0 {
			return c, false
		}
		nums[i] = v
	}
	c.WorkspaceID = parts[0]
	c.SinceMs, c.ServerTimeMs, c.After = nums[0], nums[1], store.ChangesPosition{Seq: nums[2], ID: parts[4]}
	return c, true
}

// handleGetChanges returns items modified since a given timestamp, one bounded
// page at a time.
// GET /api/v1/workspaces/{ws}/changes?since=<unix_milliseconds>[&limit=N]
// GET /api/v1/workspaces/{ws}/changes?cursor=<next_cursor>[&limit=N]
//
// This is the incremental sync endpoint used by the frontend when the
// tab regains focus. Instead of re-fetching everything, the client sends
// the timestamp of its last successful sync and gets back only the delta.
//
// A page holds at most `limit` items (missing, invalid or above the maximum:
// store.MaxChangesLimit). When more remain, `has_more` is true and
// `next_cursor` continues the walk (BUG-3791). The cursor is a position in the
// workspace's write order, so a change committed during the walk is returned
// by a later page of it.
func (s *Server) handleGetChanges(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	var walk changesCursor
	var after *store.ChangesPosition
	if cursorStr := q.Get("cursor"); cursorStr != "" {
		c, ok := decodeChangesCursor(cursorStr)
		// A cursor continues one workspace's walk; another's position and
		// since would skip this one's rows.
		if !ok || c.WorkspaceID != workspaceID {
			writeError(w, http.StatusBadRequest, "bad_request", "cursor is not a /changes next_cursor for this workspace")
			return
		}
		if sinceStr := q.Get("since"); sinceStr != "" && sinceStr != strconv.FormatInt(c.SinceMs, 10) {
			writeError(w, http.StatusBadRequest, "bad_request", "since does not match the cursor's walk; send the cursor alone")
			return
		}
		walk = c
		after = &walk.After
	} else {
		sinceStr := q.Get("since")
		if sinceStr == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "since query parameter is required (unix milliseconds)")
			return
		}
		sinceMs, err := strconv.ParseInt(sinceStr, 10, 64)
		if err != nil || sinceMs < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "since must be a valid unix timestamp in milliseconds")
			return
		}
		// Taken before the reads, so a change committed after them is at or
		// after the caller's next since.
		walk = changesCursor{WorkspaceID: workspaceID, SinceMs: sinceMs, ServerTimeMs: time.Now().UnixMilli()}
	}

	// As the item lists take it (BUG-3704): a missing or invalid limit is the
	// maximum, and an explicit one is clamped to it.
	limit := store.MaxChangesLimit
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v < limit {
		limit = v
	}

	params := store.ItemsModifiedSinceParams{
		Since: time.UnixMilli(walk.SinceMs),
		After: after,
		Limit: limit + 1,
	}

	// Filter by collection visibility so restricted members only see
	// changes from collections they have access to.
	visibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
	if visErr != nil {
		writeInternalError(w, visErr)
		return
	}
	if visibleIDs != nil {
		params.CollectionIDs = visibleIDs
		// For guests with item-level grants, a row in a collection visible only
		// through item grants must be one of the granted items. The
		// include-deleted variant, as /items-changes uses (TASK-1354), so an
		// archived granted item still reaches its grantee (BUG-3796).
		fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilterIncludeDeletedItems(r, workspaceID)
		if grantErr != nil {
			writeInternalError(w, grantErr)
			return
		}
		params.FullCollectionIDs = fullCollIDs
		params.GrantedItemIDs = grantedItemIDs
	}

	updated, err := s.store.ItemsModifiedSince(workspaceID, params)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	hasMore := len(updated) > limit
	if hasMore {
		updated = updated[:limit]
	}

	// make() always returns a non-nil slice, which serialises to "[]" rather
	// than "null".
	s.hydrateItemsAppAttribution(updated) // SPEC-6 U9c
	updatedItems := make([]interface{}, len(updated))
	deletedIDs := []string{}
	for i, item := range updated {
		updatedItems[i] = item
		if item.DeletedAt != nil {
			deletedIDs = append(deletedIDs, item.ID)
		}
	}

	resp := ChangesResponse{
		Updated:            updatedItems,
		Deleted:            deletedIDs,
		ServerTime:         walk.ServerTimeMs,
		CollectionsChanged: len(updated) > 0,
		HasMore:            hasMore,
	}
	if hasMore {
		last := updated[len(updated)-1]
		next := walk
		next.After = store.ChangesPosition{Seq: last.Seq, ID: last.ID}
		resp.NextCursor = next.encode()
	}

	writeJSON(w, http.StatusOK, resp)
}
