package store

import "fmt"

// WorkspaceContentCounts is what a workspace holds, counted with the SAME
// filters the workspace export applies (ExportWorkspace and
// workspaceAttachmentsForExportQ), so a count taken here after an import can
// be compared one-to-one with the arrays of the bundle that was imported
// (TASK-3612):
//
//   - Collections: every row, soft-deleted included, as the export carries
//     archived collections.
//   - Items: live items only.
//   - Comments: live comments on live items (a tombstone is not exported).
//   - Attachments: live ORIGINALS only; derived thumbnails carry parent_id
//     and are rebuilt on import, never shipped.
type WorkspaceContentCounts struct {
	Collections int `json:"collections"`
	Items       int `json:"items"`
	Comments    int `json:"comments"`
	Attachments int `json:"attachments"`
}

// CountWorkspaceContents counts a workspace's contents for the post-import
// check. Each count is its own statement: an import has finished writing by
// the time this runs, so the four reads see the same rows.
func (s *Store) CountWorkspaceContents(workspaceID string) (WorkspaceContentCounts, error) {
	var c WorkspaceContentCounts
	counts := []struct {
		name  string
		query string
		dest  *int
	}{
		{"collections", `SELECT COUNT(*) FROM collections WHERE workspace_id = ?`, &c.Collections},
		{"items", `SELECT COUNT(*) FROM items WHERE workspace_id = ? AND deleted_at IS NULL`, &c.Items},
		{"comments", `SELECT COUNT(*) FROM comments c JOIN items i ON c.item_id = i.id
			WHERE c.workspace_id = ? AND i.deleted_at IS NULL AND c.deleted_at IS NULL`, &c.Comments},
		{"attachments", `SELECT COUNT(*) FROM attachments
			WHERE workspace_id = ? AND deleted_at IS NULL AND parent_id IS NULL`, &c.Attachments},
	}
	for _, q := range counts {
		if err := s.db.QueryRow(s.q(q.query), workspaceID).Scan(q.dest); err != nil {
			return WorkspaceContentCounts{}, fmt.Errorf("count %s: %w", q.name, err)
		}
	}
	return c, nil
}
