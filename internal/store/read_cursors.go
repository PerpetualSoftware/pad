package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ReadCursor returns when userID last marked workspaceID caught up
// (TASK-3615). found is false when the user never has: the catch-up window
// then falls back to its ceiling, and reading never creates the row.
func (s *Store) ReadCursor(userID, workspaceID string) (seenAt time.Time, found bool, err error) {
	var raw string
	err = s.db.QueryRow(s.q(
		`SELECT seen_at FROM workspace_read_cursors WHERE user_id = ? AND workspace_id = ?`),
		userID, workspaceID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read cursor: %w", err)
	}
	t, err := time.Parse(decisionTimeFormat, raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read cursor: parse seen_at %q: %w", raw, err)
	}
	return t, true, nil
}

// MarkCaughtUp moves userID's cursor in workspaceID to at, creating it on
// the first call. The cursor never moves backwards: an older at (a request
// that raced a newer one) leaves the stored value alone. Returns the cursor
// as stored.
func (s *Store) MarkCaughtUp(userID, workspaceID string, at time.Time) (time.Time, error) {
	v := decisionTime(at)
	if _, err := s.db.Exec(s.q(
		`INSERT INTO workspace_read_cursors (user_id, workspace_id, seen_at) VALUES (?, ?, ?)
		 ON CONFLICT (user_id, workspace_id) DO UPDATE SET seen_at = excluded.seen_at
		 WHERE excluded.seen_at > workspace_read_cursors.seen_at`),
		userID, workspaceID, v); err != nil {
		return time.Time{}, fmt.Errorf("mark caught up: %w", err)
	}
	seen, _, err := s.ReadCursor(userID, workspaceID)
	return seen, err
}
