package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func (s *Store) reminderQ(q Queryer, workspaceID, id string) (*models.Reminder, error) {
	r, err := scanReminder(q.QueryRow(s.q(`SELECT `+reminderColumns+` FROM item_reminders WHERE workspace_id=? AND id=? AND `+reminderOwned), workspaceID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := s.fillOccurrenceAt(q, r); err != nil {
		return nil, err
	}
	return r, nil
}

// simpleReminderCommitmentQ checks capabilities, not provenance. A native
// start-only commitment has the same compatibility operations as a converted one.
func (s *Store) simpleReminderCommitmentQ(q Queryer, sc *models.ItemSchedule) (bool, error) {
	if sc == nil || sc.Floating || sc.Tz == nil || len(sc.Dtstart) <= 10 || derefStr(sc.Rrule) != "" || len(sc.RDates) > 0 || len(sc.ExDates) > 0 || sc.Anchor != "calendar" || sc.LeadMinutes != 0 || sc.Dtend != nil {
		return false, nil
	}
	roles, err := s.scheduleRolesOf(q, sc)
	if err != nil {
		return false, err
	}
	if roles.TargetDays != 0 || roles.TargetSecs != 0 || roles.EarliestDays != nil || roles.DeadlineDays != nil || roles.OccupiedDays != nil {
		return false, nil
	}
	var n int
	err = q.QueryRow(s.q(`SELECT (SELECT COUNT(*) FROM schedule_links WHERE parent_schedule_id=? OR child_schedule_id=?) + (SELECT COUNT(*) FROM schedule_gates WHERE blocked_item_id=? OR blocker_item_id=?)`), sc.ID, sc.ID, sc.ItemID, sc.ItemID).Scan(&n)
	return n == 0, err
}

// ReminderCompatibility checks the shape for HTTP error precedence. Every
// mutation checks it again while holding the item, workspace and schedule.
func (s *Store) ReminderCompatibility(workspaceID, itemID string) error {
	sc, err := s.GetSchedule(workspaceID, itemID)
	if err != nil {
		return err
	}
	ok, err := s.simpleReminderCommitmentQ(s.db, sc)
	if err != nil {
		return err
	}
	if !ok {
		return ErrReminderManaged
	}
	return nil
}

// lockReminderCommitment takes the strongest locks first, then checks both
// identities. A cached alarm ID cannot address a replacement commitment.
func (s *Store) lockReminderCommitment(workspaceID, id string) (*sql.Tx, *models.ItemSchedule, *models.Reminder, error) {
	expected, err := s.GetReminder(workspaceID, id)
	if err != nil || expected == nil {
		return nil, nil, nil, err
	}
	if s.beforeCommitmentPin != nil {
		s.beforeCommitmentPin()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, nil, err
	}
	fail := func(err error) (*sql.Tx, *models.ItemSchedule, *models.Reminder, error) {
		tx.Rollback()
		return nil, nil, nil, err
	}
	if err := s.pinScheduleTx(tx, workspaceID, expected.ItemID); err != nil {
		if errors.Is(err, ErrScheduleNotFound) {
			return fail(nil)
		}
		return fail(err)
	}
	sc, err := s.getScheduleQ(tx, workspaceID, expected.ItemID, false)
	if err != nil {
		return fail(err)
	}
	if sc == nil || expected.ScheduleID == nil || sc.ID != *expected.ScheduleID || sc.ReminderID == nil || *sc.ReminderID != id {
		return fail(nil)
	}
	ok, err := s.simpleReminderCommitmentQ(tx, sc)
	if err != nil {
		return fail(err)
	}
	if !ok {
		return fail(ErrReminderManaged)
	}
	r, err := s.reminderQ(tx, workspaceID, id)
	if err != nil {
		return fail(err)
	}
	return tx, sc, r, nil
}

// RearmReminder retimes and rearms even an unchanged or already-passed target.
func (s *Store) RearmReminder(workspaceID, id, remindAt string) (*models.Reminder, error) {
	at, err := normalizeRemindAt(remindAt)
	if err != nil {
		return nil, err
	}
	if _, err := compileScheduleSpec(scheduleSpecInput{Dtstart: at[:len(at)-1], Tz: "UTC"}); err != nil {
		return nil, err
	}
	tx, sc, _, err := s.lockReminderCommitment(workspaceID, id)
	if err != nil || tx == nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := s.retargetSimpleReminderTx(tx, sc, at)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r, nil
}

// retargetSimpleReminderTx requires the caller to pin and check the simple
// run-once shape. An ended commitment has no alarm, so rearming creates one.
func (s *Store) retargetSimpleReminderTx(tx *sql.Tx, sc *models.ItemSchedule, at string) (*models.Reminder, error) {
	before, err := s.scheduleSnapshotTx(tx, sc.ID)
	if err != nil {
		return nil, err
	}
	wall := at[:len(at)-1]
	if _, err := tx.Exec(s.q(`UPDATE item_schedules SET dtstart=?,tz='UTC',floating=?,next_at=?,next_wall=?,next_override_at=NULL,anchor_wall=NULL,ended_at=NULL,ended_dismissed_at=NULL,error_code=NULL,updated_at=? WHERE id=?`), wall, s.dialect.BoolToInt(false), at, wall, now(), sc.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(s.q(`DELETE FROM schedule_occurrence_overrides WHERE schedule_id=?`), sc.ID); err != nil {
		return nil, err
	}
	if sc.ReminderID == nil {
		if err := s.insertManagedReminderTx(tx, sc.WorkspaceID, sc.ItemID, sc.ID, at); err != nil {
			return nil, err
		}
	} else if _, err := s.rearmManagedReminderTx(tx, sc.ID, at); err != nil {
		return nil, err
	}
	if err := s.journalScheduleAfterTx(tx, sc.ID, "update", at+"|"+wall, "target", before, "system"); err != nil {
		return nil, err
	}
	cur, err := s.getScheduleQ(tx, sc.WorkspaceID, sc.ItemID, false)
	if err != nil {
		return nil, err
	}
	return s.reminderQ(tx, sc.WorkspaceID, *cur.ReminderID)
}

func (s *Store) AckReminder(workspaceID, id string) (*models.Reminder, error) {
	r, _, err := s.AckReminderWithActor(workspaceID, id, ResolveInput{})
	return r, err
}

// acknowledgeLegacyReminderTx is the compatibility policy for legacy ack.
// It acknowledges outstanding work without resolving it as done or skipped.
func (s *Store) acknowledgeLegacyReminderTx(tx *sql.Tx, sc *models.ItemSchedule, in ResolveInput) (*ResolveResult, error) {
	at, err := time.Parse(time.RFC3339, derefStr(sc.NextAt))
	if err != nil {
		return nil, err
	}
	return s.ackOccurrenceTx(tx, sc, in, at, time.Now())
}

func (s *Store) AckReminderWithActor(workspaceID, id string, in ResolveInput) (*models.Reminder, bool, error) {
	tx, sc, _, err := s.lockReminderCommitment(workspaceID, id)
	if err != nil || tx == nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if sc.ErrorCode != nil {
		return nil, false, &ErrScheduleBroken{Code: *sc.ErrorCode}
	}
	res, err := s.acknowledgeLegacyReminderTx(tx, sc, in)
	if err != nil {
		return nil, false, err
	}
	r, err := s.reminderQ(tx, workspaceID, id)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return r, res.AckChanged, nil
}

// DeleteReminder uses the checked commitment deletion under the same locks and
// expected alarm identity as rearm and acknowledgement.
func (s *Store) DeleteReminder(workspaceID, id string) (bool, error) {
	tx, sc, _, err := s.lockReminderCommitment(workspaceID, id)
	if err != nil || tx == nil {
		return false, err
	}
	defer tx.Rollback()
	if err := s.deleteScheduleCheckedTx(tx, sc); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
