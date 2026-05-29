package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Per-user Insights/report layout preferences (PLAN-1628 / TASK-1634).

// GetReportLayout returns the user's saved layout for a workspace, or nil when
// they have none (the caller applies surface defaults).
func (s *Store) GetReportLayout(userID, workspaceID string) (*models.ReportLayout, error) {
	var configJSON string
	err := s.db.QueryRow(s.q(`
		SELECT config FROM user_report_layouts
		WHERE user_id = ? AND workspace_id = ?
	`), userID, workspaceID).Scan(&configJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get report layout: %w", err)
	}
	var layout models.ReportLayout
	if configJSON != "" {
		if err := json.Unmarshal([]byte(configJSON), &layout); err != nil {
			// A corrupt stored config shouldn't break the page — treat as
			// "no layout" and let the caller fall back to defaults.
			return nil, nil
		}
	}
	return &layout, nil
}

// SaveReportLayout upserts the user's layout for a workspace. The caller is
// responsible for sanitizing the layout (see server handler) before saving.
func (s *Store) SaveReportLayout(userID, workspaceID string, layout models.ReportLayout) error {
	configJSON, err := json.Marshal(layout)
	if err != nil {
		return fmt.Errorf("marshal report layout: %w", err)
	}
	ts := now()
	// ON CONFLICT … DO UPDATE works identically on SQLite and Postgres (same
	// `excluded` pseudo-table) — see platform_settings.go for precedent.
	_, err = s.db.Exec(s.q(`
		INSERT INTO user_report_layouts (user_id, workspace_id, config, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user_id, workspace_id) DO UPDATE SET
			config = excluded.config,
			updated_at = excluded.updated_at
	`), userID, workspaceID, string(configJSON), ts, ts)
	if err != nil {
		return fmt.Errorf("save report layout: %w", err)
	}
	return nil
}
