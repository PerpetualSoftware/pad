package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const sqliteReminderMigration = "183_reminders_require_commitment.sql"
const postgresReminderMigration = "143_reminders_require_commitment.sql"

// Collision policy pending: refusal is a placeholder and must not ship.
func resolveReminderCollisions(counts map[string]int, scheduled map[string]bool) error {
	var conflicts []string
	for item, n := range counts {
		var reasons []string
		if n > 1 {
			reasons = append(reasons, "multiple legacy reminders")
		}
		if scheduled[item] {
			reasons = append(reasons, "existing commitment")
		}
		if len(reasons) > 0 {
			conflicts = append(conflicts, item+": "+strings.Join(reasons, ", "))
		}
	}
	sort.Strings(conflicts)
	if len(conflicts) > 0 {
		return fmt.Errorf("reminder collisions: %s", strings.Join(conflicts, "; "))
	}
	return nil
}

func validateLegacyReminder(r *models.Reminder) (string, error) {
	if r.FiredAt == nil && r.AckedAt != nil {
		return "", fmt.Errorf("reminder %s: acknowledgement without fire", r.ID)
	}
	for _, field := range []struct{ name, value string }{{"remind_at", r.RemindAt}, {"created_at", r.CreatedAt}, {"updated_at", r.UpdatedAt}} {
		if _, err := time.Parse(time.RFC3339, field.value); err != nil {
			return "", fmt.Errorf("reminder %s: invalid %s", r.ID, field.name)
		}
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{"fired_at", r.FiredAt}, {"acked_at", r.AckedAt}} {
		if field.value != nil {
			if _, err := time.Parse(time.RFC3339, *field.value); err != nil {
				return "", fmt.Errorf("reminder %s: invalid %s", r.ID, field.name)
			}
		}
	}
	at, _ := normalizeRemindAt(r.RemindAt)
	if _, err := compileScheduleSpec(scheduleSpecInput{Dtstart: strings.TrimSuffix(at, "Z"), Tz: "UTC"}); err != nil {
		return "", fmt.Errorf("reminder %s: unrepresentable target", r.ID)
	}
	return at, nil
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
	counts, scheduled := map[string]int{}, map[string]bool{}
	targets := map[string]string{}
	for _, r := range reminders {
		at, err := validateLegacyReminder(r)
		if err != nil {
			return err
		}
		targets[r.ID] = at
		var owner string
		err = tx.QueryRow(s.q(`SELECT i.workspace_id FROM items i JOIN workspaces w ON w.id=i.workspace_id WHERE i.id=?`), r.ItemID).Scan(&owner)
		if err == sql.ErrNoRows || (err == nil && owner != r.WorkspaceID) {
			return fmt.Errorf("reminder %s: missing or mismatched owner", r.ID)
		}
		if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM item_schedules WHERE item_id=?`), r.ItemID).Scan(&n); err != nil {
			return err
		}
		counts[r.ItemID]++
		scheduled[r.ItemID] = n > 0
	}
	if err := resolveReminderCollisions(counts, scheduled); err != nil {
		return err
	}
	for _, r := range reminders {
		at := targets[r.ID]
		wall := strings.TrimSuffix(at, "Z")
		sc := &models.ItemSchedule{ID: newID(), WorkspaceID: r.WorkspaceID, ItemID: r.ItemID, Kind: "check", Dtstart: wall, Tz: ptr("UTC"), Anchor: "calendar", Audience: "both", RDates: []string{}, ExDates: []string{}, NextAt: ptr(at), NextWall: ptr(wall), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
		if err := s.insertScheduleTx(tx, sc, &scheduleRoles{Origin: "native"}); err != nil {
			return err
		}
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
	return s.checkReminderMigrationTx(tx)
}

func (s *Store) checkReminderMigrationTx(tx *sql.Tx) error {
	var invalid int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM item_reminders r LEFT JOIN item_schedules sc ON sc.id=r.schedule_id WHERE sc.id IS NULL OR sc.item_id<>r.item_id OR sc.workspace_id<>r.workspace_id`).Scan(&invalid); err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("reminder conversion left %d invalid attachments", invalid)
	}
	if s.dialect.Driver() == DriverSQLite {
		rows, err := tx.Query(`PRAGMA foreign_key_check`)
		if err != nil {
			return err
		}
		defer rows.Close()
		if rows.Next() {
			return fmt.Errorf("reminder conversion left foreign key violations")
		}
		return rows.Err()
	}
	return nil
}
