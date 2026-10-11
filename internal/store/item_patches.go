package store

import (
	"database/sql"
	"errors"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// GetItemPatch reads a stored patch (PLAN-3578 step 3) for the undo path, the
// table's only reader. Nil when there is none, or when it belongs to another
// workspace: a patch id is never an oracle across workspaces.
func (s *Store) GetItemPatch(workspaceID, patchID string) (*models.ItemPatchRecord, error) {
	var r models.ItemPatchRecord
	err := s.db.QueryRow(s.q(`SELECT id, item_id, edits FROM item_patches WHERE id = ? AND workspace_id = ?`), patchID, workspaceID).
		Scan(&r.ID, &r.ItemID, &r.Edits)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}
