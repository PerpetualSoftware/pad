package store

import (
	"database/sql"
	"math"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// A resolved occurrence's offset snapshot (TASK-4714): the role offsets in
// force when it was resolved, read back for history in place of the
// schedule's current offsets. Writers: stampOccurrenceRolesTx and the bundle
// import.

var occurrenceRoleColumnNames = []string{"roles_target_days", "roles_target_secs", "roles_earliest_days", "roles_earliest_secs",
	"roles_deadline_days", "roles_deadline_secs", "roles_deadline_behavior", "roles_occupied_days", "roles_occupied_secs"}

// occurrenceRolesSelect lists the snapshot columns of alias, in the order
// occurrenceRolesScan.dest scans them.
func occurrenceRolesSelect(alias string) string {
	out := ""
	for i, c := range occurrenceRoleColumnNames {
		if i > 0 {
			out += ", "
		}
		out += alias + "." + c
	}
	return out
}

type occurrenceRolesScan struct {
	targetDays, targetSecs, earliestDays, earliestSecs sql.NullInt64
	deadlineDays, deadlineSecs, occupiedDays, occupied sql.NullInt64
	behavior                                           sql.NullString
}

func (o *occurrenceRolesScan) dest() []any {
	return []any{&o.targetDays, &o.targetSecs, &o.earliestDays, &o.earliestSecs, &o.deadlineDays, &o.deadlineSecs, &o.behavior, &o.occupiedDays, &o.occupied}
}

// roles is the snapshot, nil for an unstamped row (moved, or a row no writer
// has stamped). Outcome and origin are left empty: they stay the schedule's.
func (o *occurrenceRolesScan) roles() *scheduleRoles {
	if !o.targetDays.Valid || !o.targetSecs.Valid {
		return nil
	}
	opt := func(v sql.NullInt64) *int {
		if !v.Valid {
			return nil
		}
		return ptr(int(v.Int64))
	}
	r := &scheduleRoles{TargetDays: int(o.targetDays.Int64), TargetSecs: int(o.targetSecs.Int64),
		EarliestDays: opt(o.earliestDays), EarliestSecs: opt(o.earliestSecs), DeadlineDays: opt(o.deadlineDays), DeadlineSecs: opt(o.deadlineSecs),
		OccupiedDays: opt(o.occupiedDays), OccupiedSecs: opt(o.occupied)}
	if o.behavior.Valid {
		r.DeadlineBehavior = ptr(o.behavior.String)
	}
	return r
}

// validateCarriedOccurrenceRoles checks a bundle's snapshot against the
// columns CHECKs, so a bad one drops its schedule rather than failing the
// import mid-transaction.
func validateCarriedOccurrenceRoles(r *models.ScheduleOccurrenceRolesExport, state string) error {
	if r == nil {
		return nil
	}
	if state == "moved" {
		return dropf("occurrence_roles", "state")
	}
	for _, pair := range [][2]*int{{r.EarliestDays, r.EarliestSecs}, {r.DeadlineDays, r.DeadlineSecs}, {r.OccupiedDays, r.OccupiedSecs}} {
		if (pair[0] == nil) != (pair[1] == nil) {
			return dropf("occurrence_roles", "pairing")
		}
	}
	if (r.DeadlineDays == nil) != (r.DeadlineBehavior == nil) {
		return dropf("occurrence_roles", "pairing")
	}
	if r.DeadlineBehavior != nil && !oneOf(*r.DeadlineBehavior, []string{"lapses", "stays_owed"}) {
		return dropf("occurrence_roles", "enum")
	}
	for _, n := range []*int{&r.TargetDays, &r.TargetSecs, r.EarliestDays, r.EarliestSecs, r.DeadlineDays, r.DeadlineSecs, r.OccupiedDays, r.OccupiedSecs} {
		if n != nil && (int64(*n) < math.MinInt32 || int64(*n) > math.MaxInt32) {
			return dropf("occurrence_roles", "range")
		}
	}
	return nil
}

// historyOffsets is the offsets a seed's roles resolve from: the snapshot for
// a resolved history row that has one, the schedule's current offsets
// otherwise. Outcome and origin always stay the schedule's.
func historyOffsets(roles scheduleRoles, state string, snap *scheduleRoles) scheduleRoles {
	if snap == nil || !oneOf(state, []string{"done", "skipped", "missed", "lapsed"}) {
		return roles
	}
	out := *snap
	out.Outcome, out.Origin = roles.Outcome, roles.Origin
	return out
}
