package store

import (
	"database/sql"
	"fmt"
	"time"
)

func (s *Store) upsertScheduleOverrideTx(tx *sql.Tx, id, key, role string, at time.Time, wall *string) error {
	ts := nowNano()
	_, err := tx.Exec(s.q(`INSERT INTO schedule_occurrence_overrides (id,schedule_id,occurrence_key,role,value_at,value_wall,created_at,updated_at)
 VALUES (?,?,?,?,?,?,?,?) ON CONFLICT (schedule_id,occurrence_key,role) DO UPDATE SET value_at=excluded.value_at,value_wall=excluded.value_wall,updated_at=excluded.updated_at`), newID(), id, key, role, NormalizeInstant(at), nullStr(wall), ts, ts)
	if err != nil {
		return fmt.Errorf("override schedule role: %w", err)
	}
	return nil
}
