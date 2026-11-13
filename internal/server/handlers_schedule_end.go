package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// itemStillOpenMessage names both ways forward, so a client that prints only
// the server's message still tells its user what to do.
const itemStillOpenMessage = "This is the schedule's last occurrence and the item is still open. " +
	"Close the item first and then resolve again, or confirm ending the schedule and keeping the item open " +
	"by retrying with end_schedule (--end-schedule in the pad CLI, or choose it in the web app)."

// writeItemStillOpen answers a done or skip refused because it would end the
// schedule of an open item: 409 item_still_open with what the caller needs to
// offer the choices. The caller passed the visibility and edit gates.
func (s *Server) writeItemStillOpen(w http.ResponseWriter, r *http.Request, item *models.Item, in store.ResolveInput, end *store.ErrScheduleWouldEnd) {
	coll, err := s.store.GetCollection(item.CollectionID)
	if err != nil || coll == nil {
		writeStoreErrorR(w, r, err, http.StatusInternalServerError, "internal_error", "Failed to load collection")
		return
	}
	var schema models.CollectionSchema
	if err := models.UnmarshalItemFieldSchema([]byte(coll.Schema), &schema); err != nil {
		// 500: stored collection schema does not parse (json), not a store error
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to parse collection schema")
		return
	}
	var settings models.CollectionSettings
	if coll.Settings != "" {
		_ = json.Unmarshal([]byte(coll.Settings), &settings)
	}
	doneField, positive := models.PositiveTerminalValuesForDoneField(schema, settings)
	closeOptions := filterDeclaredOptions(schema, doneField, positive)
	writeError2(w, http.StatusConflict, "item_still_open", itemStillOpenMessage, map[string]interface{}{
		"state":         in.State,
		"occurrence_at": in.OccurrenceAt.UTC().Format(time.RFC3339),
		"done_field":    doneField,
		"item_status":   extractFieldString(item.Fields, doneField),
		"close_options": closeOptions,
		"ack_available": end.Fired && !end.Acknowledged,
	})
}

// filterDeclaredOptions keeps the values the done field declares as options,
// in the field's order, when it declares any; otherwise the values as given.
func filterDeclaredOptions(schema models.CollectionSchema, fieldKey string, values []string) []string {
	out := []string{}
	for _, f := range schema.Fields {
		if f.Key != fieldKey || len(f.Options) == 0 {
			continue
		}
		want := map[string]bool{}
		for _, v := range values {
			want[strings.ToLower(strings.TrimSpace(v))] = true
		}
		for _, o := range f.Options {
			if want[strings.ToLower(strings.TrimSpace(o))] {
				out = append(out, o)
			}
		}
		return out
	}
	return append(out, values...)
}

// handleDismissEndedCommitment is POST .../commitment/dismiss-ended: an editor
// takes an ended schedule out of Outstanding while the item stays open. The
// dismissal is shared, like an acknowledgement.
func (s *Server) handleDismissEndedCommitment(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	item := s.resolveVisibleItem(w, r, workspaceID)
	if item == nil {
		return
	}
	if !s.requireEditPermission(w, r, workspaceID, item.ID, item.CollectionID) {
		return
	}
	raw, err := readBodyForDecode(r, defaultJSONBodyLimit)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if _, err := scheduleBodyBytes[struct{}](raw); err != nil {
			writeScheduleBodyError(w)
			return
		}
	}
	changed, err := s.store.DismissEndedSchedule(workspaceID, item.ID)
	if errors.Is(err, store.ErrScheduleNotEnded) {
		writeError(w, http.StatusConflict, "not_ended", "The schedule has an outstanding occurrence; there is nothing ended to dismiss.")
		return
	}
	if errors.Is(err, store.ErrScheduleNotFound) {
		writeScheduleNotFound(w)
		return
	}
	if s.writeScheduleItemGone(w, r, workspaceID, err) || s.writeScheduleError(w, r, err) {
		return
	}
	sc, err := s.store.GetSchedule(workspaceID, item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if sc == nil {
		writeScheduleNotFound(w)
		return
	}
	if changed {
		s.recordScheduleWrite(r, workspaceID, item, "schedule_end_dismissed", scheduleActivityMeta(sc.Kind, nil))
	}
	v, err := s.buildCommitmentView(r, workspaceID, item, sc, "")
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commitment": v})
}
