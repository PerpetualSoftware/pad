package store

import (
	"fmt"
	"sort"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// BootstrapAgendaStaleAfter is how long an occurrence may stay unresolved and
// still be listed in the bootstrap agenda; past it, it collapses into a count
// (TASK-4237).
const BootstrapAgendaStaleAfter = 14 * 24 * time.Hour

// BootstrapAgendaRow is one schedule's outstanding occurrence as the bootstrap
// agenda shows it.
type BootstrapAgendaRow struct {
	AgendaSchedule
	Due    time.Time // the instant it is due, the moved instant for a moved one
	Wall   string    // the occurrence's wall, a date for an all-day one
	AllDay bool
	// MissedCount is set by ReadMissed, for the rows that are shown.
	MissedCount int
}

// BootstrapAgendaParams names whose agenda it is.
type BootstrapAgendaParams struct {
	WorkspaceID string
	UserID      string
	Agent       bool // the caller is an agent principal
	Scope       PendingReminderScope
	Loc         *time.Location // the caller's zone
	Now         time.Time
}

// BootstrapAgendaSet is the caller's open occurrences, each list oldest first.
type BootstrapAgendaSet struct {
	DueToday []BootstrapAgendaRow
	Carried  []BootstrapAgendaRow // due before today, at most 14 days ago
	Stale    []BootstrapAgendaRow // due more than 14 days ago
	// Truncated is true when the scan cap stopped the read.
	Truncated bool
}

// bootstrapAgendaScan bounds the schedules one read looks at.
const bootstrapAgendaScan = 1000

// ReadMissed sets MissedCount: the occurrences that passed unresolved after the
// outstanding one (the same count the schedule view reports).
func (r *BootstrapAgendaRow) ReadMissed(loc *time.Location, now time.Time) {
	if n, _, err := ScheduleMissed(r.Schedule, loc, now); err == nil {
		r.MissedCount = n
	}
}

// BootstrapAgenda reads the caller's agenda for the bootstrap: for each open
// schedule the caller receives (the item's assignee, else its creator) whose
// audience includes the caller's kind, the one outstanding occurrence, listed
// when it is due today or earlier. An occurrence that is resolved done or
// skipped is gone (the schedule re-arms to its next one), and a moved one is due
// at its new time; a snooze re-fires and resolves nothing, so it stays listed.
func (s *Store) BootstrapAgenda(p BootstrapAgendaParams) (*BootstrapAgendaSet, error) {
	loc := p.Loc
	if loc == nil {
		loc = time.UTC
	}
	if p.Scope.CollectionIDs != nil && len(p.Scope.CollectionIDs) == 0 && len(p.Scope.ItemIDs) == 0 {
		return &BootstrapAgendaSet{}, nil
	}
	local := p.Now.In(loc)
	startToday := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	startTomorrow := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, loc)

	audiences := []any{"human", "both"}
	if p.Agent {
		audiences = []any{"agent", "both"}
	}
	scopeSQL, scopeArgs := scopeClause("i", p.Scope)
	open, openArgs := s.nonTerminalFilter(p.WorkspaceID, "i")
	// The stored instants are early or late by up to a day and a half against the
	// caller's reading (a floating or all-day occurrence), so the bound is padded
	// and the exact test is made below.
	const f = "2006-01-02T15:04:05Z"
	bound := startTomorrow.UTC().Add(36 * time.Hour).Format(f)
	args := []any{p.WorkspaceID, audiences[0], audiences[1], bound, bound, p.WorkspaceID, p.UserID}
	args = append(append(args, scopeArgs...), openArgs...)
	rows, err := s.db.Query(s.q(`SELECT `+scheduleColumns+` FROM item_schedules
		WHERE workspace_id = ? AND ended_at IS NULL AND next_at IS NOT NULL AND audience IN (?, ?)
			AND (next_at < ? OR next_override_at < ?) AND item_id IN (
			SELECT i.id FROM items i JOIN collections c ON c.id = i.collection_id
			WHERE i.workspace_id = ? AND i.deleted_at IS NULL AND c.deleted_at IS NULL
				AND (CASE WHEN i.assigned_user_id IS NULL OR i.assigned_user_id = '' THEN i.created_by_user_id ELSE i.assigned_user_id END) = ?`+scopeSQL+` AND `+open+`)
		ORDER BY next_at, item_schedules.id LIMIT ?`), append(args, bootstrapAgendaScan+1)...)
	if err != nil {
		return nil, fmt.Errorf("list schedules for bootstrap agenda: %w", err)
	}
	defer rows.Close()
	var found []AgendaSchedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("scan bootstrap agenda schedule: %w", err)
		}
		found = append(found, AgendaSchedule{Schedule: sc})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	set := &BootstrapAgendaSet{}
	if set.Truncated = len(found) > bootstrapAgendaScan; set.Truncated {
		found = found[:bootstrapAgendaScan]
	}
	if len(found) == 0 {
		return set, nil
	}
	moved, err := s.movedWalls(found)
	if err != nil {
		return nil, err
	}
	if err := s.fillAgendaItemFacts(found); err != nil {
		return nil, err
	}
	for _, as := range found {
		sc := as.Schedule
		if sc.NextWall == nil {
			continue
		}
		wall, err := schedule.ParseWall(*sc.NextWall)
		if err != nil {
			continue // a stored wall that will not parse is not listed
		}
		due, ok, err := OutstandingInstant(sc, loc)
		if err != nil || !ok {
			continue
		}
		switch {
		case sc.NextOverrideAt != nil && movedStill(moved, sc):
			t, err := time.Parse(time.RFC3339, *sc.NextOverrideAt)
			if err != nil {
				continue
			}
			due = t
		case wall.AllDay:
			due = schedule.Resolve(loc, wall) // a date, read in the caller's zone
		}
		row := BootstrapAgendaRow{AgendaSchedule: as, Due: due, Wall: *sc.NextWall, AllDay: wall.AllDay}
		switch {
		case !due.Before(startTomorrow):
		case !due.Before(startToday):
			set.DueToday = append(set.DueToday, row)
		case p.Now.Sub(due) > BootstrapAgendaStaleAfter:
			set.Stale = append(set.Stale, row)
		default:
			set.Carried = append(set.Carried, row)
		}
	}
	for _, list := range [][]BootstrapAgendaRow{set.DueToday, set.Carried, set.Stale} {
		sort.SliceStable(list, func(i, j int) bool {
			a, b := list[i], list[j]
			switch {
			case !a.Due.Equal(b.Due):
				return a.Due.Before(b.Due)
			case a.ItemRef != b.ItemRef:
				return a.ItemRef < b.ItemRef
			}
			return a.Schedule.ID < b.Schedule.ID
		})
	}
	return set, nil
}

// movedStill says the override on sc is its MOVE. A snooze after a move replaces
// the override and leaves the moved row, so the row counts only while the
// schedule has not been written since: a later write (a snooze, or an edit) reads
// as not moved, and the occurrence is listed at its grid time, which errs toward
// showing it. The stamps are whole seconds, so a snooze in the same second as the
// move still reads as the move; the occurrence then waits for the snoozed time,
// which is when it re-fires anyway.
func movedStill(moved map[[2]string]string, sc *models.ItemSchedule) bool {
	at, ok := moved[[2]string{sc.ID, *sc.NextWall}]
	return ok && sc.UpdatedAt <= at
}

// movedWalls maps (schedule, wall) to the stamp of its MOVED history row, for the
// schedules that hold an override. A move and a snooze both set the override;
// only a move writes the row, and only a move defers the occurrence.
func (s *Store) movedWalls(found []AgendaSchedule) (map[[2]string]string, error) {
	var ids []any
	for _, as := range found {
		if as.Schedule.NextOverrideAt != nil {
			ids = append(ids, as.Schedule.ID)
		}
	}
	out := map[[2]string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(s.q(`SELECT schedule_id, occurrence_wall, resolved_at FROM schedule_occurrences
		WHERE state = 'moved' AND schedule_id IN (`+placeholders(len(ids))+`)`), ids...)
	if err != nil {
		return nil, fmt.Errorf("read moved occurrences: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, wall, at string
		if err := rows.Scan(&id, &wall, &at); err != nil {
			return nil, fmt.Errorf("scan moved occurrence: %w", err)
		}
		out[[2]string{id, wall}] = at
	}
	return out, rows.Err()
}
