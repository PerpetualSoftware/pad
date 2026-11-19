package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// ErrCommitmentRevisionStale: the caller's expected_revision is not the
// commitment's current one. Deliberately NOT wrapping ErrCommitmentChanged:
// the graph mapper answers that sentinel with details field "revision".
var ErrCommitmentRevisionStale = errors.New("the commitment changed since it was read; read it again and retry")

// commitmentRevisionColumns is every column of item_schedules, in a fixed order.
// TestCommitmentRevisionCoversEveryScheduleColumn pins it to the table.
var commitmentRevisionColumns = []string{
	"id", "workspace_id", "item_id", "kind", "dtstart", "dtend", "tz", "floating", "rrule", "rdates", "exdates",
	"anchor", "audience", "lead_minutes", "next_at", "next_wall", "next_override_at", "anchor_wall", "ended_at",
	"error_code", "ended_dismissed_at", "created_at", "updated_at",
	"target_days", "target_secs", "earliest_days", "earliest_secs", "deadline_days", "deadline_secs",
	"deadline_behavior", "occupied_days", "occupied_secs", "outcome", "roles_origin",
}

// commitmentRevisionQ is the commitment_revision of one schedule: a SHA-256
// over every column of its item_schedules row and every one of its occurrence
// overrides, base64url without padding (43 characters). Reminder fire and
// acknowledgement state lives on item_reminders and is left out, and nothing
// time-derived goes in, so the token moves only on a write to those rows.
func (s *Store) commitmentRevisionQ(q Queryer, scheduleID string) (string, error) {
	vals := make([]sql.NullString, len(commitmentRevisionColumns))
	dest := make([]any, len(vals))
	for i := range vals {
		dest[i] = &vals[i]
	}
	query := `SELECT ` + strings.Join(commitmentRevisionColumns, ",") + ` FROM item_schedules WHERE id = ?`
	if err := q.QueryRow(s.q(query), scheduleID).Scan(dest...); err != nil {
		return "", fmt.Errorf("commitment revision: schedule: %w", err)
	}
	row := nullStrings(vals)

	rows, err := q.Query(s.q(`SELECT occurrence_key, role, value_at, value_wall, created_at, updated_at
		FROM schedule_occurrence_overrides WHERE schedule_id = ? ORDER BY occurrence_key, role`), scheduleID)
	if err != nil {
		return "", fmt.Errorf("commitment revision: overrides: %w", err)
	}
	defer rows.Close()
	overrides := [][]*string{}
	for rows.Next() {
		o := make([]sql.NullString, 6)
		if err := rows.Scan(&o[0], &o[1], &o[2], &o[3], &o[4], &o[5]); err != nil {
			return "", fmt.Errorf("commitment revision: overrides: %w", err)
		}
		overrides = append(overrides, nullStrings(o))
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("commitment revision: overrides: %w", err)
	}

	b, err := json.Marshal(struct {
		V         int         `json:"v"`
		Row       []*string   `json:"row"`
		Overrides [][]*string `json:"overrides"`
	}{1, row, overrides})
	if err != nil {
		return "", fmt.Errorf("commitment revision: encode: %w", err)
	}
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func nullStrings(in []sql.NullString) []*string {
	out := make([]*string, len(in))
	for i, v := range in {
		if v.Valid {
			s := v.String
			out[i] = &s
		}
	}
	return out
}

// GetScheduleRevision reads the schedule and its commitment_revision in one
// read-only snapshot (beginReadSnapshot: REPEATABLE READ on Postgres, a read tx
// on SQLite). (nil, "", nil) when the item has none.
func (s *Store) GetScheduleRevision(workspaceID, itemID string) (*models.ItemSchedule, string, error) {
	tx, err := s.beginReadSnapshot(context.Background())
	if err != nil {
		return nil, "", fmt.Errorf("get schedule revision: %w", err)
	}
	defer tx.Rollback()
	sc, err := s.getScheduleQ(tx, workspaceID, itemID, false)
	if err != nil || sc == nil {
		return nil, "", err
	}
	rev, err := s.commitmentRevisionQ(tx, sc.ID)
	if err != nil {
		return nil, "", err
	}
	return sc, rev, nil
}
