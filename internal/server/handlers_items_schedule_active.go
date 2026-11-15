package server

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const scheduleActiveMessage = "This item has a recurring schedule. Closing it stops its occurrences until it is reopened. Retry with force to close it anyway."

type scheduleActiveDetails struct {
	ScheduleID string  `json:"schedule_id"`
	NextAt     *string `json:"next_at"`
}

type scheduleActiveError struct{ details scheduleActiveDetails }

func (e *scheduleActiveError) Error() string { return scheduleActiveMessage }

// runItemClosingGuards preserves open_children precedence while including every
// warning that a single force retry will override, using the same transaction.
func (s *Server) runItemClosingGuards(tx *sql.Tx, ctx openChildrenGuardContext) error {
	children, err := s.runOpenChildrenGuard(tx, ctx)
	if err != nil {
		return err
	}
	err = s.runScheduleActiveGuard(tx, ctx)
	if children == nil {
		return err
	}
	if err != nil {
		var active *scheduleActiveError
		if !errors.As(err, &active) {
			return err
		}
		children.ScheduleActive = &active.details
	}
	return &openChildrenGuardError{details: children}
}

// Only item updates use this second guard; move/copy retain their own schedule handling.
func (s *Server) runScheduleActiveGuard(tx *sql.Tx, ctx openChildrenGuardContext) error {
	key, terminals := models.TerminalValuesForDoneField(ctx.parentSchema, ctx.parentSettings)
	next, ok := ctx.newFieldMap[key].(string)
	if !ok || !valueInSet(next, terminals) || valueInSet(extractFieldString(ctx.currentFieldsJS, key), terminals) {
		return nil
	}
	sc, err := s.store.LiveRecurringScheduleTx(tx, ctx.workspaceID, ctx.itemID)
	if err != nil {
		return err
	}
	if sc == nil {
		return nil
	}
	return &scheduleActiveError{details: scheduleActiveDetails{ScheduleID: sc.ID, NextAt: sc.NextAt}}
}

func writeScheduleActiveError(w http.ResponseWriter, err error) bool {
	var active *scheduleActiveError
	if !errors.As(err, &active) {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]any{"code": "schedule_active", "message": scheduleActiveMessage, "details": active.details}})
	return true
}
