package server

import (
	"net/http"
	"strconv"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// StaleBodyImportHeader reports how many items in an imported bundle carried a
// body that was BEHIND the source item's live collaborative document when the
// bundle was written (BUG-3032 / BUG-3000).
//
// A HEADER rather than a field on the response, for the reason the NUL-repair
// tally next door is one: both import doors answer with the bare
// models.Workspace, so a count in the body would mean wrapping it in an
// envelope and breaking every existing client to deliver an advisory.
//
// A COUNT rather than a per-row mark on the destination, which was the other
// candidate and is wrong: the destination's op-log is empty, so nothing there is
// ahead of those rows. Stamping content_state on them would assert a pending
// flush that cannot exist in this workspace, and would never clear.
const StaleBodyImportHeader = "X-Pad-Import-Stale-Bodies"

// staleBodyTally counts an incoming bundle's stale bodies.
//
// It reads the bundle's OWN marker rather than recomputing anything, which is
// the point: the exporter evaluated the predicate against the row it actually
// serialised, and this side has no op-log to evaluate it against at all. One
// predicate, one evaluation, at the only place it can be true.
type staleBodyTally struct {
	Count int
}

// Observe records the count from a decoded bundle. Safe on a nil receiver so a
// door that does not care can pass nil.
func (t *staleBodyTally) Observe(data *models.WorkspaceExport) {
	if t == nil || data == nil {
		return
	}
	for _, it := range data.Items {
		if it.ContentState != "" {
			t.Count++
		}
	}
}

// SetHeader reports the count on a successful import, and says nothing when
// there is nothing to say — so a clean import's response is unchanged.
func (t *staleBodyTally) SetHeader(w http.ResponseWriter) {
	if t == nil || t.Count == 0 {
		return
	}
	w.Header().Set(StaleBodyImportHeader, strconv.Itoa(t.Count))
}
