package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const sqliteReminderMigration = "183_reminders_require_commitment.sql"
const postgresReminderMigration = "143_reminders_require_commitment.sql"

// resolveReminderCollisions selects one row per unscheduled item after preflight.
// State outranks chronology. Equal instants use ID for database rows and the
// later array position for bundles, whose reminder entries have no identity.
func resolveReminderCollisions(reminders []*models.Reminder, scheduled map[string]string, unreadable []string, bundle bool) map[string]int {
	winners := map[string]int{}
	rank := func(r *models.Reminder) int {
		if r.FiredAt == nil {
			return 0
		}
		if r.AckedAt == nil {
			return 1
		}
		return 2
	}
	newer := func(a, b *models.Reminder) bool {
		if rank(a) != rank(b) {
			return rank(a) < rank(b)
		}
		for _, stamps := range [][2]string{{a.RemindAt, b.RemindAt}, {a.CreatedAt, b.CreatedAt}} {
			at, _ := time.Parse(time.RFC3339, stamps[0])
			bt, _ := time.Parse(time.RFC3339, stamps[1])
			if order := at.Compare(bt); order != 0 {
				return order > 0
			}
		}
		return bundle || a.ID > b.ID
	}
	for i, r := range reminders {
		if scheduled[r.ItemID] != "" || unreadable[i] != "" {
			continue
		}
		previous, ok := winners[r.ItemID]
		if !ok || newer(r, reminders[previous]) {
			winners[r.ItemID] = i
		}
	}
	return winners
}

const legacyReminderHistoryReason = "one-shot reminder kept as history"
const legacyReminderCarrierReason = "Created ended run-once history carrier using migration time for target and creation"

// Keep every legacy column, including NULL marks, independently of the public
// Reminder JSON shape, which omits NULLs and includes derived fields.
func legacyReminderHistoryJSON(r *models.Reminder) ([]byte, error) {
	return json.Marshal(map[string]any{
		"id": r.ID, "workspace_id": r.WorkspaceID, "item_id": r.ItemID,
		"remind_at": r.RemindAt, "fired_at": r.FiredAt, "acked_at": r.AckedAt,
		"created_at": r.CreatedAt, "updated_at": r.UpdatedAt,
	})
}

// validateLegacyReminder returns the normalized target or the unreadable field.
// Unreadable data is preserved as history, never considered for winner selection.
func validateLegacyReminder(r *models.Reminder) (target, unreadable string) {
	if r.FiredAt == nil && r.AckedAt != nil {
		return "", "acked_at"
	}
	for _, field := range []struct{ name, value string }{{"remind_at", r.RemindAt}, {"created_at", r.CreatedAt}, {"updated_at", r.UpdatedAt}} {
		if _, err := time.Parse(time.RFC3339, field.value); err != nil {
			return "", field.name
		}
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{"fired_at", r.FiredAt}, {"acked_at", r.AckedAt}} {
		if field.value != nil {
			if _, err := time.Parse(time.RFC3339, *field.value); err != nil {
				return "", field.name
			}
		}
	}
	at, _ := normalizeRemindAt(r.RemindAt)
	if _, err := compileScheduleSpec(scheduleSpecInput{Dtstart: strings.TrimSuffix(at, "Z"), Tz: "UTC"}); err != nil {
		return "", "remind_at"
	}
	return at, ""
}

// prepareReminderMigrationTx runs before the constraint SQL on the migration
// transaction. The version is rechecked after exclusion, never from a stale list.
func prepareReminderMigrationTx(tx *sql.Tx, name string, d Dialect) (bool, error) {
	if name != sqliteReminderMigration && name != postgresReminderMigration {
		return false, nil
	}
	s := &Store{dialect: d}
	if d.Driver() == DriverPostgres {
		if _, err := tx.Exec(`SET LOCAL lock_timeout = '30s'`); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`LOCK TABLE items, workspaces, item_schedules, item_reminders, schedule_changes IN ACCESS EXCLUSIVE MODE`); err != nil {
			return false, err
		}
	}
	var n int
	if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM schema_migrations WHERE version=?`), name).Scan(&n); err != nil {
		return false, err
	}
	if n != 0 {
		return true, nil
	}
	return false, s.convertLegacyRemindersTx(tx)
}

func (s *Store) convertLegacyRemindersTx(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT ` + reminderColumns + ` FROM item_reminders WHERE schedule_id IS NULL ORDER BY item_id,id`)
	if err != nil {
		return err
	}
	var reminders []*models.Reminder
	for rows.Next() {
		r, err := scanReminder(rows)
		if err != nil {
			rows.Close()
			return err
		}
		reminders = append(reminders, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	scheduled := map[string]string{}
	targets := map[string]string{}
	unreadable := make([]string, len(reminders))
	counts := map[string]int{}
	migrationTime := now()
	for i, r := range reminders {
		targets[r.ID], unreadable[i] = validateLegacyReminder(r)
		if unreadable[i] != "" {
			counts[unreadable[i]]++
		}
		var owner string
		err = tx.QueryRow(s.q(`SELECT i.workspace_id FROM items i JOIN workspaces w ON w.id=i.workspace_id WHERE i.id=?`), r.ItemID).Scan(&owner)
		if err == sql.ErrNoRows || (err == nil && owner != r.WorkspaceID) {
			return fmt.Errorf("reminder %s: missing or mismatched owner", r.ID)
		}
		if err != nil {
			return err
		}
		var scheduleID, scheduleWorkspace string
		err = tx.QueryRow(s.q(`SELECT id,workspace_id FROM item_schedules WHERE item_id=?`), r.ItemID).Scan(&scheduleID, &scheduleWorkspace)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			if scheduleWorkspace != r.WorkspaceID {
				return fmt.Errorf("reminder %s: mismatched schedule owner", r.ID)
			}
			scheduled[r.ItemID] = scheduleID
		}
	}
	winners := resolveReminderCollisions(reminders, scheduled, unreadable, false)
	// A dismissed legacy winner carries history only; live commitment ack
	// semantics do not apply to reminders dismissed before the upgrade.
	converted := 0
	for i, r := range reminders {
		if winner, ok := winners[r.ItemID]; !ok || winner != i || r.AckedAt != nil {
			continue
		}
		at := targets[r.ID]
		wall := strings.TrimSuffix(at, "Z")
		sc := &models.ItemSchedule{ID: newID(), WorkspaceID: r.WorkspaceID, ItemID: r.ItemID, Kind: "check", Dtstart: wall, Tz: ptr("UTC"), Anchor: "calendar", Audience: "both", RDates: []string{}, ExDates: []string{}, NextAt: ptr(at), NextWall: ptr(wall), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
		if err := s.insertScheduleTx(tx, sc, &scheduleRoles{Origin: "native"}); err != nil {
			return err
		}
		scheduled[r.ItemID] = sc.ID
		// Target-offset bypass: the schedule above is inserted with a zero target offset.
		result, err := tx.Exec(s.q(`UPDATE item_reminders SET schedule_id=?,remind_at=? WHERE id=? AND workspace_id=? AND item_id=? AND schedule_id IS NULL`), sc.ID, at, r.ID, r.WorkspaceID, r.ItemID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("reminder %s: conversion lost identity", r.ID)
		}
		converted++
		before, err := json.Marshal(r)
		if err != nil {
			return err
		}
		after, err := s.scheduleSnapshotTx(tx, sc.ID)
		if err != nil {
			return err
		}
		if err := s.journalScheduleChangeReasonTx(tx, sc.ID, "import", at+"|"+wall, "", string(before), after, "", "Converted one-shot reminder", "system"); err != nil {
			return err
		}
		got, err := scanReminder(tx.QueryRow(s.q(`SELECT `+reminderColumns+` FROM item_reminders WHERE id=?`), r.ID))
		if err != nil {
			return err
		}
		if got.CreatedAt != r.CreatedAt || got.UpdatedAt != r.UpdatedAt || derefStr(got.FiredAt) != derefStr(r.FiredAt) || derefStr(got.AckedAt) != derefStr(r.AckedAt) {
			return fmt.Errorf("reminder %s: conversion changed lifecycle", r.ID)
		}
	}
	history := 0
	for i, r := range reminders {
		if winner, ok := winners[r.ItemID]; ok && winner == i && r.AckedAt == nil {
			continue
		}
		if scheduled[r.ItemID] == "" {
			wall := strings.TrimSuffix(migrationTime, "Z")
			sc := &models.ItemSchedule{ID: newID(), WorkspaceID: r.WorkspaceID, ItemID: r.ItemID, Kind: "check", Dtstart: wall, Tz: ptr("UTC"), Anchor: "calendar", Audience: "both", RDates: []string{}, ExDates: []string{}, EndedAt: ptr(migrationTime), EndedDismissedAt: ptr(migrationTime), CreatedAt: migrationTime, UpdatedAt: migrationTime}
			if err := s.insertScheduleTx(tx, sc, &scheduleRoles{Origin: "native"}); err != nil {
				return err
			}
			after, err := s.scheduleSnapshotTx(tx, sc.ID)
			if err != nil {
				return err
			}
			if err := s.journalScheduleChangeReasonTx(tx, sc.ID, "import", "", "", "", after, "", legacyReminderCarrierReason, "system"); err != nil {
				return err
			}
			scheduled[r.ItemID] = sc.ID
		}
		reason := legacyReminderHistoryReason
		if winner, ok := winners[r.ItemID]; ok && reminders[winner].AckedAt != nil {
			reason += ": acknowledged"
		}
		if unreadable[i] != "" {
			reason = "reminder kept as history: unreadable " + unreadable[i]
		}
		before, err := legacyReminderHistoryJSON(r)
		if err != nil {
			return err
		}
		changeID := newID()
		// History does not change the existing commitment or invalidate its undo.
		if _, err := tx.Exec(s.q(`INSERT INTO schedule_changes (id,workspace_id,item_id,schedule_id,change_type,before_json,actor,reason,reason_source,root_change_id,recorded_at)
 VALUES (?,?,?,?,'import',?,'',?,'system',?,?)`), changeID, r.WorkspaceID, r.ItemID, scheduled[r.ItemID], string(before), reason, changeID, nowNano()); err != nil {
			return err
		}
		result, err := tx.Exec(s.q(`DELETE FROM item_reminders WHERE id=? AND workspace_id=? AND item_id=? AND schedule_id IS NULL`), r.ID, r.WorkspaceID, r.ItemID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("reminder %s: history lost identity", r.ID)
		}
		history++
	}
	// History includes acknowledged winners as well as unreadable and losing rows.
	if converted+history != len(reminders) {
		return fmt.Errorf("reminder conversion count mismatch")
	}

	if err := s.checkReminderMigrationTx(tx); err != nil {
		return err
	}
	if len(counts) > 0 {
		slog.Info("reminder migration kept unreadable rows as history", "remind_at", counts["remind_at"], "created_at", counts["created_at"], "updated_at", counts["updated_at"], "fired_at", counts["fired_at"], "acked_at", counts["acked_at"])
	}
	return nil
}

func (s *Store) checkReminderMigrationTx(tx *sql.Tx) error {
	var invalid int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM item_reminders r LEFT JOIN item_schedules sc ON sc.id=r.schedule_id WHERE sc.id IS NULL OR sc.item_id<>r.item_id OR sc.workspace_id<>r.workspace_id`).Scan(&invalid); err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("reminder conversion left %d invalid attachments", invalid)
	}
	// SQLite FK integrity is checked against the pre-conversion baseline by
	// applySQLiteMigration; legacy violations must not block this upgrade.
	return nil
}
