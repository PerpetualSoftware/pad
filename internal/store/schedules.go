package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
	"github.com/PerpetualSoftware/pad/internal/usertz"
)

// Item schedules (TASK-4102, DOC-4098): the time trigger attached to an item.
//
// A schedule is the RULE; the one outstanding occurrence is a MANAGED row in
// item_reminders (schedule_id set), so the existing tick, arbiter, pending list
// and outbox carry it unchanged. See migration 162 for the shape. This file is
// the storage half: CRUD, the single validation door, and the arming rules.
// Firing a managed row, and the 409 that protects it at the HTTP door, are
// separate units.
//
// LOCK ORDER (Postgres, a hard rule): every writer that touches both rows takes
// the item_schedules row BEFORE the item_reminders row. SQLite serialises.

// Enumerations, mirrored from the CHECK constraints in migration 162.
var (
	scheduleKinds     = []string{"deadline_hard", "deadline_soft", "check", "window"}
	scheduleAnchors   = []string{"calendar", "completion"}
	scheduleAudiences = []string{"human", "agent", "both"}
)

// maxScheduleLeadMinutes is 31 days, the CHECK constraint's bound.
const maxScheduleLeadMinutes = 44640

// allDayFireHour is the local hour an all-day occurrence fires at (DOC-4098
// Timezones); the lead is subtracted from it. Occurrence.At for an all-day
// occurrence is local midnight and is not reinterpreted.
const allDayFireHour = 9

var (
	// ErrScheduleExists is CreateSchedule's answer when the item already has one.
	ErrScheduleExists = errors.New("item already has a schedule")
	// ErrHardDeadlineRenegotiate is the answer to a write that would move or
	// loosen a native commitment's deadline outside a renegotiation: a hard
	// deadline changes only through RenegotiateDeadline, which records who and why.
	ErrHardDeadlineRenegotiate = errors.New("a hard deadline changes only by renegotiation")
	// ErrDeadlineDeleteAuthority is DeleteScheduleChecked's answer when the actor
	// may not take a native schedule's deadline away.
	ErrDeadlineDeleteAuthority = errors.New("a schedule with a hard deadline is removed only by its assignee or an owner")
	// ErrDeadlineDeleteChanged refuses a delete based on a stale assignee.
	ErrDeadlineDeleteChanged = errors.New("the commitment changed; re-preview before applying")
	// ErrScheduleItemGone is the answer when the item is not a live item of the
	// workspace; missing, archived and foreign are indistinguishable on purpose
	// (the existence-oracle posture ErrReminderItemGone takes).
	ErrScheduleItemGone = errors.New("schedule item is not live in this workspace")
	// ErrScheduleExhausted is CreateSchedule's answer when the rule has no
	// occurrence at or after now. An update that exhausts a schedule ends it
	// instead.
	ErrScheduleExhausted = errors.New("schedule has no occurrence at or after now")
	// ErrReminderManaged is the answer when a caller tries to change a reminder
	// that belongs to a schedule. Change or delete the schedule instead.
	ErrReminderManaged = errors.New("reminder is managed by its item's schedule")
)

// ScheduleFieldError is a refusal of one of the store-owned field checks (the
// enums, the lead range, the window shape). Code is one of enum, range,
// combination, required, the same vocabulary internal/schedule uses where the
// meaning is the same.
type ScheduleFieldError struct {
	Field   string
	Code    string
	Message string
}

func (e *ScheduleFieldError) Error() string {
	return fmt.Sprintf("schedule %s: %s", e.Field, e.Message)
}

func fieldErr(field, code, msg string) error {
	return &ScheduleFieldError{Field: field, Code: code, Message: msg}
}

// ScheduleErrorCode names an error from compiling or advancing a rule as the
// stable code item_schedules.error_code stores. The full set is the ten
// schedule.Code values (syntax, unknown_part, duplicate_part, missing_part,
// range, combination, unsupported, unsatisfiable, zone, wall) and the five cap
// codes (cap_scan, cap_stall, cap_time, cap_window, cap_instances). The fire path
// compiles the stored rule, so it can store a Code or a cap code Compile raises;
// cap_window and cap_instances come only from expanding a window, which a later
// writer may do. It is "" for any other error, and never carries third-party
// text.
func ScheduleErrorCode(err error) string {
	var m *schedule.MalformedError
	if errors.As(err, &m) {
		return string(m.Code)
	}
	var c *schedule.CapError
	if errors.As(err, &c) {
		return "cap_" + string(c.Kind)
	}
	return ""
}

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// scheduleSpecInput is the part of a schedule that compileScheduleSpec checks.
type scheduleSpecInput struct {
	Dtstart  string
	Tz       string
	Floating bool
	Rrule    string
	RDates   []string
	ExDates  []string
}

// compileScheduleSpec is the ONLY door a schedule's timing text goes through,
// called by CreateSchedule and UpdateSchedule, so no caller can store a rule
// that does not compile. It returns the Compiled spec, whose Spec() holds the
// canonical rule and walls that are stored. Errors are *schedule.MalformedError
// or *schedule.CapError, untouched, so the HTTP edge can map them by code.
func compileScheduleSpec(in scheduleSpecInput) (*schedule.Compiled, error) {
	start, err := schedule.ParseWall(in.Dtstart)
	if err != nil {
		return nil, err
	}
	rule := ""
	if in.Rrule != "" {
		if rule, err = schedule.NormalizeRule(in.Rrule); err != nil {
			return nil, err
		}
	}
	parse := func(ws []string) ([]schedule.Wall, error) {
		var out []schedule.Wall
		for _, w := range ws {
			p, err := schedule.ParseWall(w)
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		}
		return out, nil
	}
	rd, err := parse(in.RDates)
	if err != nil {
		return nil, err
	}
	ex, err := parse(in.ExDates)
	if err != nil {
		return nil, err
	}
	return schedule.Compile(schedule.Spec{
		Start: start, Zone: in.Tz, Floating: in.Floating, Rule: rule, RDates: rd, ExDates: ex,
	})
}

// ScheduleFloor is the shortest gap allowed between two fire instants of one
// schedule (TASK-4236). A schedule under it is refused at create and update,
// never thinned. Display expansion and stored schedules are not limited by it.
const ScheduleFloor = 15 * time.Minute

// checkScheduleTiming runs at the user write paths only (create, and an update
// that names timing): copy, import, restore and migrations never call it, so a
// schedule stored before these checks still expands and fires (TASK-4236).
//
// One walk of the compiled schedule gives all three answers, and this is the
// order they win in: a cap hit with nothing found is the cap's own error (the
// schedule could not be read, which is not the same as "never occurs"); then no
// occurrence at all from dtstart, "this schedule never occurs"; then a gap under
// ScheduleFloor. A rule whose occurrences have all passed from NOW still has
// occurrences from dtstart and so passes here; the create path's exhausted check
// and an update that ends the schedule decide that, as before.
func (s *Store) checkScheduleTiming(c *schedule.Compiled) error {
	gap, n, err := c.MinGap(ScheduleFloor, s.scheduleWriteLimits)
	if err != nil {
		return err
	}
	spec := c.Spec()
	field := "rrule"
	if spec.Rule == "" {
		field = "rdates"
	}
	if n == 0 {
		return fieldErr(field, "never_occurs", "this schedule never occurs")
	}
	if n < 2 || gap >= ScheduleFloor {
		return nil
	}
	if len(spec.RDates) > 0 && spec.Rule != "" {
		// The RDATEs are named unless the rule alone is measurable and already
		// under the floor: a rule that cannot be measured by itself (it never
		// fires and needs an RDATE to compile, or a cap hit with nothing found)
		// contributed no instants of its own.
		alone := spec
		alone.RDates = nil
		field = "rdates"
		if rc, cerr := schedule.Compile(alone); cerr == nil {
			if g, rn, rerr := rc.MinGap(ScheduleFloor, s.scheduleWriteLimits); rerr == nil && rn >= 2 && g < ScheduleFloor {
				field = "rrule"
			}
		}
	}
	return fieldErr(field, "floor", "schedules fire at most every 15 minutes")
}

// validateScheduleFields checks the store-owned fields. dtend is the canonical
// wall text or "" when absent; the caller has already compiled dtstart.
func validateScheduleFields(kind, anchor, audience string, lead int, start schedule.Wall, dtend string) (schedule.Wall, bool, error) {
	if !oneOf(kind, scheduleKinds) {
		return schedule.Wall{}, false, fieldErr("kind", "enum", "kind must be one of deadline_hard, deadline_soft, check, window")
	}
	if !oneOf(anchor, scheduleAnchors) {
		return schedule.Wall{}, false, fieldErr("anchor", "enum", "anchor must be calendar or completion")
	}
	if !oneOf(audience, scheduleAudiences) {
		return schedule.Wall{}, false, fieldErr("audience", "enum", "audience must be human, agent or both")
	}
	if lead < 0 || lead > maxScheduleLeadMinutes {
		return schedule.Wall{}, false, fieldErr("lead_minutes", "range", fmt.Sprintf("lead_minutes must be 0..%d", maxScheduleLeadMinutes))
	}
	switch {
	case kind == "window" && dtend == "":
		return schedule.Wall{}, false, fieldErr("dtend", "required", "a window needs a dtend")
	case kind != "window" && dtend != "":
		return schedule.Wall{}, false, fieldErr("dtend", "combination", "only a window has a dtend")
	case dtend == "":
		return schedule.Wall{}, false, nil
	}
	end, err := schedule.ParseWall(dtend)
	if err != nil {
		return schedule.Wall{}, false, err
	}
	if end.AllDay != start.AllDay {
		return schedule.Wall{}, false, fieldErr("dtend", "combination", "dtend must be all-day exactly when dtstart is")
	}
	if end.String() <= start.String() { // the text form is fixed-width, so it sorts
		return schedule.Wall{}, false, fieldErr("dtend", "range", "dtend must be after dtstart")
	}
	return end, true, nil
}

// boundZone is the largest offset in tzdb. A floating schedule's row is armed
// at the wall read here, the earliest instant any zone could give it, so the
// row is never armed late; the exact instant is decided at fire time in the
// recipient's zone.
var boundZone = time.FixedZone("+14", 14*3600)

// wallInstant resolves a wall in loc. An all-day wall resolves at the all-day
// fire hour, because Occurrence.At for an all-day occurrence is local midnight.
func wallInstant(loc *time.Location, w schedule.Wall) time.Time {
	if w.AllDay {
		w.AllDay = false
		w.Hour = allDayFireHour
	}
	return schedule.Resolve(loc, w)
}

// fireInstant is the one function that turns an occurrence into the instant its
// managed row is armed at, used by create, update and the firing path. at is
// the occurrence's instant (timed occurrences use it as given, which keeps a
// repeated fall-back hour on an elapsed rule distinct); an all-day occurrence
// is read at the all-day hour in loc; a set override replaces the base instant
// before the lead is subtracted.
func fireInstant(at time.Time, w schedule.Wall, loc *time.Location, leadMinutes int, override *time.Time) time.Time {
	base := at
	if w.AllDay {
		base = wallInstant(loc, w)
	}
	if override != nil {
		base = *override
	}
	return base.Add(-time.Duration(leadMinutes) * time.Minute).UTC()
}

// scheduleArming is where a schedule's outstanding occurrence lands.
type scheduleArming struct {
	NextAt   time.Time
	NextWall schedule.Wall
	RemindAt time.Time
}

// armSchedule computes the arming of an occurrence. Pinned: the occurrence
// instant and its fire instant in the schedule's own zone. Floating: the early
// bound for both, decided exactly at fire time.
func armSchedule(c *schedule.Compiled, occ schedule.Occurrence, lead int, override *time.Time) (scheduleArming, error) {
	spec := c.Spec()
	if spec.Floating {
		at := wallInstant(boundZone, occ.Wall)
		return scheduleArming{NextAt: at, NextWall: occ.Wall, RemindAt: fireInstant(at, occ.Wall, boundZone, lead, override)}, nil
	}
	loc, err := time.LoadLocation(spec.Zone)
	if err != nil {
		return scheduleArming{}, fmt.Errorf("load schedule zone: %w", err)
	}
	return scheduleArming{NextAt: occ.At, NextWall: occ.Wall, RemindAt: fireInstant(occ.At, occ.Wall, loc, lead, override)}, nil
}

// scheduleRecipientZone is the zone a floating schedule is read in: the item's
// assignee, else its creator, which is exactly the rule
// enqueueReminderNotificationTx uses, so whose zone and who is notified cannot
// disagree. Unset, deleted or unloadable is UTC.
func (s *Store) scheduleRecipientZone(q Queryer, itemID string) (*time.Location, error) {
	// One statement: a reassignment committing between a read of the assignee
	// and a read of the zone would arm in the former assignee's zone.
	var tz sql.NullString
	err := q.QueryRow(s.q(`
		SELECT u.timezone FROM items i
		LEFT JOIN users u ON u.id = COALESCE(NULLIF(i.assigned_user_id, ''), NULLIF(i.created_by_user_id, ''))
		WHERE i.id = ?`), itemID).Scan(&tz)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read schedule recipient zone: %w", err)
	}
	var stored *string
	if tz.Valid {
		stored = &tz.String
	}
	return usertz.Resolve(stored).Loc, nil
}

// scheduleZoneOf is the zone of the assignee, else of the creator: the one rule
// the cross-workspace copy and its preview use, which already hold both ids.
// The copy passes the copier's ACCOUNT id as creator, the one the new item's
// created_by_user_id holds. When the carried assignee is dropped (not a member of
// the destination) this fallback changes a floating schedule's zone, so the
// destination walks its walls in the new zone: a wall the source already fired
// can fire again there, or one can be skipped (TASK-4221 arms strictly after the
// source's last fired instant).
func (s *Store) scheduleZoneOf(q Queryer, assignee, creator string) (*time.Location, error) {
	uid := assignee
	if uid == "" {
		uid = creator
	}
	var tz sql.NullString
	err := q.QueryRow(s.q(`SELECT timezone FROM users WHERE id = ?`), uid).Scan(&tz)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read schedule recipient zone: %w", err)
	}
	var stored *string
	if tz.Valid {
		stored = &tz.String
	}
	return usertz.Resolve(stored).Loc, nil
}

// nextOccurrence is the first occurrence strictly after nowT-1s, so one that is
// due this very second still counts. A floating schedule is read in view.
func nextOccurrence(c *schedule.Compiled, view *time.Location, nowT time.Time) (schedule.Occurrence, bool, error) {
	return c.NextAfter(nowT.Add(-time.Second), view, schedule.DefaultLimits)
}

const scheduleColumns = `id, workspace_id, item_id, kind, dtstart, dtend, tz, floating, rrule, rdates, exdates,
	anchor, audience, lead_minutes, next_at, next_wall, next_override_at, anchor_wall, ended_at, error_code,
	(SELECT r.id FROM item_reminders r WHERE r.schedule_id = item_schedules.id),
	(SELECT r.fired_at FROM item_reminders r WHERE r.schedule_id = item_schedules.id),
 (SELECT r.acked_at FROM item_reminders r WHERE r.schedule_id = item_schedules.id), created_at, updated_at`

func scanSchedule(row interface{ Scan(...any) error }) (*models.ItemSchedule, error) {
	var sc models.ItemSchedule
	var dtend, tz, rrule, nextAt, nextWall, override, anchorWall, endedAt, errCode, reminderID, reminderFired, reminderAcked sql.NullString
	var rdates, exdates string
	if err := row.Scan(&sc.ID, &sc.WorkspaceID, &sc.ItemID, &sc.Kind, &sc.Dtstart, &dtend, &tz, &sc.Floating,
		&rrule, &rdates, &exdates, &sc.Anchor, &sc.Audience, &sc.LeadMinutes, &nextAt, &nextWall, &override,
		&anchorWall, &endedAt, &errCode, &reminderID, &reminderFired, &reminderAcked, &sc.CreatedAt, &sc.UpdatedAt); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		dst **string
		src sql.NullString
	}{{&sc.Dtend, dtend}, {&sc.Tz, tz}, {&sc.Rrule, rrule}, {&sc.NextAt, nextAt}, {&sc.NextWall, nextWall},
		{&sc.NextOverrideAt, override}, {&sc.AnchorWall, anchorWall}, {&sc.EndedAt, endedAt},
		{&sc.ErrorCode, errCode}, {&sc.ReminderID, reminderID}, {&sc.ReminderAckedAt, reminderAcked}} {
		if f.src.Valid {
			v := f.src.String
			*f.dst = &v
		}
	}
	sc.ReminderFired = reminderFired.Valid
	if err := json.Unmarshal([]byte(rdates), &sc.RDates); err != nil {
		return nil, fmt.Errorf("decode rdates: %w", err)
	}
	if err := json.Unmarshal([]byte(exdates), &sc.ExDates); err != nil {
		return nil, fmt.Errorf("decode exdates: %w", err)
	}
	if sc.RDates == nil {
		sc.RDates = []string{}
	}
	if sc.ExDates == nil {
		sc.ExDates = []string{}
	}
	return &sc, nil
}

func wallStrings(ws []schedule.Wall) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.String())
	}
	return out
}

func mustJSON(v []string) string {
	b, _ := json.Marshal(v) // a []string cannot fail to marshal
	return string(b)
}

// GetSchedule returns an item's schedule, or (nil, nil) when it has none.
func (s *Store) GetSchedule(workspaceID, itemID string) (*models.ItemSchedule, error) {
	return s.getScheduleQ(s.db, workspaceID, itemID, false)
}

func (s *Store) getScheduleQ(q Queryer, workspaceID, itemID string, lock bool) (*models.ItemSchedule, error) {
	query := `SELECT ` + scheduleColumns + ` FROM item_schedules WHERE item_id = ? AND workspace_id = ?`
	if lock && s.dialect.Driver() == DriverPostgres {
		query += ` FOR UPDATE`
	}
	sc, err := scanSchedule(q.QueryRow(s.q(query), itemID, workspaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get schedule: %w", err)
	}
	if err := s.fillScheduleAcknowledgements(q, []*models.ItemSchedule{sc}); err != nil {
		return nil, err
	}
	return sc, nil
}

// CreateSchedule attaches a schedule to a live item, with its first managed
// reminder row, in one transaction: both rows or neither.
func (s *Store) CreateSchedule(workspaceID, itemID string, in models.ScheduleCreate) (*models.ItemSchedule, error) {
	return s.createScheduleAt(workspaceID, itemID, in, time.Now())
}

func (s *Store) createScheduleAt(workspaceID, itemID string, in models.ScheduleCreate, nowT time.Time) (*models.ItemSchedule, error) {
	return s.createScheduleRolesAt(workspaceID, itemID, in, nil, nowT)
}

// createScheduleRolesAt creates the schedule with the legacy roles derived from
// its kind, or with native roles when roles is given.
func (s *Store) createScheduleRolesAt(workspaceID, itemID string, in models.ScheduleCreate, roles *scheduleRoles, nowT time.Time) (*models.ItemSchedule, error) {
	anchor, audience := in.Anchor, in.Audience
	if anchor == "" {
		anchor = "calendar"
	}
	if audience == "" {
		audience = "both"
	}
	// A floating schedule with a zone is refused by Compile as CodeZone.
	c, err := compileScheduleSpec(scheduleSpecInput{Dtstart: in.Dtstart, Tz: in.Tz, Floating: in.Floating, Rrule: in.Rrule, RDates: in.RDates, ExDates: in.ExDates})
	if err != nil {
		return nil, err
	}
	if err := s.checkScheduleTiming(c); err != nil {
		return nil, err
	}
	spec := c.Spec()
	validationKind, validationEnd := in.Kind, in.Dtend
	if roles != nil {
		validationKind, validationEnd = "check", ""
	}
	end, hasEnd, err := validateScheduleFields(validationKind, anchor, audience, in.LeadMinutes, spec.Start, validationEnd)
	if err != nil {
		return nil, err
	}
	if err := validateAnchorRule(anchor, spec); err != nil {
		return nil, err
	}

	var view *time.Location
	if spec.Floating {
		if view, err = s.scheduleRecipientZone(s.db, itemID); err != nil {
			return nil, err
		}
	}
	occ, found, err := nextOccurrence(c, view, nowT)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrScheduleExhausted
	}
	arm, err := armSchedule(c, occ, in.LeadMinutes, nil)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("create schedule: %w", err)
	}
	defer tx.Rollback()

	if err := s.requireLiveItemTx(tx, workspaceID, itemID); err != nil {
		return nil, err
	}
	if s.afterScheduleLiveCheck != nil {
		s.afterScheduleLiveCheck(tx)
	}
	id, ts := newID(), now()
	var dtend, tzVal, rrule any
	if hasEnd {
		dtend = end.String()
	}
	if !spec.Floating {
		tzVal = spec.Zone
	}
	if spec.Rule != "" {
		rrule = spec.Rule
	}
	sc := &models.ItemSchedule{ID: id, WorkspaceID: workspaceID, ItemID: itemID, Kind: in.Kind, Dtstart: spec.Start.String(),
		Floating: spec.Floating, RDates: wallStrings(spec.RDates), ExDates: wallStrings(spec.ExDates), Anchor: anchor, Audience: audience, LeadMinutes: in.LeadMinutes,
		NextAt: ptr(NormalizeInstant(arm.NextAt)), NextWall: ptr(arm.NextWall.String()), CreatedAt: ts, UpdatedAt: ts}
	if dtend != nil {
		sc.Dtend = ptr(dtend.(string))
	}
	if tzVal != nil {
		sc.Tz = ptr(tzVal.(string))
	}
	if rrule != nil {
		sc.Rrule = ptr(rrule.(string))
	}
	if roles != nil {
		err = s.writeNativeRolesTx(tx, sc, *roles, true)
	} else {
		err = s.insertScheduleTx(tx, sc, nil)
	}
	if err != nil {
		return nil, err
	}
	if err := s.insertManagedReminderTx(tx, workspaceID, itemID, id, NormalizeInstant(arm.RemindAt)); err != nil {
		return nil, err
	}
	if roles == nil {
		if err := s.setLegacyRolesTx(tx, id, in.Kind, spec.Start.String(), in.Dtend); err != nil {
			return nil, err
		}
	}
	if err := s.journalScheduleAfterTx(tx, id, "create", "", "", "", "system"); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create schedule: %w", err)
	}
	return s.GetSchedule(workspaceID, itemID)
}

// insertScheduleTx stores the initial kind/end and native role columns in the
// same statement. Legacy callers populate their roles immediately afterwards.
func (s *Store) insertScheduleTx(tx *sql.Tx, sc *models.ItemSchedule, native *scheduleRoles) error {
	r := scheduleRoles{Origin: "legacy_backfill"}
	if native != nil {
		r = *native
	}
	_, err := tx.Exec(s.q(`INSERT INTO item_schedules (id,workspace_id,item_id,kind,dtstart,dtend,tz,floating,rrule,rdates,exdates,anchor,audience,lead_minutes,next_at,next_wall,next_override_at,anchor_wall,ended_at,error_code,created_at,updated_at,
 target_days,target_secs,earliest_days,earliest_secs,deadline_days,deadline_secs,deadline_behavior,occupied_days,occupied_secs,outcome,roles_origin)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		sc.ID, sc.WorkspaceID, sc.ItemID, sc.Kind, sc.Dtstart, nullStr(sc.Dtend), nullStr(sc.Tz), s.dialect.BoolToInt(sc.Floating), nullStr(sc.Rrule), mustJSON(sc.RDates), mustJSON(sc.ExDates), sc.Anchor, sc.Audience, sc.LeadMinutes, nullStr(sc.NextAt), nullStr(sc.NextWall), nullStr(sc.NextOverrideAt), nullStr(sc.AnchorWall), nullStr(sc.EndedAt), nullStr(sc.ErrorCode), sc.CreatedAt, sc.UpdatedAt,
		r.TargetDays, r.TargetSecs, r.EarliestDays, r.EarliestSecs, r.DeadlineDays, r.DeadlineSecs, r.DeadlineBehavior, r.OccupiedDays, r.OccupiedSecs, r.Outcome, r.Origin)
	if isUniqueViolation(err) {
		return ErrScheduleExists
	}
	if isForeignKeyViolation(err) {
		return ErrScheduleItemGone
	}
	if err != nil {
		return fmt.Errorf("create schedule: %w", err)
	}
	return nil
}

// requireLiveItemTx asserts the item is a live item of the workspace and, on
// Postgres, holds it shared so an archival cannot commit under the insert.
func (s *Store) requireLiveItemTx(tx *sql.Tx, workspaceID, itemID string) error {
	q := `SELECT 1 FROM items WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL`
	if s.dialect.Driver() == DriverPostgres {
		q += ` FOR SHARE`
	}
	var one int
	err := tx.QueryRow(s.q(q), itemID, workspaceID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrScheduleItemGone
	}
	if err != nil {
		return fmt.Errorf("check schedule item: %w", err)
	}
	return nil
}

// The managed-row writers below are the schedule's own, so they carry NO
// schedule_id IS NULL predicate (the public reminder writers do). Callers hold
// the schedule row first.

func (s *Store) insertManagedReminderTx(tx *sql.Tx, workspaceID, itemID, scheduleID, remindAt string) error {
	ts := now()
	if _, err := tx.Exec(s.q(`
		INSERT INTO item_reminders (id, workspace_id, item_id, remind_at, fired_at, acked_at, schedule_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, NULL, NULL, ?, ?, ?)`),
		newID(), workspaceID, itemID, remindAt, scheduleID, ts, ts); err != nil {
		return fmt.Errorf("insert managed reminder: %w", err)
	}
	return nil
}

// rearmManagedReminderTx moves the managed row's instant and clears both fire
// marks, reporting whether a row existed.
func (s *Store) rearmManagedReminderTx(tx *sql.Tx, scheduleID, remindAt string) (bool, error) {
	res, err := tx.Exec(s.q(`
		UPDATE item_reminders SET remind_at = ?, fired_at = NULL, acked_at = NULL, updated_at = ?
		WHERE schedule_id = ?`), remindAt, now(), scheduleID)
	if err != nil {
		return false, fmt.Errorf("rearm managed reminder: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) deleteManagedReminderTx(tx *sql.Tx, scheduleID string) error {
	if _, err := tx.Exec(s.q(`DELETE FROM item_reminders WHERE schedule_id = ?`), scheduleID); err != nil {
		return fmt.Errorf("delete managed reminder: %w", err)
	}
	return nil
}

// UpdateSchedule applies a partial update. It returns (nil, nil) when the item
// has no schedule.
//
// A timing edit (dtstart, tz, floating, rrule, rdates, exdates or anchor
// named in the update, with a value that differs, or on a schedule in an error
// state) recomputes the outstanding occurrence from now, clears anchor_wall,
// next_override_at and error_code, and re-arms the managed row. An edit that
// leaves no occurrence ends the schedule and deletes the managed row; an edit
// that gives an ended schedule an occurrence again revives it. Changing only
// kind, audience or lead_minutes keeps the outstanding occurrence and moves the
// instant of a still-armed row.
func (s *Store) UpdateSchedule(workspaceID, itemID string, upd models.ScheduleUpdate) (*models.ItemSchedule, error) {
	return s.updateScheduleAt(workspaceID, itemID, upd, time.Now())
}

func (s *Store) updateScheduleAt(workspaceID, itemID string, upd models.ScheduleUpdate, nowT time.Time) (*models.ItemSchedule, error) {
	sc, err := s.updateCommitmentAt(workspaceID, itemID, upd, nil, nowT)
	if errors.Is(err, ErrScheduleNotFound) {
		return nil, nil
	}
	return sc, err
}

func (s *Store) updateCommitmentAt(workspaceID, itemID string, upd models.ScheduleUpdate, partial *models.CommitmentRolesPatch, nowT time.Time) (*models.ItemSchedule, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("update schedule: %w", err)
	}
	defer tx.Rollback()

	if err := s.pinScheduleTx(tx, workspaceID, itemID); err != nil {
		return nil, err
	}
	cur, err := s.getScheduleQ(tx, workspaceID, itemID, false)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, ErrScheduleNotFound
	}
	storedRoles, err := s.scheduleRolesOf(tx, cur)
	if err != nil {
		return nil, err
	}
	// An edge reads this schedule's dates: a timing edit, or a change to a role an
	// edge uses, would leave the other end where the link no longer puts it. The
	// link refusal comes before the hard-deadline one, which is about who may
	// change a deadline, not about whether the dates may move at all.
	linked, err := s.linkedRolesTx(tx, cur.ID)
	if err != nil {
		return nil, err
	}
	// Merge only after pinScheduleTx and the transaction's storedRoles read.
	var roles *scheduleRoles
	if partial != nil {
		merged, err := mergeRolesPatch(storedRoles, *partial)
		if err != nil {
			return nil, err
		}
		if err := guardLinkedRoles(linked, storedRoles, merged); err != nil {
			return nil, err
		}
		if storedRoles.Origin == "native" && storedRoles.DeadlineDays != nil && !sameDeadline(storedRoles, merged) {
			return nil, ErrHardDeadlineRenegotiate
		}
		// Origin participates: a deliberate legacy edit still converts and journals.
		if !reflect.DeepEqual(storedRoles, merged) {
			roles = &merged
		}
	}
	before, err := s.scheduleSnapshotTx(tx, cur.ID)
	if err != nil {
		return nil, err
	}
	// The item may have been archived while this waited on the schedule row: a
	// cross-workspace move archives it, ends the schedule and commits first. A
	// plain read, so no lock edge from the schedule to the item; an edit must not
	// revive a schedule that move ended (TASK-4104 D3).
	var live int
	switch err := tx.QueryRow(s.q(`SELECT 1 FROM items WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL`),
		itemID, workspaceID).Scan(&live); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrScheduleItemGone
	case err != nil:
		return nil, fmt.Errorf("update schedule: check item: %w", err)
	}

	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	next := scheduleSpecInput{
		Dtstart: cur.Dtstart, Tz: deref(cur.Tz), Floating: cur.Floating, Rrule: deref(cur.Rrule),
		RDates: cur.RDates, ExDates: cur.ExDates,
	}
	kind, anchor, audience, lead, dtend := cur.Kind, cur.Anchor, cur.Audience, cur.LeadMinutes, deref(cur.Dtend)
	if upd.Kind != nil {
		kind = *upd.Kind
	}
	if upd.Anchor != nil {
		anchor = *upd.Anchor
	}
	if upd.Audience != nil {
		audience = *upd.Audience
	}
	if upd.LeadMinutes != nil {
		lead = *upd.LeadMinutes
	}
	if upd.Dtstart != nil {
		next.Dtstart = *upd.Dtstart
	}
	if upd.Dtend != nil {
		dtend = *upd.Dtend
	}
	if upd.Floating != nil {
		next.Floating = *upd.Floating
		if next.Floating {
			next.Tz = ""
		}
	}
	if upd.Tz != nil {
		next.Tz = *upd.Tz
	}
	if upd.Rrule != nil {
		next.Rrule = *upd.Rrule
	}
	if upd.RDates != nil {
		next.RDates = *upd.RDates
	}
	if upd.ExDates != nil {
		next.ExDates = *upd.ExDates
	}

	timingNamed := upd.Dtstart != nil || upd.Tz != nil || upd.Floating != nil ||
		upd.Rrule != nil || upd.RDates != nil || upd.ExDates != nil || upd.Anchor != nil

	// A non-timing edit never compiles: the stored rule is carried as it is, so a
	// schedule whose rule no longer compiles can still change its label-level
	// fields and keeps its error_code. Only a timing edit goes through the door.
	var c *schedule.Compiled
	var spec schedule.Spec
	if timingNamed {
		if c, err = compileScheduleSpec(next); err != nil {
			return nil, err
		}
		// The floor and the no-occurrence check re-run only when the rule, dtstart,
		// RDATEs or EXDATEs change; a tz, floating or anchor edit of an old row
		// saves (TASK-4236).
		if upd.Dtstart != nil || upd.Rrule != nil || upd.RDates != nil || upd.ExDates != nil {
			if err := s.checkScheduleTiming(c); err != nil {
				return nil, err
			}
		}
		spec = c.Spec()
	} else {
		start, err := schedule.ParseWall(cur.Dtstart)
		if err != nil {
			return nil, fmt.Errorf("parse stored dtstart: %w", err)
		}
		spec = schedule.Spec{Start: start, Floating: cur.Floating, Zone: deref(cur.Tz), Rule: deref(cur.Rrule)}
		if spec.RDates, err = parseWalls(cur.RDates); err != nil {
			return nil, err
		}
		if spec.ExDates, err = parseWalls(cur.ExDates); err != nil {
			return nil, err
		}
	}
	if roles == nil && upd.Dtstart != nil && upd.Dtend == nil && storedRoles.OccupiedDays != nil {
		derivedEnd, err := occupiedEnd(spec.Start.String(), storedRoles)
		if err != nil {
			return nil, err
		}
		if kind == "window" {
			dtend = derefStr(derivedEnd)
		}
	}
	if roles != nil {
		if _, err := occupiedEnd(spec.Start.String(), *roles); err != nil {
			return nil, err
		}
	} else if storedRoles.Origin == "native" {
		if _, err := occupiedEnd(spec.Start.String(), storedRoles); err != nil {
			return nil, err
		}
	}
	validationKind, validationEnd := kind, dtend
	if roles != nil {
		validationKind, validationEnd = "check", ""
	}
	end, hasEnd, err := validateScheduleFields(validationKind, anchor, audience, lead, spec.Start, validationEnd)
	if err != nil {
		return nil, err
	}
	if timingNamed { // spec is the compiled canonical timing only then
		if err := validateAnchorRule(anchor, spec); err != nil {
			return nil, err
		}
	}

	// Compare canonical forms: the stored rule and walls are canonical.
	timingChanged := spec.Start.String() != cur.Dtstart ||
		spec.Floating != cur.Floating || (!spec.Floating && spec.Zone != deref(cur.Tz)) ||
		spec.Rule != deref(cur.Rrule) || anchor != cur.Anchor ||
		!equalStringSlices(wallStrings(spec.RDates), cur.RDates) || !equalStringSlices(wallStrings(spec.ExDates), cur.ExDates)
	timingEdit := timingNamed && (timingChanged || cur.ErrorCode != nil)
	if len(linked) > 0 && timingEdit {
		return nil, ErrScheduleLinked
	}
	if storedRoles.Origin == "native" && storedRoles.DeadlineDays != nil &&
		(timingEdit || (timingNamed && cur.EndedAt != nil)) {
		return nil, ErrHardDeadlineRenegotiate
	}

	if !timingEdit && roles == nil && kind == cur.Kind && anchor == cur.Anchor && audience == cur.Audience && lead == cur.LeadMinutes && dtend == deref(cur.Dtend) {
		return cur, nil
	}
	ts := now()
	nextAt, nextWall, override := cur.NextAt, cur.NextWall, cur.NextOverrideAt
	anchorWall, endedAt, errCode := cur.AnchorWall, cur.EndedAt, cur.ErrorCode
	var rearm *time.Time
	leadRetime := false
	deleteRow := false

	switch {
	case timingEdit:
		var view *time.Location
		if spec.Floating {
			if view, err = s.scheduleRecipientZone(tx, itemID); err != nil {
				return nil, err
			}
		}
		occ, found, err := nextOccurrence(c, view, nowT)
		if err != nil {
			return nil, err
		}
		override, anchorWall, errCode = nil, nil, nil
		if !found {
			nextAt, nextWall = nil, nil
			if endedAt == nil {
				endedAt = &ts
			}
			deleteRow = true
			break
		}
		arm, err := armSchedule(c, occ, lead, nil)
		if err != nil {
			return nil, err
		}
		na, nw := NormalizeInstant(arm.NextAt), arm.NextWall.String()
		nextAt, nextWall, endedAt = &na, &nw, nil
		r := arm.RemindAt
		rearm = &r
	case lead != cur.LeadMinutes && cur.EndedAt == nil && nextAt != nil && nextWall != nil:
		r, err := storedFireInstant(cur, lead)
		if err != nil {
			return nil, err
		}
		rearm = &r
		leadRetime = true
	}

	var tzVal, rrule, dtendVal any
	if !spec.Floating {
		tzVal = spec.Zone
	}
	if spec.Rule != "" {
		rrule = spec.Rule
	}
	if hasEnd {
		dtendVal = end.String()
	}
	if roles != nil {
		edited := *cur
		edited.Dtstart, edited.Anchor, edited.Audience, edited.LeadMinutes = spec.Start.String(), anchor, audience, lead
		if err := s.writeNativeRolesTx(tx, &edited, *roles, false); err != nil {
			return nil, err
		}
		kind, dtend = edited.Kind, derefStr(edited.Dtend)
		dtendVal = nullStr(edited.Dtend)
	}
	derivedColumns := "kind=?,dtend=?,"
	args := []any{kind, dtendVal}
	if roles != nil {
		derivedColumns = ""
		args = nil
	}
	args = append(args, spec.Start.String(), tzVal, s.dialect.BoolToInt(spec.Floating), rrule,
		mustJSON(wallStrings(spec.RDates)), mustJSON(wallStrings(spec.ExDates)), anchor, audience, lead,
		nullStr(nextAt), nullStr(nextWall), nullStr(override), nullStr(anchorWall), nullStr(endedAt), nullStr(errCode), ts, cur.ID)
	if _, err := tx.Exec(s.q(`UPDATE item_schedules SET `+derivedColumns+`dtstart=?,tz=?,floating=?,rrule=?,rdates=?,exdates=?,
 anchor=?,audience=?,lead_minutes=?,next_at=?,next_wall=?,next_override_at=?,anchor_wall=?,ended_at=?,error_code=?,updated_at=? WHERE id=?`), args...); err != nil {
		return nil, fmt.Errorf("update schedule: %w", err)
	}

	switch {
	case deleteRow:
		if err := s.deleteManagedReminderTx(tx, cur.ID); err != nil {
			return nil, err
		}
	case leadRetime:
		// Only an armed row moves, and the write itself is conditional, so a tick
		// that fires the row between our read of the schedule and this statement
		// is never un-fired. fired_at and acked_at are not touched.
		if err := s.retimeArmedReminderTx(tx, cur.ID, NormalizeInstant(*rearm)); err != nil {
			return nil, err
		}
	case rearm != nil:
		at := NormalizeInstant(*rearm)
		ok, err := s.rearmManagedReminderTx(tx, cur.ID, at)
		if err != nil {
			return nil, err
		}
		if !ok {
			if err := s.insertManagedReminderTx(tx, workspaceID, itemID, cur.ID, at); err != nil {
				return nil, err
			}
		}
	}
	clearedOverrides := false
	if timingEdit {
		n, err := s.rekeyResolvedOverridesTx(tx, cur, spec.Floating, nextAt, nextWall)
		if err != nil {
			return nil, err
		}
		clearedOverrides = n > 0
		if err := s.rekeyPendingGatesTx(tx, cur, spec.Floating, nextAt, nextWall); err != nil {
			return nil, err
		}
	}
	if storedRoles.Origin == "legacy_backfill" && roles == nil {
		if err := s.setLegacyRolesTx(tx, cur.ID, kind, spec.Start.String(), dtend); err != nil {
			return nil, err
		}
	}
	if len(linked) > 0 {
		// A legacy row derives its roles from kind and dtend, so an edit that names
		// no role can still move one.
		edited, err := s.getScheduleQ(tx, workspaceID, itemID, false)
		if err != nil {
			return nil, err
		}
		now, err := s.scheduleRolesOf(tx, edited)
		if err != nil {
			return nil, err
		}
		if err := guardLinkedRoles(linked, storedRoles, now); err != nil {
			return nil, err
		}
	}
	after, err := s.scheduleSnapshotTx(tx, cur.ID)
	if err != nil {
		return nil, err
	}
	if after != before || clearedOverrides {
		reason := ""
		if roles != nil {
			reason = rolesWriteReason
		}
		if clearedOverrides {
			reason = "Occurrence overrides cleared"
		}
		if err := s.journalScheduleChangeReasonTx(tx, cur.ID, "update", "", "", before, after, "system", reason, "system"); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("update schedule: %w", err)
	}
	return s.GetSchedule(workspaceID, itemID)
}

// storedFireInstant recomputes a managed row's instant after a lead change from
// the stored outstanding occurrence, its zone and any override, through
// fireInstant. It never compiles the rule.
func storedFireInstant(cur *models.ItemSchedule, lead int) (time.Time, error) {
	wall, err := schedule.ParseWall(*cur.NextWall)
	if err != nil {
		return time.Time{}, err
	}
	at, err := time.Parse(time.RFC3339, *cur.NextAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse next_at: %w", err)
	}
	var override *time.Time
	if cur.NextOverrideAt != nil {
		o, err := time.Parse(time.RFC3339, *cur.NextOverrideAt)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse next_override_at: %w", err)
		}
		override = &o
	}
	loc := boundZone
	if !cur.Floating {
		if cur.Tz == nil {
			return time.Time{}, errors.New("pinned schedule has no zone")
		}
		if loc, err = time.LoadLocation(*cur.Tz); err != nil {
			return time.Time{}, fmt.Errorf("load schedule zone: %w", err)
		}
	}
	return fireInstant(at, wall, loc, lead, override), nil
}

// retimeArmedReminderTx moves the managed row's instant only while it is still
// armed. It returns nil on 0 rows (fired or absent): a fired row's occurrence
// has already gone out and is left alone.
func (s *Store) retimeArmedReminderTx(tx *sql.Tx, scheduleID, remindAt string) error {
	if _, err := tx.Exec(s.q(`
		UPDATE item_reminders SET remind_at = ?, updated_at = ?
		WHERE schedule_id = ? AND fired_at IS NULL`), remindAt, now(), scheduleID); err != nil {
		return fmt.Errorf("retime managed reminder: %w", err)
	}
	return nil
}

func parseWalls(ws []string) ([]schedule.Wall, error) {
	var out []schedule.Wall
	for _, w := range ws {
		p, err := schedule.ParseWall(w)
		if err != nil {
			return nil, fmt.Errorf("parse stored wall: %w", err)
		}
		out = append(out, p)
	}
	return out, nil
}

// DeleteSchedule removes an item's schedule, its managed reminder row and its
// resolved occurrences, reporting whether there was one.
func (s *Store) DeleteSchedule(workspaceID, itemID string) (bool, error) {
	return s.DeleteScheduleChecked(workspaceID, itemID, true, nil)
}

// DeleteScheduleChecked is DeleteSchedule for a caller that has decided whether
// the actor may take a deadline away. A native schedule that has a deadline is
// removed only when mayDropDeadline is true; otherwise nothing is deleted and
// the answer is ErrDeadlineDeleteAuthority. The deadline is read under the
// joint schedule pin, in the delete's own transaction, so a deadline added
// while the call waits is still seen. For a native deadline, expectedAssignee
// guards that authority decision; nil means it does not depend on assignment.
func (s *Store) DeleteScheduleChecked(workspaceID, itemID string, mayDropDeadline bool, expectedAssignee *string) (bool, error) {
	if s.beforeCommitmentPin != nil {
		s.beforeCommitmentPin()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("delete schedule: %w", err)
	}
	defer tx.Rollback()
	if err := s.pinScheduleTx(tx, workspaceID, itemID); err != nil {
		if errors.Is(err, ErrScheduleNotFound) {
			return false, nil
		}
		return false, err
	}
	cur, err := s.getScheduleQ(tx, workspaceID, itemID, false)
	if err != nil || cur == nil {
		return false, err
	}
	// The roles are read only when an authority decision depends on them: a
	// caller that may drop a deadline and carries no assignee guard needs no
	// read, so a row whose legacy dates no longer parse still deletes.
	if !mayDropDeadline || expectedAssignee != nil {
		roles, err := s.scheduleRolesOf(tx, cur)
		if err != nil {
			return false, err
		}
		if roles.Origin == "native" && roles.DeadlineDays != nil {
			if matches, err := s.commitmentAssigneeMatchesTx(tx, itemID, expectedAssignee); err != nil {
				return false, err
			} else if !matches {
				return false, ErrDeadlineDeleteChanged
			}
			if !mayDropDeadline {
				return false, ErrDeadlineDeleteAuthority
			}
		}
	}
	used, err := s.linkedRolesTx(tx, cur.ID)
	if err != nil {
		return false, err
	}
	if len(used) > 0 {
		return false, ErrScheduleLinked
	}
	before, err := s.scheduleSnapshotTx(tx, cur.ID)
	if err != nil {
		return false, err
	}
	if err := s.journalScheduleChangeTx(tx, cur.ID, "delete", "", "", before, "", "system"); err != nil {
		return false, err
	}
	if err := s.deleteManagedReminderTx(tx, cur.ID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(s.q(`DELETE FROM item_schedules WHERE id = ?`), cur.ID); err != nil {
		return false, fmt.Errorf("delete schedule: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("delete schedule: %w", err)
	}
	return true, nil
}

func nullStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// rekeyPendingGatesTx runs on a timing, rule or floating edit, in the edit's own
// transaction under its pin. A commitment gate is fixed on the occurrence that
// was outstanding when it was written, by its key, and a timing edit changes that
// key: the same promise, rescheduled. A gate that has not opened and was fixed on
// the occurrence being edited follows it to the new outstanding occurrence, or to
// no key at all when the edit leaves none (the blocker item status then decides).
// An open gate stays open, and a gate fixed on any other occurrence is untouched.
func (s *Store) rekeyPendingGatesTx(tx *sql.Tx, cur *models.ItemSchedule, newFloating bool, newAt, newWall *string) error {
	if cur.NextAt == nil || cur.NextWall == nil {
		return nil
	}
	oldKey, newKey := OccurrenceKey(cur, *cur.NextAt, *cur.NextWall), ""
	if newAt != nil && newWall != nil {
		newKey = OccurrenceKey(&models.ItemSchedule{Floating: newFloating}, *newAt, *newWall)
	}
	if newKey == oldKey {
		return nil
	}
	if _, err := tx.Exec(s.q(`UPDATE schedule_gates SET blocker_occurrence_key=? WHERE blocker_schedule_id=? AND blocker_occurrence_key=? AND opened_at IS NULL`), newKey, cur.ID, oldKey); err != nil {
		return fmt.Errorf("rekey schedule gates: %w", err)
	}
	return nil
}

// rekeyResolvedOverridesTx runs on a timing, rule or floating edit. An open
// occurrence is back on the grid, so its override goes. A resolved occurrence
// (done, skipped, missed, or moved past a later one) is no longer on the grid and has no target column of
// its own, so its override stays, re-keyed to the new key form. It returns how
// many override rows were removed.
func (s *Store) rekeyResolvedOverridesTx(tx *sql.Tx, cur *models.ItemSchedule, newFloating bool, newAt, newWall *string) (int, error) {
	type resolved struct{ at, wall string }
	rows, err := tx.Query(s.q(`SELECT occurrence_at, occurrence_wall, state FROM schedule_occurrences WHERE schedule_id=?`), cur.ID)
	if err != nil {
		return 0, fmt.Errorf("read resolved occurrences: %w", err)
	}
	outstanding := ""
	if cur.NextAt != nil && cur.NextWall != nil {
		outstanding = OccurrenceKey(cur, *cur.NextAt, *cur.NextWall)
	}
	newOpen := ""
	if newAt != nil && newWall != nil {
		newOpen = OccurrenceKey(&models.ItemSchedule{Floating: newFloating}, *newAt, *newWall)
	}
	rekey := map[string]string{}
	for rows.Next() {
		var r resolved
		var state string
		if err := rows.Scan(&r.at, &r.wall, &state); err != nil {
			rows.Close()
			return 0, err
		}
		if state == "moved" && outstanding != "" && OccurrenceKey(cur, r.at, r.wall) == outstanding {
			continue // still the open occurrence: back on the grid
		}
		key := OccurrenceKey(&models.ItemSchedule{Floating: newFloating}, r.at, r.wall)
		if key == newOpen {
			continue // the edit put it back on the grid as the open occurrence
		}
		rekey[OccurrenceKey(cur, r.at, r.wall)] = key
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	old, err := s.scheduleOverridesQ(tx, cur.WorkspaceID, cur.ID)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, o := range old[cur.ID] {
		if _, err := tx.Exec(s.q(`DELETE FROM schedule_occurrence_overrides WHERE schedule_id=? AND occurrence_key=? AND role=?`), cur.ID, o.OccurrenceKey, o.Role); err != nil {
			return 0, fmt.Errorf("clear schedule override: %w", err)
		}
		key, kept := rekey[o.OccurrenceKey]
		if !kept {
			removed++
			continue
		}
		o.OccurrenceKey = key
		if err := s.upsertScheduleOverrideStoredTx(tx, cur.ID, o); err != nil {
			return 0, err
		}
	}
	return removed, nil
}
