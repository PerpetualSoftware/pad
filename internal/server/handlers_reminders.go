package server

import (
	"net/http"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/go-chi/chi/v5"
)

// Item reminder handlers (IDEA-2641, GitHub #1010).
//
// The write surface is deliberately small: arm, re-arm, acknowledge, disarm.
// A reminder has no content of its own — it is an instant and a lifecycle —
// so there is nothing else to edit.

// reminderRequest is the arm/re-arm body.
type reminderRequest struct {
	RemindAt string `json:"remind_at"`
}

// parseRemindAt normalizes a caller-supplied instant to RFC3339 UTC.
//
// THE PARSE IS STRICT AND THE NORMALIZATION HAPPENS HERE, once, at the edge.
// Everything downstream compares remind_at as a string against a UTC clock, so
// a value that reached the store still carrying a local offset would compare
// wrong by that offset — and it would compare wrong SILENTLY, firing early or
// late with nothing in the row to show why. Doing it at the boundary means the
// store never has to reason about zones and there is exactly one place that
// decides what an instant means.
//
// A BARE DATE IS REFUSED, and this is the one refusal worth explaining: the
// `date` schema type accepts `YYYY-MM-DD`, so a caller reasonably expects it
// here too. But a bare date does not name an instant — "2026-08-01" is a
// 24-hour span, and picking midnight for them would be this code inventing a
// time the user did not choose and then firing at it. Refusing with a message
// that names the accepted form costs one round trip; guessing costs a reminder
// that arrives at 00:00 for someone who meant "that morning".
func parseRemindAt(raw string) (string, error) {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "", err
	}
	return t.UTC().Format(time.RFC3339), nil
}

func writeRemindAtError(w http.ResponseWriter) {
	writeError(w, http.StatusBadRequest, "invalid_remind_at",
		"remind_at must be an RFC3339 instant (e.g. 2026-08-01T09:00:00Z). A bare date has no time of day, so it is refused rather than assumed to mean midnight.")
}

// handleListItemReminders returns every reminder on an item, armed or fired.
// GET /api/v1/workspaces/{slug}/items/{itemSlug}/reminders
func (s *Server) handleListItemReminders(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	item := s.resolveVisibleItem(w, r, workspaceID)
	if item == nil {
		return
	}

	reminders, err := s.store.ListRemindersForItem(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref":       item.Ref,
		"reminders": reminders,
	})
}

// handleCreateItemReminder arms a reminder on an item.
// POST /api/v1/workspaces/{slug}/items/{itemSlug}/reminders
func (s *Server) handleCreateItemReminder(w http.ResponseWriter, r *http.Request) {
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

	var req reminderRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Request body must be JSON")
		return
	}
	remindAt, err := parseRemindAt(req.RemindAt)
	if err != nil {
		writeRemindAtError(w)
		return
	}

	reminder, err := s.store.CreateReminder(workspaceID, item.ID, remindAt)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, reminder)
}

// handleRearmReminder moves a reminder's instant, clearing its fire marks.
// PATCH /api/v1/workspaces/{slug}/reminders/{reminderID}
func (s *Server) handleRearmReminder(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	reminder, _ := s.resolveReminderForWrite(w, r, workspaceID)
	if reminder == nil {
		return
	}

	var req reminderRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Request body must be JSON")
		return
	}
	remindAt, err := parseRemindAt(req.RemindAt)
	if err != nil {
		writeRemindAtError(w)
		return
	}

	updated, err := s.store.RearmReminder(workspaceID, reminder.ID, remindAt)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "not_found", "Reminder not found")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleAckReminder acknowledges a fired reminder.
// POST /api/v1/workspaces/{slug}/reminders/{reminderID}/ack
func (s *Server) handleAckReminder(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	reminder, _ := s.resolveReminderForWrite(w, r, workspaceID)
	if reminder == nil {
		return
	}

	acked, err := s.store.AckReminder(workspaceID, reminder.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if acked == nil {
		// The row exists — resolveReminderForWrite just read it — so a
		// no-op ack means it was not in the fired-unacked state. Report
		// WHICH, because "nothing happened" is the same response for an
		// armed reminder (too early) and an already-acked one (already
		// done), and those need opposite reactions from the caller.
		if reminder.FiredAt == nil {
			writeError(w, http.StatusConflict, "reminder_not_fired",
				"This reminder has not fired yet, so there is nothing to acknowledge.")
			return
		}
		writeJSON(w, http.StatusOK, reminder)
		return
	}
	writeJSON(w, http.StatusOK, acked)
}

// handleDeleteReminder disarms a reminder by removing it.
// DELETE /api/v1/workspaces/{slug}/reminders/{reminderID}
func (s *Server) handleDeleteReminder(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	reminder, _ := s.resolveReminderForWrite(w, r, workspaceID)
	if reminder == nil {
		return
	}

	removed, err := s.store.DeleteReminder(workspaceID, reminder.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "not_found", "Reminder not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": reminder.ID, "deleted": true})
}

// resolveVisibleItem resolves {itemSlug} and enforces read visibility,
// writing the error response itself. Returns nil when the caller should stop.
func (s *Server) resolveVisibleItem(w http.ResponseWriter, r *http.Request, workspaceID string) *models.Item {
	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return nil
	}
	if item == nil {
		s.writeItemResolveError(w, r, workspaceID, itemSlug)
		return nil
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return nil
	}
	return item
}

// resolveReminderForWrite resolves {reminderID} and enforces edit permission
// on the ITEM the reminder hangs off.
//
// PERMISSION IS THE ITEM'S, not the reminder's, and the reminder has no
// separate owner on purpose: a reminder is a property of an item's schedule,
// so anyone who may edit the item may schedule work on it, and anyone who may
// not must not be able to arm one and make the workspace notify about it.
//
// The visibility check runs BEFORE the edit check for the usual reason: an
// edit-permission failure on an item the caller cannot see would confirm the
// reminder exists, which is the existence-oracle shape a sibling handler
// family already had to be fixed for.
func (s *Server) resolveReminderForWrite(w http.ResponseWriter, r *http.Request, workspaceID string) (*models.Reminder, *models.Item) {
	id := chi.URLParam(r, "reminderID")
	reminder, err := s.store.GetReminder(workspaceID, id)
	if err != nil {
		writeInternalError(w, err)
		return nil, nil
	}
	if reminder == nil {
		writeError(w, http.StatusNotFound, "not_found", "Reminder not found")
		return nil, nil
	}
	item, err := s.store.GetItem(reminder.ItemID)
	if err != nil {
		writeInternalError(w, err)
		return nil, nil
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Reminder not found")
		return nil, nil
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return nil, nil
	}
	if !s.requireEditPermission(w, r, workspaceID, item.ID, item.CollectionID) {
		return nil, nil
	}
	return reminder, item
}
