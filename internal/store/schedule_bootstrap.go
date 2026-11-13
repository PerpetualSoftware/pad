package store

import (
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BootstrapAgendaStaleAfter is how long an occurrence may stay unresolved and
// still be listed in the bootstrap agenda; past it, it collapses into a count
// (TASK-4237).
const BootstrapAgendaStaleAfter = 14 * 24 * time.Hour

// BootstrapAgendaRow is one schedule's outstanding occurrence as the bootstrap
// agenda shows it.
type BootstrapAgendaRow struct {
	AgendaSchedule
	Due           time.Time // the instant it is due, the moved instant for a moved one
	Wall          string    // the occurrence's wall, a date for an all-day one
	Roles         models.CommitmentRoles
	OccurrenceKey string
	AllDay        bool
	// MissedCount is set by ReadMissed, for the rows that are shown.
	MissedCount int
}

// BootstrapAgendaParams names whose agenda it is.
type BootstrapAgendaParams struct {
	WorkspaceID string
	UserID      string
	// Needs due-claim reads exclude quiet occurrences before the shared scan cap.
	ExcludeAcknowledged bool
	Agent               bool // the caller is an agent principal
	Scope               PendingReminderScope
	Loc                 *time.Location // the caller's zone
	Now                 time.Time
}

// BootstrapAgendaSet is the caller's open occurrences, each list oldest first.
type BootstrapAgendaSet struct {
	DueToday []BootstrapAgendaRow
	Carried  []BootstrapAgendaRow // due before today, at most 14 days ago
	Stale    []BootstrapAgendaRow // due more than 14 days ago
	// Truncated is true when the scan cap stopped the read.
	Truncated bool
	// Incomplete marks skipped unreadable schedules, whose group cannot be known.
	Incomplete bool
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
	// Only near-term occurrences belong in this due/carried/stale scan. Any role
	// landing today makes a row due, so the bound admits the seed (next_at), a
	// moved target (next_override_at, next_at stays at the seed), an override of
	// any role on the outstanding occurrence, and a role offset that reaches
	// back before the bound. The role check below then decides membership.
	boundAt := startTomorrow.Add(36 * time.Hour).UTC()
	bound := boundAt.Format(time.RFC3339)
	epoch := "unixepoch(next_at)"
	if s.dialect.Driver() == DriverPostgres {
		epoch = "EXTRACT(EPOCH FROM CAST(next_at AS timestamptz))"
	}
	nearSQL := `(next_at < ? OR next_override_at < ? OR EXISTS (SELECT 1 FROM schedule_occurrence_overrides v
			WHERE v.schedule_id = item_schedules.id AND v.value_at < ?
				AND (v.occurrence_key = next_wall OR v.occurrence_key = next_at || '|' || next_wall))`
	nearArgs := []any{bound, bound, bound}
	for _, role := range []string{"target", "earliest", "deadline", "occupied"} {
		// Day offsets count as 86400s; 72h of slack covers DST and zone shifts.
		nearSQL += ` OR ((COALESCE(` + role + `_days, 0) < 0 OR COALESCE(` + role + `_secs, 0) < 0) AND ` + epoch + ` + COALESCE(` + role + `_days, 0) * 86400.0 + COALESCE(` + role + `_secs, 0) < ?)`
		nearArgs = append(nearArgs, boundAt.Add(72*time.Hour).Unix())
	}
	nearSQL += `)`
	args := append([]any{p.WorkspaceID}, nearArgs...)
	args = append(args, audiences[0], audiences[1], p.WorkspaceID, p.UserID)
	args = append(append(args, scopeArgs...), openArgs...)
	ackFilter := ""
	if p.ExcludeAcknowledged {
		ackFilter = ` AND NOT EXISTS (SELECT 1 FROM item_reminders ar WHERE ar.schedule_id=item_schedules.id AND ar.acked_at IS NOT NULL)`
	}
	rows, err := s.db.Query(s.q(`SELECT `+scheduleColumns+` FROM item_schedules
		WHERE workspace_id = ? AND ended_at IS NULL AND next_at IS NOT NULL`+ackFilter+` AND `+nearSQL+` AND audience IN (?, ?)
			AND item_id IN (
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
	var ackBatch []*models.ItemSchedule
	for _, row := range found {
		ackBatch = append(ackBatch, row.Schedule)
	}
	if err := s.fillScheduleAcknowledgements(s.db, ackBatch); err != nil {
		return nil, err
	}
	if err := s.fillAgendaItemFacts(found, "", nil); err != nil {
		return nil, err
	}
	outstanding, incomplete := s.outstandingAgendaRows(found, loc)
	set.Incomplete = incomplete
	for _, row := range outstanding {
		due := row.Due
		roleToday := false
		for _, v := range []*string{row.Roles.Target, row.Roles.Earliest, row.Roles.Deadline, row.Roles.OccupiedUntil} {
			if v == nil {
				continue
			}
			at, err := agendaRoleInstant(*v, loc)
			if err == nil && !at.Before(startToday) && at.Before(startTomorrow) {
				roleToday = true
			}
		}
		switch {
		case roleToday:
			set.DueToday = append(set.DueToday, row)
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

// outstandingAgendaRows reads each outstanding occurrence at its effective due
// instant. A move defers the claim; a snooze only defers its next notification.
func (s *Store) outstandingAgendaRows(found []AgendaSchedule, loc *time.Location) ([]BootstrapAgendaRow, bool) {
	var out []BootstrapAgendaRow
	incomplete := false
	skip := func(as AgendaSchedule, err error) {
		incomplete = true
		slog.Warn("agenda: schedule unreadable", "schedule_id", as.Schedule.ID, "error_class", fmt.Sprintf("%T", err))
	}
	for _, as := range found {
		gates, err := s.evaluateGatesQ(s.db, as.Schedule.WorkspaceID, as.Schedule.ItemID)
		if err != nil {
			skip(as, err)
			continue
		}
		if gates.state != models.GateStateReleased {
			continue
		}
		e, err := s.AgendaOutstanding(as.Schedule, loc, time.Time{})
		if err != nil {
			skip(as, err)
			continue
		}
		if e == nil {
			continue
		}
		due := e.At
		if e.Roles.Target != nil && len(*e.Roles.Target) == 10 {
			due, err = agendaRoleInstant(*e.Roles.Target, loc)
			if err != nil {
				skip(as, err)
				continue
			}
		}
		out = append(out, BootstrapAgendaRow{AgendaSchedule: as, Due: due, Wall: e.Wall.String(), AllDay: e.Roles.Target != nil && len(*e.Roles.Target) == 10, Roles: e.Roles, OccurrenceKey: e.OccurrenceKey})
	}
	return out, incomplete
}
