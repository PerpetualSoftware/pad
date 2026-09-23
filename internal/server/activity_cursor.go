package server

import (
	"net/http"
	"time"
)

// activityCursorFromQuery reads the keyset cursor the paginated activity
// feeds accept (BUG-2781): before=<created_at, RFC3339> and before_id=<id>,
// both taken by the client from the LAST row it already holds. It writes a
// 400 and returns ok=false on a malformed cursor.
//
// BOTH OR NEITHER. A timestamp alone skips the rest of a run of rows sharing
// that second (created_at is stored at whole-second precision), and an id
// alone orders nothing. Refused rather than degraded, because either half on
// its own pages silently wrong.
//
// NEVER WITH offset. Offset stays accepted on its own through a deprecation
// window, for callers that have not moved; combining the two has no single
// meaning, so it is refused rather than one of them being picked.
func activityCursorFromQuery(w http.ResponseWriter, r *http.Request, offset int) (before time.Time, beforeID string, ok bool) {
	q := r.URL.Query()
	rawBefore, rawID := q.Get("before"), q.Get("before_id")
	if rawBefore == "" && rawID == "" {
		return time.Time{}, "", true
	}
	if rawBefore == "" || rawID == "" {
		writeError(w, http.StatusBadRequest, "invalid_cursor", "`before` and `before_id` must be sent together: both come from the last row of the previous page")
		return time.Time{}, "", false
	}
	if offset > 0 {
		writeError(w, http.StatusBadRequest, "invalid_cursor", "`offset` cannot be combined with `before`/`before_id`; page with the cursor alone")
		return time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339Nano, rawBefore)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cursor", "`before` must be an RFC3339 timestamp, the `created_at` of the last row of the previous page")
		return time.Time{}, "", false
	}
	return t, rawID, true
}
