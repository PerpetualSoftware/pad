package store

import (
	"fmt"
	"time"

	"github.com/xarmian/pad/internal/models"
)

// CreateSnapshot inserts a new progress snapshot.
func (s *Store) CreateSnapshot(snap models.ProgressSnapshot) error {
	_, err := s.db.Exec(s.q(`
		INSERT INTO progress_snapshots (id, workspace_id, total_tasks, done_tasks, open_tasks, in_progress, percentage, phase_data, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		newID(), snap.WorkspaceID, snap.TotalTasks, snap.DoneTasks, snap.OpenTasks, snap.InProgress, snap.Percentage, snap.PlanData, now(),
	)
	if err != nil {
		return fmt.Errorf("insert snapshot: %w", err)
	}
	return nil
}

// ListSnapshots returns snapshots for a workspace, ordered chronologically.
func (s *Store) ListSnapshots(workspaceID string, params models.SnapshotListParams) ([]models.ProgressSnapshot, error) {
	query := `
		SELECT id, workspace_id, total_tasks, done_tasks, open_tasks, in_progress, percentage, phase_data, created_at
		FROM progress_snapshots
		WHERE workspace_id = ?
	`
	args := []interface{}{workspaceID}

	if params.Since != nil {
		query += " AND created_at >= ?"
		args = append(args, params.Since.UTC().Format(time.RFC3339))
	}
	if params.Until != nil {
		query += " AND created_at <= ?"
		args = append(args, params.Until.UTC().Format(time.RFC3339))
	}

	query += " ORDER BY created_at ASC"

	if params.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", params.Limit)
	}

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	defer rows.Close()

	var snapshots []models.ProgressSnapshot
	for rows.Next() {
		var snap models.ProgressSnapshot
		var createdAt string
		if err := rows.Scan(&snap.ID, &snap.WorkspaceID, &snap.TotalTasks, &snap.DoneTasks, &snap.OpenTasks, &snap.InProgress, &snap.Percentage, &snap.PlanData, &createdAt); err != nil {
			return nil, fmt.Errorf("scan snapshot: %w", err)
		}
		snap.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		snapshots = append(snapshots, snap)
	}
	return snapshots, rows.Err()
}

// LatestSnapshot returns the most recent snapshot for a workspace, or nil.
func (s *Store) LatestSnapshot(workspaceID string) (*models.ProgressSnapshot, error) {
	var snap models.ProgressSnapshot
	var createdAt string
	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, total_tasks, done_tasks, open_tasks, in_progress, percentage, phase_data, created_at
		FROM progress_snapshots
		WHERE workspace_id = ?
		ORDER BY created_at DESC
		LIMIT 1`),
		workspaceID,
	).Scan(&snap.ID, &snap.WorkspaceID, &snap.TotalTasks, &snap.DoneTasks, &snap.OpenTasks, &snap.InProgress, &snap.Percentage, &snap.PlanData, &createdAt)

	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("latest snapshot: %w", err)
	}
	snap.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &snap, nil
}

// DeleteOldSnapshots removes snapshots older than the given time.
func (s *Store) DeleteOldSnapshots(workspaceID string, olderThan time.Time) (int64, error) {
	result, err := s.db.Exec(s.q(`
		DELETE FROM progress_snapshots
		WHERE workspace_id = ? AND created_at < ?`),
		workspaceID, olderThan.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("delete old snapshots: %w", err)
	}
	return result.RowsAffected()
}
