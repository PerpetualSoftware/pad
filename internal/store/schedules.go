package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// nextOccurrence is the first occurrence strictly after nowT-1s, so one that is
// due this very second still counts. A floating schedule is read in view.
func nextOccurrence(c *schedule.Compiled, view *time.Location, nowT time.Time) (schedule.Occurrence, bool, error) {
	return c.NextAfter(nowT.Add(-time.Second), view, schedule.DefaultLimits)
}

const scheduleColumns = `id, workspace_id, item_id, kind, dtstart, dtend, tz, floating, rrule, rdates, exdates,
	anchor, audience, lead_minutes, next_at, next_wall, next_override_at, anchor_wall, ended_at, error_code,
	(SELECT r.id FROM item_reminders r WHERE r.schedule_id = item_schedules.id), created_at, updated_at`

func scanSchedule(row interface{ Scan(...any) error }) (*models.ItemSchedule, error) {
	var sc models.ItemSchedule
	var dtend, tz, rrule, nextAt, nextWall, override, anchorWall, endedAt, errCode, reminderID sql.NullString
	var rdates, exdates string
	if err := row.Scan(&sc.ID, &sc.WorkspaceID, &sc.ItemID, &sc.Kind, &sc.Dtstart, &dtend, &tz, &sc.Floating,
		&rrule, &rdates, &exdates, &sc.Anchor, &sc.Audience, &sc.LeadMinutes, &nextAt, &nextWall, &override,
		&anchorWall, &endedAt, &errCode, &reminderID, &sc.CreatedAt, &sc.UpdatedAt); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		dst **string
		src sql.NullString
	}{{&sc.Dtend, dtend}, {&sc.Tz, tz}, {&sc.Rrule, rrule}, {&sc.NextAt, nextAt}, {&sc.NextWall, nextWall},
		{&sc.NextOverrideAt, override}, {&sc.AnchorWall, anchorWall}, {&sc.EndedAt, endedAt},
		{&sc.ErrorCode, errCode}, {&sc.ReminderID, reminderID}} {
		if f.src.Valid {
			v := f.src.String
			*f.dst = &v
		}
	}
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
	return sc, nil
}

// CreateSchedule attaches a schedule to a live item, with its first managed
// reminder row, in one transaction: both rows or neither.
func (s *Store) CreateSchedule(workspaceID, itemID string, in models.ScheduleCreate) (*models.ItemSchedule, error) {
	return s.createScheduleAt(workspaceID, itemID, in, time.Now())
}

func (s *Store) createScheduleAt(workspaceID, itemID string, in models.ScheduleCreate, nowT time.Time) (*models.ItemSchedule, error) {
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
	spec := c.Spec()
	end, hasEnd, err := validateScheduleFields(in.Kind, anchor, audience, in.LeadMinutes, spec.Start, in.Dtend)
	if err != nil {
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
	if _, err := tx.Exec(s.q(`
		INSERT INTO item_schedules (id, workspace_id, item_id, kind, dtstart, dtend, tz, floating, rrule, rdates, exdates,
			anchor, audience, lead_minutes, next_at, next_wall, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, workspaceID, itemID, in.Kind, spec.Start.String(), dtend, tzVal, s.dialect.BoolToInt(spec.Floating), rrule,
		mustJSON(wallStrings(spec.RDates)), mustJSON(wallStrings(spec.ExDates)), anchor, audience, in.LeadMinutes,
		NormalizeInstant(arm.NextAt), arm.NextWall.String(), ts, ts); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrScheduleExists
		}
		if isForeignKeyViolation(err) { // the item went away after the liveness check
			return nil, ErrScheduleItemGone
		}
		return nil, fmt.Errorf("create schedule: %w", err)
	}
	if err := s.insertManagedReminderTx(tx, workspaceID, itemID, id, NormalizeInstant(arm.RemindAt)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create schedule: %w", err)
	}
	return s.GetSchedule(workspaceID, itemID)
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
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("update schedule: %w", err)
	}
	defer tx.Rollback()

	cur, err := s.getScheduleQ(tx, workspaceID, itemID, true)
	if err != nil || cur == nil {
		return nil, err
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
	end, hasEnd, err := validateScheduleFields(kind, anchor, audience, lead, spec.Start, dtend)
	if err != nil {
		return nil, err
	}

	// Compare canonical forms: the stored rule and walls are canonical.
	timingChanged := spec.Start.String() != cur.Dtstart ||
		spec.Floating != cur.Floating || (!spec.Floating && spec.Zone != deref(cur.Tz)) ||
		spec.Rule != deref(cur.Rrule) || anchor != cur.Anchor ||
		!equalStringSlices(wallStrings(spec.RDates), cur.RDates) || !equalStringSlices(wallStrings(spec.ExDates), cur.ExDates)
	timingEdit := timingNamed && (timingChanged || cur.ErrorCode != nil)

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
	if _, err := tx.Exec(s.q(`
		UPDATE item_schedules SET kind = ?, dtstart = ?, dtend = ?, tz = ?, floating = ?, rrule = ?, rdates = ?, exdates = ?,
			anchor = ?, audience = ?, lead_minutes = ?, next_at = ?, next_wall = ?, next_override_at = ?, anchor_wall = ?,
			ended_at = ?, error_code = ?, updated_at = ?
		WHERE id = ?`),
		kind, spec.Start.String(), dtendVal, tzVal, s.dialect.BoolToInt(spec.Floating), rrule,
		mustJSON(wallStrings(spec.RDates)), mustJSON(wallStrings(spec.ExDates)), anchor, audience, lead,
		nullStr(nextAt), nullStr(nextWall), nullStr(override), nullStr(anchorWall), nullStr(endedAt), nullStr(errCode), ts,
		cur.ID); err != nil {
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
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("delete schedule: %w", err)
	}
	defer tx.Rollback()
	cur, err := s.getScheduleQ(tx, workspaceID, itemID, true)
	if err != nil || cur == nil {
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
