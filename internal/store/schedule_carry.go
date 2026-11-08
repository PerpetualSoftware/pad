package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// Carrying a schedule between workspaces (TASK-4104 D2): workspace export and
// import (which `pad db migrate-to-pg` rides) now, and the cross-workspace copy
// (D3) through the same insertCarriedScheduleTx. A carried schedule is never
// trusted: every field goes through the doors a create goes through, the stored
// timing is the CANONICAL text (never the bundle's), and the outstanding
// occurrence's instant and the managed reminder row are re-derived.

// carriedSchedule is a schedule to store in a workspace other than the one it
// came from. Empty strings mean "none". The caller has already remapped any id.
type carriedSchedule struct {
	Roles                    *models.ScheduleRolesExport
	Overrides                []models.ScheduleOverrideExport
	Kind, Dtstart, Dtend, Tz string
	Floating                 bool
	Rrule                    string
	RDates, ExDates          []string
	Anchor, Audience         string
	LeadMinutes              int

	// NextWall is the outstanding occurrence; empty exactly when EndedAt is set.
	// NextAt is only a hint for a state the wall cannot give (the repeated half
	// of a fall-back hour on a pinned schedule; a fired or snoozed floating
	// occurrence's exact instant); the instant is otherwise recomputed.
	NextWall, NextAt string
	NextOverrideAt   string
	AnchorWall       string
	EndedAt          string

	// FiredAt is the managed row's fired mark ("" = armed). RemindAt is read only
	// when FiredAt is set: a fired row's instant is history, not a schedule.
	FiredAt, RemindAt string

	CreatedAt, UpdatedAt string // "" = now
	Occurrences          []carriedOccurrence
}

// carriedOccurrence is one resolved occurrence. CommentID is the id in the
// DESTINATION workspace, "" for none.
type carriedOccurrence struct {
	At, Wall, State, CommentID, Actor, ResolvedAt string
	ActualDoneAt, ActualSource                    string
}

// scheduleCarryDrop says a carried schedule failed validation and was NOT
// stored. Reason is a stable code, never the carried text. Anything else
// insertCarriedScheduleTx returns is a database failure.
type scheduleCarryDrop struct{ Reason string }

func (e *scheduleCarryDrop) Error() string { return "carried schedule refused: " + e.Reason }

// carryReason names a validation error as a drop reason.
func carryReason(err error) string {
	var fe *ScheduleFieldError
	if errors.As(err, &fe) {
		return fe.Field + ":" + fe.Code
	}
	if code := ScheduleErrorCode(err); code != "" {
		return "timing:" + code
	}
	return "invalid"
}

func dropf(field, code string) error { return &scheduleCarryDrop{Reason: field + ":" + code} }

// carryInstant normalizes an RFC3339 instant, or "" when empty.
func carryInstant(field, v string) (string, error) {
	if v == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return "", dropf(field, "instant")
	}
	return NormalizeInstant(t), nil
}

// carryWall parses a wall and returns its canonical text.
func carryWall(field, v string) (schedule.Wall, error) {
	w, err := schedule.ParseWall(v)
	if err != nil {
		return w, dropf(field, "wall")
	}
	return w, nil
}

// insertCarriedScheduleTx stores a carried schedule on a live item of
// workspaceID, with its managed reminder row (none when it is ended) and its
// history, inside tx. It validates BEFORE it writes anything (a failed
// statement poisons a Postgres transaction), returning *scheduleCarryDrop for a
// schedule it refuses. The caller holds the item.
//
// What is recomputed, and what is carried:
//   - timing: compiled and stored canonical; error_code starts clear (a rule
//     that compiles is healthy; the fire path rediscovers a cap);
//   - next_wall, next_override_at, anchor_wall, ended_at: carried (recomputing
//     from dtstart would lose a completion re-phase and a snooze), validated;
//   - next_at and the managed row's remind_at: derived from next_wall, the
//     override and the lead by armSchedule's own arithmetic.
func (s *Store) insertCarriedScheduleTx(tx *sql.Tx, workspaceID, itemID string, in carriedSchedule) (string, []string, error) {
	p, err := prepareCarriedSchedule(in)
	if err != nil {
		return "", nil, err
	}
	id, err := s.writePreparedSchedule(tx, workspaceID, itemID, in, p, "")
	return id, p.drops, err
}

// preparedCarry is a carried schedule that passed every check, with everything
// the write needs derived.
type preparedCarry struct {
	overrides                  []models.ScheduleOverrideExport
	drops                      []string
	spec                       schedule.Spec
	dtend                      schedule.Wall
	hasEnd                     bool
	anchorWall                 string
	endedAt, firedAt, override string
	createdAt, updatedAt       string
	nextAt, nextWall, remindAt any
	occs                       []carriedOccurrence
}

// prepareCarriedSchedule is the validation half of insertCarriedScheduleTx: pure
// (no database), so the copy preflight asks the question the copy does
// (TASK-4104 D3).
func prepareCarriedSchedule(in carriedSchedule) (preparedCarry, error) {
	if err := validateCarriedRoles(in.Roles); err != nil {
		return preparedCarry{}, err
	}
	overrides, drops, err := prepareCarriedOverrides(in.Overrides, in.Floating)
	if err != nil {
		return preparedCarry{}, err
	}
	c, err := compileScheduleSpec(scheduleSpecInput{Dtstart: in.Dtstart, Tz: in.Tz, Floating: in.Floating, Rrule: in.Rrule, RDates: in.RDates, ExDates: in.ExDates})
	if err != nil {
		return preparedCarry{}, &scheduleCarryDrop{Reason: carryReason(err)}
	}
	spec := c.Spec()
	if in.Roles != nil && in.Roles.RolesOrigin == "native" {
		if _, err := occupiedEnd(spec.Start.String(), scheduleRolesFromExport(in.Roles)); err != nil {
			return preparedCarry{}, &scheduleCarryDrop{Reason: carryReason(err)}
		}
	}
	end, hasEnd, err := validateScheduleFields(in.Kind, in.Anchor, in.Audience, in.LeadMinutes, spec.Start, in.Dtend)
	if err == nil {
		err = validateAnchorRule(in.Anchor, spec)
	}
	if err != nil {
		return preparedCarry{}, &scheduleCarryDrop{Reason: carryReason(err)}
	}

	var anchorWall string
	if in.AnchorWall != "" {
		aw, err := carryWall("anchor_wall", in.AnchorWall)
		if err != nil {
			return preparedCarry{}, err
		}
		if aw.AllDay != spec.Start.AllDay {
			return preparedCarry{}, dropf("anchor_wall", "combination")
		}
		// The fire path compiles from the anchor, so it must compile.
		if _, err := compileScheduleSpec(scheduleSpecInput{Dtstart: aw.String(), Tz: in.Tz, Floating: in.Floating, Rrule: in.Rrule, RDates: in.RDates, ExDates: in.ExDates}); err != nil {
			return preparedCarry{}, &scheduleCarryDrop{Reason: "anchor_wall:" + carryReason(err)}
		}
		anchorWall = aw.String()
	}

	endedAt, err := carryInstant("ended_at", in.EndedAt)
	if err != nil {
		return preparedCarry{}, err
	}
	firedAt, err := carryInstant("fired_at", in.FiredAt)
	if err != nil {
		return preparedCarry{}, err
	}
	override, err := carryInstant("next_override_at", in.NextOverrideAt)
	if err != nil {
		return preparedCarry{}, err
	}
	hint, err := carryInstant("next_at", in.NextAt)
	if err != nil {
		return preparedCarry{}, err
	}
	createdAt, err := carryInstant("created_at", in.CreatedAt)
	if err != nil {
		return preparedCarry{}, err
	}
	updatedAt, err := carryInstant("updated_at", in.UpdatedAt)
	if err != nil {
		return preparedCarry{}, err
	}
	if ts := now(); createdAt == "" {
		createdAt = ts
	}
	if updatedAt == "" {
		updatedAt = createdAt
	}

	// The outstanding occurrence.
	var nextAt, nextWall, remindAt any
	var arm scheduleArming
	if endedAt != "" {
		if in.NextWall != "" || in.NextAt != "" || override != "" {
			return preparedCarry{}, dropf("ended_at", "combination") // ended_at => next_at NULL
		}
	} else {
		if in.NextWall == "" {
			return preparedCarry{}, dropf("next_wall", "required")
		}
		w, err := carryWall("next_wall", in.NextWall)
		if err != nil {
			return preparedCarry{}, err
		}
		if w.AllDay != spec.Start.AllDay {
			return preparedCarry{}, dropf("next_wall", "combination")
		}
		var ov *time.Time
		if override != "" {
			t, _ := time.Parse(time.RFC3339, override)
			ov = &t
		}
		if arm, err = carriedArming(spec, w, hint, firedAt != "" || ov != nil, in.LeadMinutes, ov); err != nil {
			return preparedCarry{}, err
		}
		if firedAt != "" && in.RemindAt != "" {
			ra, err := carryInstant("remind_at", in.RemindAt)
			if err != nil {
				return preparedCarry{}, err
			}
			arm.RemindAt, _ = time.Parse(time.RFC3339, ra)
		}
		nextAt, nextWall, remindAt = NormalizeInstant(arm.NextAt), w.String(), NormalizeInstant(arm.RemindAt)
	}
	occs, err := prepareCarriedOccurrences(in.Occurrences)
	if err != nil {
		return preparedCarry{}, err
	}

	return preparedCarry{
		overrides: overrides, drops: drops,
		spec: spec, dtend: end, hasEnd: hasEnd, anchorWall: anchorWall, endedAt: endedAt, firedAt: firedAt, override: override,
		createdAt: createdAt, updatedAt: updatedAt, nextAt: nextAt, nextWall: nextWall, remindAt: remindAt, occs: occs,
	}, nil
}

// writePreparedSchedule stores a prepared carry: the schedule, its managed row
// (none when ended) and its history.
func (s *Store) writePreparedSchedule(tx *sql.Tx, workspaceID, itemID string, in carriedSchedule, p preparedCarry, source string) (string, error) {
	spec, end, hasEnd, anchorWall, endedAt, firedAt, override := p.spec, p.dtend, p.hasEnd, p.anchorWall, p.endedAt, p.firedAt, p.override
	createdAt, updatedAt, nextAt, nextWall, remindAt, occs := p.createdAt, p.updatedAt, p.nextAt, p.nextWall, p.remindAt, p.occs
	// Everything is valid: write.
	id := newID()
	optional := func(value string) *string {
		if value == "" {
			return nil
		}
		return ptr(value)
	}
	textPointer := func(value any) *string {
		if value == nil {
			return nil
		}
		return ptr(value.(string))
	}
	sc := &models.ItemSchedule{ID: id, WorkspaceID: workspaceID, ItemID: itemID, Kind: in.Kind, Dtstart: spec.Start.String(), Floating: spec.Floating,
		RDates: wallStrings(spec.RDates), ExDates: wallStrings(spec.ExDates), Anchor: in.Anchor, Audience: in.Audience, LeadMinutes: in.LeadMinutes,
		NextAt: textPointer(nextAt), NextWall: textPointer(nextWall), NextOverrideAt: optional(override), AnchorWall: optional(anchorWall), EndedAt: optional(endedAt), CreatedAt: createdAt, UpdatedAt: updatedAt}
	if hasEnd {
		sc.Dtend = ptr(end.String())
	}
	if !spec.Floating {
		sc.Tz = ptr(spec.Zone)
	}
	sc.Rrule = optional(spec.Rule)
	var roles *scheduleRoles
	if in.Roles != nil {
		r := scheduleRolesFromExport(in.Roles)
		roles = &r
	}
	var err error
	if roles != nil && roles.Origin == "native" {
		err = s.writeNativeRolesTx(tx, sc, *roles, true)
	} else {
		err = s.insertScheduleTx(tx, sc, roles)
	}
	if err != nil {
		return "", err
	}
	if endedAt == "" {
		if _, err := tx.Exec(s.q(`
			INSERT INTO item_reminders (id, workspace_id, item_id, remind_at, fired_at, acked_at, schedule_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?)`),
			newID(), workspaceID, itemID, remindAt, nilIfEmpty(firedAt), id, createdAt, updatedAt); err != nil {
			return "", fmt.Errorf("insert carried managed reminder: %w", err)
		}
	}
	for _, o := range occs {
		if _, err := tx.Exec(s.q(`
			INSERT INTO schedule_occurrences (id, schedule_id, occurrence_at, occurrence_wall, state, comment_id, actor, resolved_at, actual_done_at, actual_source)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			newID(), id, o.At, o.Wall, o.State, nilIfEmpty(o.CommentID), o.Actor, o.ResolvedAt, nilIfEmpty(o.ActualDoneAt), o.ActualSource); err != nil {
			return "", fmt.Errorf("insert carried occurrence: %w", err)
		}
	}
	if roles == nil {
		if err := s.setLegacyRolesTx(tx, id, in.Kind, spec.Start.String(), in.Dtend); err != nil {
			return "", err
		}
	}
	for _, o := range p.overrides {
		if err := s.upsertScheduleOverrideStoredTx(tx, id, o); err != nil {
			return "", err
		}
	}
	if source != "" {
		if err := s.journalScheduleAfterTx(tx, id, source, "", "", "", "system"); err != nil {
			return "", err
		}
	}

	return id, nil
}

// carriedArming derives the outstanding occurrence's instant and fire instant
// from its wall, by the arithmetic create and fire use. exact is true when the
// row is fired or overridden: a floating schedule then holds its exact instant
// (models.ItemSchedule.NextAt), taken from the hint when the bundle has one.
func carriedArming(spec schedule.Spec, w schedule.Wall, hint string, exact bool, lead int, override *time.Time) (scheduleArming, error) {
	var hintT time.Time
	hasHint := false
	if hint != "" {
		hintT, _ = time.Parse(time.RFC3339, hint)
		hasHint = true
	}
	if spec.Floating {
		at := wallInstant(boundZone, w)
		if exact && hasHint {
			at = hintT
		}
		return scheduleArming{NextAt: at, NextWall: w, RemindAt: fireInstant(at, w, boundZone, lead, override)}, nil
	}
	loc, err := time.LoadLocation(spec.Zone)
	if err != nil {
		return scheduleArming{}, dropf("tz", "zone")
	}
	at := schedule.Resolve(loc, w)
	// The repeated half of a fall-back hour on an elapsed rule has the wall of
	// the first half and a different instant; keep the hint when it still reads
	// as this wall in the zone.
	if hasHint && !w.AllDay && wallOf(hintT.In(loc)).String() == w.String() {
		at = hintT
	}
	return scheduleArming{NextAt: at, NextWall: w, RemindAt: fireInstant(at, w, loc, lead, override)}, nil
}

// prepareCarriedOccurrences checks a schedule's history before anything is
// written, including the two unique indexes, so a duplicate cannot fail an
// insert mid-transaction.
func prepareCarriedOccurrences(in []carriedOccurrence) ([]carriedOccurrence, error) {
	out := make([]carriedOccurrence, 0, len(in))
	seenAt, seenWall := map[string]bool{}, map[string]bool{}
	for _, o := range in {
		if !oneOf(o.State, []string{"done", "skipped", "moved", "missed"}) {
			return nil, dropf("occurrence_state", "enum")
		}
		if !oneOf(o.Actor, []string{"", "user", "agent"}) {
			return nil, dropf("occurrence_actor", "enum")
		}
		if o.ActualSource == "" {
			o.ActualSource = "legacy_unknown"
		}
		if !oneOf(o.ActualSource, []string{"reported", "recorded_now", "legacy_unknown"}) || (o.ActualDoneAt != "" && (o.State != "done" || o.ActualSource == "legacy_unknown")) || (o.ActualDoneAt == "" && o.ActualSource != "legacy_unknown") {
			return nil, dropf("actual", "combination")
		}
		actual, err := carryInstant("actual", o.ActualDoneAt)
		if err != nil {
			return nil, err
		}
		at, err := carryInstant("occurrence_at", o.At)
		if err != nil || at == "" {
			return nil, dropf("occurrence_at", "instant")
		}
		w, err := carryWall("occurrence_wall", o.Wall)
		if err != nil {
			return nil, err
		}
		resolved, err := carryInstant("resolved_at", o.ResolvedAt)
		if err != nil || resolved == "" {
			return nil, dropf("resolved_at", "instant")
		}
		if seenAt[at] || (w.AllDay && seenWall[w.String()]) {
			return nil, dropf("occurrence", "duplicate")
		}
		seenAt[at], seenWall[w.String()] = true, true
		out = append(out, carriedOccurrence{At: at, Wall: w.String(), State: o.State, CommentID: o.CommentID, Actor: o.Actor, ResolvedAt: resolved, ActualDoneAt: actual, ActualSource: o.ActualSource})
	}
	return out, nil
}

// carriedFromExport reads a bundle's schedule. rdates and exdates are rebuilt
// from the parsed values, never kept as the bundle's text (BUG-4193).
func carriedFromExport(x models.ScheduleExport, commentIDs map[string]string) (carriedSchedule, error) {
	walls := func(field string, vs []any) ([]string, error) {
		var out []string
		for _, v := range vs {
			str, ok := v.(string)
			if !ok {
				return nil, dropf(field, "type")
			}
			out = append(out, str)
		}
		return out, nil
	}
	rd, err := walls("rdates", x.RDates)
	if err != nil {
		return carriedSchedule{}, err
	}
	ex, err := walls("exdates", x.ExDates)
	if err != nil {
		return carriedSchedule{}, err
	}
	in := carriedSchedule{
		Kind: x.Kind, Dtstart: x.Dtstart, Dtend: x.Dtend, Tz: x.Tz, Floating: x.Floating, Rrule: x.Rrule,
		RDates: rd, ExDates: ex, Anchor: x.Anchor, Audience: x.Audience, LeadMinutes: x.LeadMinutes,
		NextWall: x.NextWall, NextAt: x.NextAt, NextOverrideAt: x.NextOverrideAt, AnchorWall: x.AnchorWall,
		EndedAt: x.EndedAt, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt,
		Roles: x.Roles, Overrides: x.Overrides,
	}
	if x.Reminder != nil {
		in.FiredAt, in.RemindAt = x.Reminder.FiredAt, x.Reminder.RemindAt
	}
	for _, o := range x.Occurrences {
		in.Occurrences = append(in.Occurrences, carriedOccurrence{
			At: o.OccurrenceAt, Wall: o.OccurrenceWall, State: o.State, CommentID: commentIDs[x.ItemID+"\x00"+o.CommentID],
			Actor: o.Actor, ResolvedAt: o.ResolvedAt, ActualDoneAt: o.ActualDoneAt, ActualSource: o.ActualSource,
		})
	}
	return in, nil
}

// exportSchedules reads the schedules of the workspace's live items, with the
// managed row's state, roles, overrides and history. Each read on q finishes
// before the next starts.
func (s *Store) exportSchedules(q Queryer, workspaceID string) ([]models.ScheduleExport, error) {
	rows, err := q.Query(s.q(`SELECT `+scheduleColumns+` FROM item_schedules
		WHERE workspace_id = ? AND item_id IN (SELECT i.id FROM items i WHERE i.workspace_id = ? AND i.deleted_at IS NULL)
		ORDER BY item_schedules.created_at, item_schedules.id`), workspaceID, workspaceID) // qualified: scheduleColumns also yields the reminder's id, so a bare id is ambiguous on Postgres
	if err != nil {
		return nil, fmt.Errorf("export schedules: %w", err)
	}
	var scs []*models.ItemSchedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan schedule: %w", err)
		}
		scs = append(scs, sc)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(scs) == 0 {
		return nil, nil
	}

	type managed struct{ remindAt, firedAt string }
	rem := map[string]managed{}
	rr, err := q.Query(s.q(`SELECT schedule_id, remind_at, COALESCE(fired_at, '') FROM item_reminders
		WHERE workspace_id = ? AND schedule_id IS NOT NULL`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("export schedule reminders: %w", err)
	}
	for rr.Next() {
		var id string
		var m managed
		if err := rr.Scan(&id, &m.remindAt, &m.firedAt); err != nil {
			rr.Close()
			return nil, fmt.Errorf("scan schedule reminder: %w", err)
		}
		rem[id] = m
	}
	if err := rr.Err(); err != nil {
		rr.Close()
		return nil, err
	}
	rr.Close()

	occ := map[string][]models.ScheduleOccurrenceExport{}
	or, err := q.Query(s.q(`SELECT o.schedule_id, o.occurrence_at, o.occurrence_wall, o.state, COALESCE(o.comment_id, ''), o.actor, o.resolved_at, COALESCE(o.actual_done_at,''), COALESCE(o.actual_source,'legacy_unknown')
		FROM schedule_occurrences o JOIN item_schedules sc ON sc.id = o.schedule_id
		WHERE sc.workspace_id = ? ORDER BY o.schedule_id, o.occurrence_at`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("export schedule occurrences: %w", err)
	}
	for or.Next() {
		var id string
		var o models.ScheduleOccurrenceExport
		if err := or.Scan(&id, &o.OccurrenceAt, &o.OccurrenceWall, &o.State, &o.CommentID, &o.Actor, &o.ResolvedAt, &o.ActualDoneAt, &o.ActualSource); err != nil {
			or.Close()
			return nil, fmt.Errorf("scan schedule occurrence: %w", err)
		}
		occ[id] = append(occ[id], o)
	}
	if err := or.Err(); err != nil {
		or.Close()
		return nil, err
	}
	or.Close()

	overrides, err := s.scheduleOverridesQ(q, workspaceID, "")
	if err != nil {
		return nil, err
	}
	out := make([]models.ScheduleExport, 0, len(scs))
	for _, sc := range scs {
		roles, err := s.scheduleRolesQ(q, sc.ID)
		if err != nil {
			return nil, err
		}
		x := models.ScheduleExport{
			ID: sc.ID, Roles: roles, Overrides: overrides[sc.ID],
			ItemID: sc.ItemID, Kind: sc.Kind, Dtstart: sc.Dtstart, Dtend: derefStr(sc.Dtend), Tz: derefStr(sc.Tz),
			Floating: sc.Floating, Rrule: derefStr(sc.Rrule), Anchor: sc.Anchor, Audience: sc.Audience, LeadMinutes: sc.LeadMinutes,
			NextAt: derefStr(sc.NextAt), NextWall: derefStr(sc.NextWall), NextOverrideAt: derefStr(sc.NextOverrideAt),
			AnchorWall: derefStr(sc.AnchorWall), EndedAt: derefStr(sc.EndedAt), CreatedAt: sc.CreatedAt, UpdatedAt: sc.UpdatedAt,
			Occurrences: occ[sc.ID],
		}
		for _, w := range sc.RDates {
			x.RDates = append(x.RDates, w)
		}
		for _, w := range sc.ExDates {
			x.ExDates = append(x.ExDates, w)
		}
		if m, ok := rem[sc.ID]; ok {
			x.Reminder = &models.ScheduleReminderExport{RemindAt: m.remindAt, FiredAt: m.firedAt}
		}
		out = append(out, x)
	}
	return out, nil
}
