package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

type scheduleRoles struct {
	TargetDays       int     `json:"target_days"`
	TargetSecs       int     `json:"target_secs"`
	EarliestDays     *int    `json:"earliest_days"`
	EarliestSecs     *int    `json:"earliest_secs"`
	DeadlineDays     *int    `json:"deadline_days"`
	DeadlineSecs     *int    `json:"deadline_secs"`
	DeadlineBehavior *string `json:"deadline_behavior"`
	OccupiedDays     *int    `json:"occupied_days"`
	OccupiedSecs     *int    `json:"occupied_secs"`
	Outcome          string  `json:"outcome"`
	Origin           string  `json:"roles_origin"`
}

func legacyRoles(kind, dtstart, dtend string) (scheduleRoles, error) {
	r := scheduleRoles{Origin: "legacy_backfill"}
	switch kind {
	case "deadline_hard":
		r.DeadlineDays, r.DeadlineSecs, r.DeadlineBehavior = ptr(0), ptr(0), ptr("stays_owed")
	case "window":
		layout := "2006-01-02T15:04:05"
		if len(dtstart) == 10 {
			layout = "2006-01-02"
		}
		if len(dtend) == 16 {
			dtend += ":00"
		}
		start, err := time.Parse(layout, dtstart)
		if err != nil {
			return r, err
		}
		end, err := time.Parse(layout, dtend)
		if err != nil {
			return r, err
		}
		day := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }
		secs := func(t time.Time) int { return t.Hour()*3600 + t.Minute()*60 + t.Second() }
		r.OccupiedDays = ptr(int(day(end).Sub(day(start)) / (24 * time.Hour)))
		r.OccupiedSecs = ptr(secs(end) - secs(start))
	}
	return r, nil
}

// OccurrenceKey identifies the recurrence instance independently of its target.
func OccurrenceKey(sc *models.ItemSchedule, at, wall string) string {
	if sc.Floating || len(wall) == 10 {
		return wall
	}
	return at + "|" + wall
}

func (s *Store) setLegacyRolesTx(tx *sql.Tx, id, kind, start, end string) error {
	r, err := legacyRoles(kind, start, end)
	if err != nil {
		return fmt.Errorf("derive schedule roles: %w", err)
	}
	_, err = tx.Exec(s.q(`UPDATE item_schedules SET target_days=?, target_secs=?, earliest_days=?, earliest_secs=?, deadline_days=?, deadline_secs=?, deadline_behavior=?, occupied_days=?, occupied_secs=? WHERE id=? AND roles_origin='legacy_backfill'`), r.TargetDays, r.TargetSecs, r.EarliestDays, r.EarliestSecs, r.DeadlineDays, r.DeadlineSecs, r.DeadlineBehavior, r.OccupiedDays, r.OccupiedSecs, id)
	return err
}

type scheduleDateSnapshot struct {
	Tz             *string       `json:"tz"`
	Floating       bool          `json:"floating"`
	Kind           string        `json:"kind"`
	Dtstart        string        `json:"dtstart"`
	Dtend          *string       `json:"dtend"`
	Roles          scheduleRoles `json:"roles"`
	Rrule          *string       `json:"rrule"`
	RDates         string        `json:"rdates"`
	ExDates        string        `json:"exdates"`
	Anchor         string        `json:"anchor"`
	Audience       string        `json:"audience"`
	LeadMinutes    int           `json:"lead_minutes"`
	NextAt         *string       `json:"next_at"`
	NextWall       *string       `json:"next_wall"`
	NextOverrideAt *string       `json:"next_override_at"`
	AnchorWall     *string       `json:"anchor_wall"`
	EndedAt        *string       `json:"ended_at"`
	ErrorCode      *string       `json:"error_code"`
	// The managed reminder's state, so the before of a resolve that deleted it
	// still holds what an undo would restore (TASK-4537). Absent with no row.
	ReminderRemindAt *string `json:"reminder_remind_at,omitempty"`
	ReminderFiredAt  *string `json:"reminder_fired_at,omitempty"`
	ReminderAckedAt  *string `json:"reminder_acked_at,omitempty"`
}

func (s *Store) scheduleSnapshotTx(tx *sql.Tx, id string) (string, error) {
	var v scheduleDateSnapshot
	r := &v.Roles
	err := tx.QueryRow(s.q(`SELECT tz,floating,kind,dtstart,dtend,target_days,target_secs,earliest_days,earliest_secs,deadline_days,deadline_secs,deadline_behavior,occupied_days,occupied_secs,outcome,roles_origin,rrule,rdates,exdates,anchor,audience,lead_minutes,next_at,next_wall,next_override_at,anchor_wall,ended_at,error_code,
 (SELECT r.remind_at FROM item_reminders r WHERE r.schedule_id=item_schedules.id),
 (SELECT r.fired_at FROM item_reminders r WHERE r.schedule_id=item_schedules.id),
 (SELECT r.acked_at FROM item_reminders r WHERE r.schedule_id=item_schedules.id) FROM item_schedules WHERE id=?`), id).Scan(&v.Tz, &v.Floating, &v.Kind, &v.Dtstart, &v.Dtend, &r.TargetDays, &r.TargetSecs, &r.EarliestDays, &r.EarliestSecs, &r.DeadlineDays, &r.DeadlineSecs, &r.DeadlineBehavior, &r.OccupiedDays, &r.OccupiedSecs, &r.Outcome, &r.Origin, &v.Rrule, &v.RDates, &v.ExDates, &v.Anchor, &v.Audience, &v.LeadMinutes, &v.NextAt, &v.NextWall, &v.NextOverrideAt, &v.AnchorWall, &v.EndedAt, &v.ErrorCode, &v.ReminderRemindAt, &v.ReminderFiredAt, &v.ReminderAckedAt)
	if err != nil {
		return "", fmt.Errorf("snapshot schedule: %w", err)
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// Journal population: schedule create/update/delete and resolves; native role
// writes; occurrence and series renegotiations (RenegotiateCommitment); deadline
// evaluation, on-time corrections and assignee changes; carry and restore.
// Readers: GetCommitment selects deadline renegotiations by role and user source
// as well as move/update; deadlineJournal identifies conversions by the roles
// origin snapshots; recordOnTimeCorrectionTx selects deadline misses; bundle and
// carry readers preserve all rows through scheduleChangesQ. A change type alone
// does not identify a conversion or a deadline renegotiation.
func (s *Store) journalScheduleChangeTx(tx *sql.Tx, id, change, key, role, before, after, actor string) error {
	return s.journalScheduleChangeReasonTx(tx, id, change, key, role, before, after, actor, "", "system")
}

func (s *Store) journalScheduleChangeReasonTx(tx *sql.Tx, id, change, key, role, before, after, actor, reason, source string) error {
	return s.journalScheduleChangeLineageTx(tx, id, change, key, role, before, after, actor, reason, source, "", "")
}

// journalScheduleChangeLineageTx writes a row that answers an earlier one: its
// parent is parentID and its root is rootID. With both empty the row is its own
// root and has no parent.
func (s *Store) journalScheduleChangeLineageTx(tx *sql.Tx, id, change, key, role, before, after, actor, reason, source, parentID, rootID string) error {
	return s.journalScheduleChangeRowTx(tx, newID(), id, change, key, role, before, after, actor, reason, source, parentID, rootID)
}

// journalScheduleChangeRowTx is the lineage writer with the row id chosen by
// the caller, so a cascade can name its first row as the root of the rest.
func (s *Store) journalScheduleChangeRowTx(tx *sql.Tx, changeID, id, change, key, role, before, after, actor, reason, source, parentID, rootID string) error {
	if rootID == "" {
		rootID = changeID
	}
	_, err := tx.Exec(s.q(`INSERT INTO schedule_changes (id,workspace_id,item_id,schedule_id,occurrence_key,change_type,role,before_json,after_json,actor,reason,reason_source,root_change_id,parent_change_id,recorded_at)
 SELECT ?,workspace_id,item_id,id,?,?,?,?,?,?,?,?,?,?,? FROM item_schedules WHERE id=?`), changeID, nilIfEmpty(key), change, nilIfEmpty(role), nilIfEmpty(before), nilIfEmpty(after), actor, reason, source, rootID, nilIfEmpty(parentID), nowNano(), id)
	if err != nil {
		return fmt.Errorf("journal schedule change: %w", err)
	}
	return nil
}

func (s *Store) journalScheduleAfterTx(tx *sql.Tx, id, change, key, role, before, actor string) error {
	after, err := s.scheduleSnapshotTx(tx, id)
	if err != nil {
		return err
	}
	return s.journalScheduleChangeTx(tx, id, change, key, role, before, after, actor)
}
