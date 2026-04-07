package models

import "time"

// ProgressSnapshot stores a point-in-time snapshot of task progress
// for a workspace, used for burndown charts and velocity tracking.
type ProgressSnapshot struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	TotalTasks  int       `json:"total_tasks"`
	DoneTasks   int       `json:"done_tasks"`
	OpenTasks   int       `json:"open_tasks"`
	InProgress  int       `json:"in_progress"`
	Percentage  float64   `json:"percentage"`
	PlanData    string    `json:"phase_data"` // JSON array of per-plan snapshots (legacy DB column name: phase_data)
	CreatedAt   time.Time `json:"created_at"`
}

// PlanSnapshot is a single entry in the PlanData JSON array.
type PlanSnapshot struct {
	Title      string  `json:"title"`
	Done       int     `json:"done"`
	Total      int     `json:"total"`
	Percentage float64 `json:"percentage"`
	Status     string  `json:"status"`
}

// SnapshotListParams controls filtering when listing snapshots.
type SnapshotListParams struct {
	Since *time.Time
	Until *time.Time
	Limit int
}
