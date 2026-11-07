package store

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// The store half of the commitment contract (TASK-4351): the roles reader, the
// native-role write door, the deadline renegotiation and the journal steps that
// answer a late on-time report and an assignee change. Nothing here checks who
// may call it; the handler layer owns authority.

const (
	correctionReason = "Reported on time"
	rolesWriteReason = "Commitment roles updated"
)

// roleResolution is one role's value for the outstanding occurrence.
type roleResolution struct {
	value    string    // a date for an all-day role, else an RFC3339 UTC instant
	expires  time.Time // when the role stops being in the future: next local midnight for a date
	dateOnly bool
}

// resolveRoleValue is the occurrence's seed wall displaced by the role's
// offset, or the override that replaces it, read in loc. It is the derivation
// the deadline evaluator uses, so a reader and the evaluator cannot disagree.
func resolveRoleValue(loc *time.Location, nextWall string, days, secs int, override *string) (roleResolution, error) {
	wall, err := schedule.ParseWall(nextWall)
	if err != nil {
		return roleResolution{}, err
	}
	civil := time.Date(wall.Year, wall.Month, wall.Day, wall.Hour, wall.Minute, wall.Second, 0, time.UTC).AddDate(0, 0, days).Add(time.Duration(secs) * time.Second)
	w := wallOf(civil)
	w.AllDay = wall.AllDay && secs == 0
	dateOnly := w.AllDay
	at := schedule.Resolve(loc, w).UTC()
	if override != nil {
		dateOnly = len(*override) == 10
		if dateOnly {
			if w, err = schedule.ParseWall(*override); err != nil {
				return roleResolution{}, err
			}
			at = schedule.Resolve(loc, w).UTC()
		} else {
			if at, err = time.Parse(time.RFC3339, *override); err != nil {
				return roleResolution{}, err
			}
			at = at.UTC()
		}
	}
	if dateOnly {
		end := wallOf(time.Date(w.Year, w.Month, w.Day, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1))
		end.AllDay = true
		return roleResolution{value: w.String(), expires: schedule.Resolve(loc, end).UTC(), dateOnly: true}, nil
	}
	return roleResolution{value: at.Format(time.RFC3339Nano), expires: at}, nil
}

func (r roleResolution) metBy(done time.Time) bool {
	if r.dateOnly {
		return done.Before(r.expires)
	}
	return !done.After(r.expires)
}

func scheduleRolesFromExport(r *models.ScheduleRolesExport) scheduleRoles {
	return scheduleRoles{TargetDays: r.TargetDays, TargetSecs: r.TargetSecs, EarliestDays: r.EarliestDays, EarliestSecs: r.EarliestSecs,
		DeadlineDays: r.DeadlineDays, DeadlineSecs: r.DeadlineSecs, DeadlineBehavior: r.DeadlineBehavior,
		OccupiedDays: r.OccupiedDays, OccupiedSecs: r.OccupiedSecs, Outcome: r.Outcome, Origin: r.RolesOrigin}
}

// scheduleRolesOf is the roles a schedule reads as: the columns of a native
// row, legacyRoles of a legacy_backfill row.
func (s *Store) scheduleRolesOf(q Queryer, sc *models.ItemSchedule) (scheduleRoles, error) {
	stored, err := s.scheduleRolesQ(q, sc.ID)
	if err != nil {
		return scheduleRoles{}, err
	}
	if stored.RolesOrigin != "legacy_backfill" {
		return scheduleRolesFromExport(stored), nil
	}
	return legacyRoles(sc.Kind, sc.Dtstart, derefStr(sc.Dtend))
}

// scheduleViewZone is the zone a schedule's walls are read in: its own, or for
// a floating one the recipient's.
func (s *Store) scheduleViewZone(q Queryer, sc *models.ItemSchedule) (*time.Location, error) {
	if sc.Floating {
		return s.scheduleRecipientZone(q, sc.ItemID)
	}
	loc, err := time.LoadLocation(derefStr(sc.Tz))
	if err != nil {
		return nil, fmt.Errorf("load schedule zone: %w", err)
	}
	return loc, nil
}

// outstandingKey is the key of the schedule's outstanding occurrence, "" when it
// has none.
func outstandingKey(sc *models.ItemSchedule, loc *time.Location) (string, error) {
	at, ok, err := OutstandingInstant(sc, loc)
	if err != nil || !ok || sc.NextWall == nil {
		return "", err
	}
	return OccurrenceKey(sc, NormalizeInstant(at), *sc.NextWall), nil
}

func (s *Store) occurrenceOverrides(q Queryer, scheduleID, key string) (map[string]string, error) {
	rows, err := q.Query(s.q(`SELECT role,value_at FROM schedule_occurrence_overrides WHERE schedule_id=? AND occurrence_key=?`), scheduleID, key)
	if err != nil {
		return nil, fmt.Errorf("read role overrides: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var role, value string
		if err := rows.Scan(&role, &value); err != nil {
			return nil, err
		}
		out[role] = value
	}
	return out, rows.Err()
}

func overrideFor(m map[string]string, role string) *string {
	if v, ok := m[role]; ok {
		return &v
	}
	return nil
}

// GetCommitment reads an item's schedule as roles: the effective value of each
// role for the outstanding occurrence with that occurrence's overrides applied,
// the occurrence key, and the journal rows that carry the deadline's history (the
// recorded misses, corrections, renegotiations and assignee changes). Legacy rows read through
// legacyRoles, in the same shape as native ones. ErrScheduleNotFound when the
// item has no schedule.
func (s *Store) GetCommitment(workspaceID, itemID string) (*models.Commitment, error) {
	sc, err := s.GetSchedule(workspaceID, itemID)
	if err != nil {
		return nil, err
	}
	if sc == nil {
		return nil, ErrScheduleNotFound
	}
	roles, err := s.scheduleRolesOf(s.db, sc)
	if err != nil {
		return nil, err
	}
	out := &models.Commitment{ScheduleID: sc.ID, ItemID: itemID, Journal: []models.CommitmentChange{}}
	out.Roles = models.CommitmentRoles{DeadlineBehavior: roles.DeadlineBehavior, Outcome: roles.Outcome, RolesOrigin: roles.Origin}
	if sc.EndedAt == nil && sc.NextWall != nil {
		loc, err := s.scheduleViewZone(s.db, sc)
		if err != nil {
			return nil, err
		}
		if out.OccurrenceKey, err = outstandingKey(sc, loc); err != nil {
			return nil, err
		}
		overrides, err := s.occurrenceOverrides(s.db, sc.ID, out.OccurrenceKey)
		if err != nil {
			return nil, err
		}
		for _, role := range []struct {
			name       string
			days, secs *int
			into       **string
		}{
			{"target", &roles.TargetDays, &roles.TargetSecs, &out.Roles.Target},
			{"earliest", roles.EarliestDays, roles.EarliestSecs, &out.Roles.Earliest},
			{"deadline", roles.DeadlineDays, roles.DeadlineSecs, &out.Roles.Deadline},
			{"occupied_until", roles.OccupiedDays, roles.OccupiedSecs, &out.Roles.OccupiedUntil},
		} {
			if role.days == nil || role.secs == nil {
				continue
			}
			r, err := resolveRoleValue(loc, *sc.NextWall, *role.days, *role.secs, overrideFor(overrides, role.name))
			if err != nil {
				return nil, fmt.Errorf("resolve %s: %w", role.name, err)
			}
			v := r.value
			*role.into = &v
		}
	}
	changes, err := s.scheduleChangesQ(s.db, workspaceID, sc.ID)
	if err != nil {
		return nil, err
	}
	for _, c := range changes {
		// Role decides: an occurrence the schedule passed over is also journaled
		// missed_recorded, with no role, and is not the deadline's history.
		deadlineRow := c.Role != nil && *c.Role == "deadline"
		correction := c.ChangeType == "done" && deadlineRow && c.ParentChangeID != nil
		move := c.ChangeType == "move" && deadlineRow && c.ReasonSource == "user"
		assignee := c.ChangeType == "update" && c.Role == nil && c.BeforeJSON == nil &&
			c.ReasonSource == "system" && strings.HasPrefix(c.Reason, "Assignee changed from ") && strings.Contains(c.Reason, " to ")
		if !(c.ChangeType == "missed_recorded" && deadlineRow) && !correction && !move && !assignee {
			continue
		}
		out.Journal = append(out.Journal, models.CommitmentChange{ID: c.ID, ChangeType: c.ChangeType, OccurrenceKey: c.OccurrenceKey, Role: c.Role,
			AfterJSON: c.AfterJSON, Actor: c.Actor, Reason: c.Reason, ReasonSource: c.ReasonSource, RootChangeID: c.RootChangeID,
			ParentChangeID: c.ParentChangeID, RecordedAt: c.RecordedAt})
	}
	return out, nil
}

func rolesFromInput(in models.CommitmentRolesInput) (scheduleRoles, error) {
	r := scheduleRoles{TargetDays: in.TargetDays, TargetSecs: in.TargetSecs, EarliestDays: in.EarliestDays, EarliestSecs: in.EarliestSecs,
		DeadlineDays: in.DeadlineDays, DeadlineSecs: in.DeadlineSecs, DeadlineBehavior: in.DeadlineBehavior,
		OccupiedDays: in.OccupiedDays, OccupiedSecs: in.OccupiedSecs, Outcome: in.Outcome, Origin: "native"}
	for _, pair := range []struct {
		name string
		a, b *int
	}{{"earliest", in.EarliestDays, in.EarliestSecs}, {"deadline", in.DeadlineDays, in.DeadlineSecs}, {"occupied_until", in.OccupiedDays, in.OccupiedSecs}} {
		if (pair.a == nil) != (pair.b == nil) {
			return r, fieldErr(pair.name, "combination", pair.name+" days and secs are set together")
		}
	}
	if (in.DeadlineDays == nil) != (in.DeadlineBehavior == nil) {
		return r, fieldErr("deadline_behavior", "combination", "a deadline and its behavior are set together")
	}
	if in.DeadlineBehavior != nil && !oneOf(*in.DeadlineBehavior, []string{"lapses", "stays_owed"}) {
		return r, fieldErr("deadline_behavior", "enum", "deadline_behavior must be lapses or stays_owed")
	}
	for _, n := range []*int{&in.TargetDays, &in.TargetSecs, in.EarliestDays, in.EarliestSecs, in.DeadlineDays, in.DeadlineSecs, in.OccupiedDays, in.OccupiedSecs} {
		if n != nil && (int64(*n) < math.MinInt32 || int64(*n) > math.MaxInt32) {
			return r, fieldErr("roles", "range", "a role offset is out of range")
		}
	}
	if hasNUL(in.Outcome) {
		return r, fieldErr("outcome", "enum", "outcome cannot contain a NUL")
	}
	return r, nil
}

func (s *Store) writeNativeRolesTx(tx *sql.Tx, id string, r scheduleRoles) error {
	_, err := tx.Exec(s.q(`UPDATE item_schedules SET target_days=?,target_secs=?,earliest_days=?,earliest_secs=?,deadline_days=?,deadline_secs=?,deadline_behavior=?,occupied_days=?,occupied_secs=?,outcome=?,roles_origin='native',updated_at=? WHERE id=?`),
		r.TargetDays, r.TargetSecs, r.EarliestDays, r.EarliestSecs, r.DeadlineDays, r.DeadlineSecs, r.DeadlineBehavior, r.OccupiedDays, r.OccupiedSecs, r.Outcome, now(), id)
	if err != nil {
		return fmt.Errorf("write schedule roles: %w", err)
	}
	return nil
}

// CreateCommitmentSchedule creates an item's schedule with native roles: the one
// create path that stores roles_origin 'native'.
func (s *Store) CreateCommitmentSchedule(workspaceID, itemID string, in models.ScheduleCreate, roles models.CommitmentRolesInput) (*models.ItemSchedule, error) {
	r, err := rolesFromInput(roles)
	if err != nil {
		return nil, err
	}
	return s.createScheduleRolesAt(workspaceID, itemID, in, &r, time.Now())
}

// CommitmentHasDeadline reports whether the item's schedule is native and has a
// deadline role: the promise that only its assignee or an owner takes away.
// False for no schedule, a legacy_backfill row, or a native row without a
// deadline.
func (s *Store) CommitmentHasDeadline(workspaceID, itemID string) (bool, error) {
	sc, err := s.GetSchedule(workspaceID, itemID)
	if err != nil || sc == nil {
		return false, err
	}
	roles, err := s.scheduleRolesOf(s.db, sc)
	if err != nil {
		return false, err
	}
	return roles.Origin == "native" && roles.DeadlineDays != nil, nil
}

// sameDeadline reports whether two role sets carry the same deadline offset and
// behavior; a deadline present on one side only is a difference.
func sameDeadline(a, b scheduleRoles) bool {
	eq := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return eq(a.DeadlineDays, b.DeadlineDays) && eq(a.DeadlineSecs, b.DeadlineSecs) &&
		(a.DeadlineBehavior == nil) == (b.DeadlineBehavior == nil) &&
		(a.DeadlineBehavior == nil || *a.DeadlineBehavior == *b.DeadlineBehavior)
}

// SetCommitmentRoles replaces a schedule's role columns and marks them native.
// Writing a legacy_backfill row converts it, and the journal row it writes is the
// only one whose before_json shows roles_origin legacy_backfill beside an
// after_json of native: the deadline evaluator reads that pair as the conversion
// and records no miss for a deadline already past at that moment.
//
// A native schedule that has a deadline keeps it here: a different deadline
// offset or behavior, or no deadline, is ErrHardDeadlineRenegotiate
// (RenegotiateDeadline is the one door that moves it). Other roles change
// freely. A legacy_backfill row has no native renegotiation path, so its first
// write here sets the deadline.
func (s *Store) SetCommitmentRoles(workspaceID, itemID string, roles models.CommitmentRolesInput) (*models.ItemSchedule, error) {
	r, err := rolesFromInput(roles)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("set schedule roles: %w", err)
	}
	defer tx.Rollback()
	if err := s.pinScheduleTx(tx, workspaceID, itemID); err != nil {
		return nil, err
	}
	sc, err := s.getScheduleQ(tx, workspaceID, itemID, false)
	if err != nil {
		return nil, err
	}
	if sc == nil {
		return nil, ErrScheduleNotFound
	}
	cur, err := s.scheduleRolesOf(tx, sc)
	if err != nil {
		return nil, err
	}
	if cur.Origin == "native" && cur.DeadlineDays != nil && !sameDeadline(cur, r) {
		return nil, ErrHardDeadlineRenegotiate
	}
	before, err := s.scheduleSnapshotTx(tx, sc.ID)
	if err != nil {
		return nil, err
	}
	if err := s.writeNativeRolesTx(tx, sc.ID, r); err != nil {
		return nil, err
	}
	after, err := s.scheduleSnapshotTx(tx, sc.ID)
	if err != nil {
		return nil, err
	}
	if err := s.journalScheduleChangeReasonTx(tx, sc.ID, "update", "", "", before, after, "system", rolesWriteReason, "system"); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("set schedule roles: %w", err)
	}
	return s.GetSchedule(workspaceID, itemID)
}

// RenegotiateDeadline moves the outstanding occurrence's deadline to until: a
// 'deadline' override for the occurrence key, and a journal row of change_type
// move, role deadline, reason_source user. A reason is required.
//
// It does not check who may renegotiate: the caller owns authority. The journal
// row's before_json and after_json are {"deadline": value} objects with the old
// and the new deadline and no roles_origin member, so a renegotiation after a
// conversion leaves later misses recorded. A row that is not native with a deadline has
// nothing to renegotiate and is refused.
func (s *Store) RenegotiateDeadline(workspaceID, itemID, actor string, until time.Time, reason string) error {
	return s.renegotiateDeadlineAt(workspaceID, itemID, actor, until, reason, time.Now())
}

func (s *Store) renegotiateDeadlineAt(workspaceID, itemID, actor string, until time.Time, reason string, now time.Time) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fieldErr("reason", "required", "renegotiating a deadline needs a reason")
	}
	if until.IsZero() {
		return fieldErr("until", "required", "renegotiating a deadline needs the new deadline")
	}
	// Validate and journal the value that is stored: the override writer rounds
	// a fractional second up.
	if n, err := time.Parse(time.RFC3339, NormalizeInstant(until)); err == nil {
		until = n
	}
	if !until.After(now) || until.After(now.Add(maxResolveUntil)) {
		return fieldErr("until", "range", "until must be after now and within 366 days")
	}
	if actor == "" {
		actor = "user"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("renegotiate deadline: %w", err)
	}
	defer tx.Rollback()
	if err := s.pinScheduleTx(tx, workspaceID, itemID); err != nil {
		return err
	}
	sc, err := s.getScheduleQ(tx, workspaceID, itemID, false)
	if err != nil {
		return err
	}
	if sc == nil {
		return ErrScheduleNotFound
	}
	roles, err := s.scheduleRolesQ(tx, sc.ID)
	if err != nil {
		return err
	}
	if roles.RolesOrigin != "native" || roles.DeadlineDays == nil {
		return fieldErr("deadline", "combination", "the schedule has no native deadline to renegotiate")
	}
	if sc.EndedAt != nil || sc.NextWall == nil {
		return fieldErr("deadline", "combination", "the schedule has no outstanding occurrence")
	}
	loc, err := s.scheduleViewZone(tx, sc)
	if err != nil {
		return err
	}
	key, err := outstandingKey(sc, loc)
	if err != nil || key == "" {
		return fmt.Errorf("renegotiate deadline: outstanding key: %w", err)
	}
	overrides, err := s.occurrenceOverrides(tx, sc.ID, key)
	if err != nil {
		return err
	}
	prior, err := resolveRoleValue(loc, *sc.NextWall, *roles.DeadlineDays, *roles.DeadlineSecs, overrideFor(overrides, "deadline"))
	if err != nil {
		return err
	}
	// The journal row carries the old and new deadline values, because the
	// override row is overwritten by the next renegotiation.
	before := fmt.Sprintf(`{"deadline":%q}`, prior.value)
	after := fmt.Sprintf(`{"deadline":%q}`, until.UTC().Format(time.RFC3339Nano))
	if err := s.upsertScheduleOverrideTx(tx, sc.ID, key, "deadline", until, nil); err != nil {
		return err
	}
	if err := s.journalScheduleChangeReasonTx(tx, sc.ID, "move", key, "deadline", before, after, actor, reason, "user"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("renegotiate deadline: %w", err)
	}
	return nil
}

// recordOnTimeCorrectionTx answers a recorded miss when a completion reported at
// or before the occurrence's deadline arrives after it: a change_type done row,
// role deadline, that names the latest miss as its parent and shares its root.
// The miss row stays. A schedule with no recorded miss for the key, or whose
// deadline the report missed, writes nothing. The correction is journal-only:
// resolution reads the occurrence rows, never these.
func (s *Store) recordOnTimeCorrectionTx(tx *sql.Tx, sc *models.ItemSchedule, key string, reported time.Time) error {
	rows, err := tx.Query(s.q(`SELECT id,root_change_id,recorded_at FROM schedule_changes WHERE schedule_id=? AND occurrence_key=? AND change_type='missed_recorded' AND role='deadline'`), sc.ID, key)
	if err != nil {
		return fmt.Errorf("read recorded misses: %w", err)
	}
	var missID, rootID string
	var latest time.Time
	for rows.Next() {
		var id, root, recorded string
		if err := rows.Scan(&id, &root, &recorded); err != nil {
			rows.Close()
			return err
		}
		at, err := time.Parse(time.RFC3339Nano, recorded)
		if err != nil {
			rows.Close()
			return fmt.Errorf("parse journal recorded_at: %w", err)
		}
		if missID == "" || at.After(latest) || (at.Equal(latest) && id > missID) {
			missID, rootID, latest = id, root, at
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if missID == "" {
		return nil
	}
	roles, err := s.scheduleRolesQ(tx, sc.ID)
	if err != nil {
		return err
	}
	if roles.RolesOrigin != "native" || roles.DeadlineDays == nil || roles.DeadlineSecs == nil || sc.NextWall == nil {
		return nil
	}
	loc, err := s.scheduleViewZone(tx, sc)
	if err != nil {
		return err
	}
	overrides, err := s.occurrenceOverrides(tx, sc.ID, key)
	if err != nil {
		return err
	}
	deadline, err := resolveRoleValue(loc, *sc.NextWall, *roles.DeadlineDays, *roles.DeadlineSecs, overrideFor(overrides, "deadline"))
	if err != nil {
		return err
	}
	if !deadline.metBy(reported) {
		return nil
	}
	return s.journalScheduleChangeLineageTx(tx, sc.ID, "done", key, "deadline", "", "", "system", correctionReason, "system", missID, rootID)
}

// journalAssigneeChangeTx writes the journal step for an assignee change on the
// item's native schedule with a deadline. It is ONE statement that selects the
// schedule and inserts nothing when the item has none, so an item with no such
// schedule pays that one lookup and no write; UpdateItem calls it only when the
// assignee changed.
func (s *Store) journalAssigneeChangeTx(tx *sql.Tx, itemID, from, to string) error {
	changeID := newID()
	_, err := tx.Exec(s.q(`INSERT INTO schedule_changes (id,workspace_id,item_id,schedule_id,change_type,actor,reason,reason_source,root_change_id,recorded_at)
 SELECT ?,workspace_id,item_id,id,'update','system',?,'system',?,? FROM item_schedules WHERE item_id=? AND roles_origin='native' AND deadline_days IS NOT NULL`),
		changeID, "Assignee changed from "+from+" to "+to, changeID, nowNano(), itemID)
	if err != nil {
		return fmt.Errorf("journal assignee change: %w", err)
	}
	return nil
}
