package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
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
	return store.NormalizeInstant(t), nil
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

	reminders, err := s.store.ListRemindersForItem(workspaceID, item.ID)
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
	if errors.Is(err, store.ErrReminderItemGone) {
		// resolveVisibleItem saw a live item in this workspace a moment ago;
		// the store's own predicate did not. The item was archived in the
		// window, and the answer is the one the resolver would have given.
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}
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
		// THE PRE-READ IS NOT CONSULTED HERE (codex round 12). AckReminder
		// matches every fired row, acknowledged or not, so a nil answer means
		// exactly "not fired at the instant of the ack" — or "gone", which a
		// fresh read tells apart. The earlier form decided 409-vs-200 from the
		// row resolveReminderForWrite read before the UPDATE, and a fire or
		// re-arm landing in between made it answer for a state that had
		// already stopped holding. The variable is still called `reminder`
		// above only because the resolver's permission checks need it.
		current, err := s.store.GetReminder(workspaceID, reminder.ID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if current == nil {
			writeError(w, http.StatusNotFound, "not_found", "Reminder not found")
			return
		}
		writeError(w, http.StatusConflict, "reminder_not_fired",
			"This reminder has not fired yet, so there is nothing to acknowledge.")
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

// Bounds for the pending-reminder read (IDEA-2641, codex round 4).
const (
	// pendingReminderWindow is how many pending reminders the dashboard shows.
	pendingReminderWindow = 50

	// pendingReminderMaxScan bounds how many rows may be READ to fill that
	// window. The two differ because one filter cannot run in SQL: terminality
	// is defined by a collection's schema, so a workspace where most reminders
	// sit on completed items would otherwise need an unbounded scan to fill a
	// bounded window.
	//
	// THE RECEIPT: 10x the window, so the common shape — a handful of finished
	// items among live ones — fills the window on the first page, and the
	// pathological shape (hundreds of completed items with reminders, which the
	// documented "arm it to fire after the work is done" pattern actually
	// produces) still terminates in a fixed number of indexed reads. When the
	// scan bound stops us, the result is reported as truncated, which is
	// honest: there may be more, and we did not look further.
	pendingReminderMaxScan = 500
)

// collectPendingReminders fills the pending-reminder window, paging past
// reminders whose items are in a terminal state.
//
// Terminal-item reminders are FILTERED, never acked — see the ack handler for
// why. That filtering happens here rather than in SQL because terminality is
// schema-defined, and it is the reason this function exists at all: without
// paging, a bounded query plus an above-the-query filter is a starvation, and
// that is precisely the defect this replaced.
func (s *Server) collectPendingReminders(workspaceID string, scope store.PendingReminderScope, ctxMap map[string]doneContext) ([]*models.PendingReminder, bool, error) {
	return s.collectPendingRemindersBounded(workspaceID, scope, ctxMap, pendingReminderWindow, pendingReminderMaxScan)
}

// collectPendingRemindersBounded is the paging loop with its bounds injected,
// so a test can drive the case the production constants make impractical to
// build: a window that fills PART WAY through a page. Reaching that with a
// window of 50 needs ~75 rows in a specific terminal pattern; with a window of
// 3 it is four rows. Same split, and the same reason, as the store's arbiter
// and isolation seams.
func (s *Server) collectPendingRemindersBounded(workspaceID string, scope store.PendingReminderScope, ctxMap map[string]doneContext, window, maxScan int) ([]*models.PendingReminder, bool, error) {
	var out []*models.PendingReminder
	scanned := 0

	for len(out) < window && scanned < maxScan {
		page, more, err := s.store.ListPendingReminders(workspaceID, scope, window, scanned)
		if err != nil {
			return nil, false, err
		}
		if len(page) == 0 {
			// Source exhausted with room to spare: nothing was truncated.
			return out, false, nil
		}
		scanned += len(page)
		filledMidPage := false
		for i, pr := range page {
			if isItemDone(pr.ItemFields, pr.CollectionID, ctxMap) {
				continue
			}
			out = append(out, pr)
			if len(out) == window {
				// Rows AFTER this one in the page are pending reminders the
				// caller is not being shown, so the set is truncated even if
				// this was the last page (codex round 11). Reporting `more`
				// alone said "you have seen everything" while unread rows sat
				// in the very page we stopped reading.
				filledMidPage = i < len(page)-1
				break
			}
		}
		if filledMidPage {
			return out, true, nil
		}
		if !more {
			// We read to the end of the set. Whatever we have is all there is,
			// even if it is short of the window.
			return out, false, nil
		}
	}
	// Either the window filled or the scan bound stopped us; in both cases
	// rows remain unread, so say so.
	return out, true, nil
}
