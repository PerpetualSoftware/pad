package store

import (
	"database/sql"
	"errors"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// ImportStatusPartial marks a workspace a bundle import kept after a data
// error once the workspace existed (TASK-896). It is the only status written.
const ImportStatusPartial = "partial"

// SetWorkspaceImportPartial records that a workspace was only partly imported,
// with a server-composed note (a fixed category, a fixed detail and a
// correlation id; never request bytes or driver text). Re-marking replaces it.
func (s *Store) SetWorkspaceImportPartial(workspaceID, note string) error {
	if s.dialect.Driver() == DriverPostgres {
		_, err := s.db.Exec(s.q(`
			INSERT INTO workspace_import_status (workspace_id, status, note, created_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (workspace_id) DO UPDATE SET status = EXCLUDED.status, note = EXCLUDED.note, created_at = EXCLUDED.created_at`),
			workspaceID, ImportStatusPartial, note, now())
		return err
	}
	_, err := s.db.Exec(`
		INSERT INTO workspace_import_status (workspace_id, status, note, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (workspace_id) DO UPDATE SET status = excluded.status, note = excluded.note, created_at = excluded.created_at`,
		workspaceID, ImportStatusPartial, note, now())
	return err
}

// GetWorkspaceImportStatus answers the workspace's import marker, or nil when
// it has none (the normal case).
func (s *Store) GetWorkspaceImportStatus(workspaceID string) (*models.WorkspaceImportStatus, error) {
	var st models.WorkspaceImportStatus
	err := s.db.QueryRow(s.q(`SELECT status, note FROM workspace_import_status WHERE workspace_id = ?`), workspaceID).
		Scan(&st.Status, &st.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// ListWorkspaceImportStatuses answers the markers for a set of workspaces,
// keyed by workspace id, in one query.
func (s *Store) ListWorkspaceImportStatuses(workspaceIDs []string) (map[string]models.WorkspaceImportStatus, error) {
	out := map[string]models.WorkspaceImportStatus{}
	if len(workspaceIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(workspaceIDs))
	ph := make([]byte, 0, len(workspaceIDs)*2)
	for i, id := range workspaceIDs {
		args[i] = id
		if i > 0 {
			ph = append(ph, ',')
		}
		ph = append(ph, '?')
	}
	rows, err := s.db.Query(s.q(`SELECT workspace_id, status, note FROM workspace_import_status WHERE workspace_id IN (`+string(ph)+`)`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var st models.WorkspaceImportStatus
		if err := rows.Scan(&id, &st.Status, &st.Note); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

// ClearWorkspaceImportStatus removes the marker (the owner kept the partial
// workspace). Clearing an absent marker is not an error.
func (s *Store) ClearWorkspaceImportStatus(workspaceID string) error {
	_, err := s.db.Exec(s.q(`DELETE FROM workspace_import_status WHERE workspace_id = ?`), workspaceID)
	return err
}
