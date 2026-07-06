package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Hard-purge of soft-deleted workspaces (TASK-1966).
//
// Account deletion (DeleteAccountAtomic) and manual workspace deletion
// (DeleteWorkspace) both only SOFT-delete a workspace — they stamp
// workspaces.deleted_at and leave every item/comment/attachment/etc.
// row in place. The published /privacy policy promises owned workspaces
// are removed from live systems within 30 days, so a scheduled sweeper
// (internal/server/workspace_purge.go) hard-deletes soft-deleted
// workspaces past that window. These are the store-layer primitives it
// drives:
//
//   - ListPurgeableWorkspaces — the eligibility query (soft-deleted +
//     older than the cutoff). Purely additive; touches no live rows.
//   - WorkspaceAttachmentBlobs — captures every attachment row's
//     storage_key/content_hash/size BEFORE the DB purge so the server
//     can reclaim on-disk/S3 blobs afterward (the DB rows are gone by
//     then).
//   - CountAttachmentsForHashOutsideWorkspace — the content-addressed
//     dedupe guard: a blob shared by another workspace must NOT be
//     deleted when this one is purged.
//   - PurgeWorkspaceData — the transactional cascade that removes every
//     child row (in FK-dependency order) and the workspace row itself.
//
// Both dialects: SQLite runs with `_pragma=foreign_keys(on)` and
// Postgres always enforces FKs, so the delete order matters on both.
// PurgeWorkspaceData explicitly deletes every child table rather than
// leaning on ON DELETE CASCADE (most workspace FKs are RESTRICT/NO
// ACTION, which would otherwise block the parent delete) — the same
// posture DeleteAccountAtomic takes for the users FK graph.

// WorkspacePurgeCandidate identifies a workspace eligible for hard
// purge. Slug is carried for logging/observability only; ID is the key
// PurgeWorkspaceData operates on.
type WorkspacePurgeCandidate struct {
	ID   string
	Slug string
}

// ListPurgeableWorkspaces returns workspaces whose deleted_at is set and
// older than cutoff — i.e. soft-deleted long enough ago to hard-purge.
// Live workspaces (deleted_at IS NULL) are never returned, so a caller
// can never purge a workspace that is still in use.
//
// deleted_at is stored as fixed-width RFC3339 UTC (see store.now()), so
// the `< cutoff` comparison is a valid lexicographic ordering — the same
// technique SweepMCPAuditOlderThan uses for its timestamp cutoff.
func (s *Store) ListPurgeableWorkspaces(cutoff time.Time) ([]WorkspacePurgeCandidate, error) {
	cutoffStr := cutoff.UTC().Format(time.RFC3339)
	rows, err := s.db.Query(s.q(`
		SELECT id, slug
		FROM workspaces
		WHERE deleted_at IS NOT NULL AND deleted_at < ?
		ORDER BY deleted_at
	`), cutoffStr)
	if err != nil {
		return nil, fmt.Errorf("list purgeable workspaces: %w", err)
	}
	defer rows.Close()

	var out []WorkspacePurgeCandidate
	for rows.Next() {
		var c WorkspacePurgeCandidate
		if err := rows.Scan(&c.ID, &c.Slug); err != nil {
			return nil, fmt.Errorf("scan purgeable workspace: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate purgeable workspaces: %w", err)
	}
	return out, nil
}

// WorkspaceAttachmentBlobs returns every attachment row for a workspace —
// originals AND thumbnail variants, live AND soft-deleted — so the purge
// sweeper can reclaim their blobs. This deliberately does NOT filter on
// deleted_at or parent_id (unlike WorkspaceAttachments): once the
// workspace is purged, ALL its blobs must be considered for reclamation.
//
// Captured BEFORE PurgeWorkspaceData runs; afterward the rows are gone
// and the storage_key/content_hash are unrecoverable.
func (s *Store) WorkspaceAttachmentBlobs(workspaceID string) ([]models.Attachment, error) {
	rows, err := s.db.Query(s.q(`
		SELECT `+attachmentColumns+`
		FROM attachments
		WHERE workspace_id = ?
	`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspace attachment blobs: %w", err)
	}
	defer rows.Close()

	var out []models.Attachment
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan workspace attachment blob: %w", err)
		}
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workspace attachment blobs: %w", err)
	}
	return out, nil
}

// CountAttachmentsForStorageKeyOutsideWorkspace counts attachment rows
// in a DIFFERENT workspace that point at the same PHYSICAL blob
// (storage_key). Attachments are content-addressed and deduped: two
// workspaces uploading identical bytes to the same backend share one
// physical object under one storage_key. Before the purge sweeper
// deletes a blob it MUST confirm no other workspace still references
// that exact storage_key — otherwise purging workspace A strands
// workspace B's live attachment on a missing blob.
//
// Keyed on storage_key rather than content_hash so it stays correct
// under mixed backends (FS + S3): identical bytes stored in two backends
// have the same content_hash but DIFFERENT storage_keys and are distinct
// physical objects — deleting one must not be gated on rows referencing
// the other. storage_key is the physical identity; content_hash is not.
//
// Returns the count of protecting rows in OTHER workspaces; 0 means the
// physical blob is safe to delete.
func (s *Store) CountAttachmentsForStorageKeyOutsideWorkspace(storageKey, workspaceID string) (int, error) {
	var n int
	err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM attachments
		WHERE storage_key = ? AND workspace_id <> ?
	`), storageKey, workspaceID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count attachments for storage key outside workspace: %w", err)
	}
	return n, nil
}

// purgeWorkspaceChildDeletes is the FK-dependency-ordered list of
// statements that remove every workspace-scoped child row. Each query
// takes a single workspace_id placeholder. Ordering rules (both SQLite
// with foreign_keys=ON and Postgres enforce these):
//
//   - Item child rows (comments, item_versions, item_links, ...) before
//     items — several carry RESTRICT FKs to items.
//   - Collection child rows (views, collection_grants,
//     member_collection_access) before collections; items before
//     collections (items.collection_id is RESTRICT).
//   - versions before documents (versions.document_id is RESTRICT).
//   - comments before activities (comments.activity_id is RESTRICT).
//   - Everything before the workspace row (handled by PurgeWorkspaceData).
//
// Self-referential RESTRICT columns (items.parent_id, comments.parent_id)
// are NULLed first (see PurgeWorkspaceData) so a bulk DELETE can't trip
// the constraint on SQLite, which checks NO ACTION FKs immediately
// rather than at end-of-statement.
//
// mcp_audit_log.workspace_id is NULLed rather than deleted: it is a
// user-scoped security audit log (rows may belong to other users who
// accessed the workspace via MCP) under its own 90-day retention. NULLing
// clears the RESTRICT FK to workspaces while preserving the audit trail —
// the de-identify posture DeleteAccountAtomic uses for audit rows.
var purgeWorkspaceChildDeletes = []struct{ what, query string }{
	// --- item child rows (before items) ---
	{"comment reactions", `DELETE FROM comment_reactions WHERE comment_id IN (SELECT id FROM comments WHERE workspace_id = ?)`},
	{"comments", `DELETE FROM comments WHERE workspace_id = ?`},
	{"item versions", `DELETE FROM item_versions WHERE item_id IN (SELECT id FROM items WHERE workspace_id = ?)`},
	{"item links", `DELETE FROM item_links WHERE workspace_id = ?`},
	{"item stars", `DELETE FROM item_stars WHERE item_id IN (SELECT id FROM items WHERE workspace_id = ?)`},
	{"item yjs op-log", `DELETE FROM item_yjs_updates WHERE item_id IN (SELECT id FROM items WHERE workspace_id = ?)`},
	{"item wiki links", `DELETE FROM item_wiki_links WHERE source_item_id IN (SELECT id FROM items WHERE workspace_id = ?)`},
	{"item collection moves", `DELETE FROM item_collection_moves WHERE workspace_id = ?`},
	{"item grants", `DELETE FROM item_grants WHERE workspace_id = ?`},
	{"status transitions", `DELETE FROM status_transitions WHERE workspace_id = ?`},
	// --- items (self-ref parent_id NULLed by PurgeWorkspaceData first) ---
	{"items", `DELETE FROM items WHERE workspace_id = ?`},
	// --- collection child rows (before collections) ---
	{"views", `DELETE FROM views WHERE workspace_id = ?`},
	{"collection grants", `DELETE FROM collection_grants WHERE workspace_id = ?`},
	{"member collection access", `DELETE FROM member_collection_access WHERE workspace_id = ?`},
	{"collections", `DELETE FROM collections WHERE workspace_id = ?`},
	// --- documents child rows (before documents) ---
	{"document versions", `DELETE FROM versions WHERE document_id IN (SELECT id FROM documents WHERE workspace_id = ?)`},
	{"documents", `DELETE FROM documents WHERE workspace_id = ?`},
	// --- remaining workspace-direct rows ---
	{"agent roles", `DELETE FROM agent_roles WHERE workspace_id = ?`},
	{"progress snapshots", `DELETE FROM progress_snapshots WHERE workspace_id = ?`},
	{"webhooks", `DELETE FROM webhooks WHERE workspace_id = ?`},
	{"workspace invitations", `DELETE FROM workspace_invitations WHERE workspace_id = ?`},
	{"custom templates", `DELETE FROM custom_templates WHERE workspace_id = ?`},
	{"workspace-scoped api tokens", `DELETE FROM api_tokens WHERE workspace_id = ?`},
	{"share link views", `DELETE FROM share_link_views WHERE share_link_id IN (SELECT id FROM share_links WHERE workspace_id = ?)`},
	{"share links", `DELETE FROM share_links WHERE workspace_id = ?`},
	{"oauth connection workspaces", `DELETE FROM oauth_connection_workspaces WHERE workspace_id = ?`},
	{"user report layouts", `DELETE FROM user_report_layouts WHERE workspace_id = ?`},
	{"workspace members", `DELETE FROM workspace_members WHERE workspace_id = ?`},
	{"attachments", `DELETE FROM attachments WHERE workspace_id = ?`},
	{"detach mcp audit log", `UPDATE mcp_audit_log SET workspace_id = NULL WHERE workspace_id = ?`},
	{"activities", `DELETE FROM activities WHERE workspace_id = ?`},
}

// PurgeWorkspaceData hard-deletes every child row of a soft-deleted
// workspace and the workspace row itself, in a single transaction. Blob
// bytes are NOT touched here — the caller reclaims them through the
// attachment store abstraction after this commits (capture the rows via
// WorkspaceAttachmentBlobs first).
//
// SAFETY: refuses to touch a workspace that is not soft-deleted. The
// eligibility age check lives in ListPurgeableWorkspaces; this method
// re-verifies deleted_at IS NOT NULL inside the transaction as
// defense-in-depth so a misused/stale workspace ID can never destroy a
// live workspace's data. If the workspace is missing or live, it returns
// an error and rolls back without deleting anything.
func (s *Store) PurgeWorkspaceData(workspaceID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("purge workspace: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Defense-in-depth: never purge a live workspace, even if handed a
	// bad ID. deleted_at must be set.
	var deletedAt sql.NullString
	err = tx.QueryRow(s.q(`SELECT deleted_at FROM workspaces WHERE id = ?`), workspaceID).Scan(&deletedAt)
	if err == sql.ErrNoRows {
		return fmt.Errorf("purge workspace %s: not found", workspaceID)
	}
	if err != nil {
		return fmt.Errorf("purge workspace %s: read deleted_at: %w", workspaceID, err)
	}
	if !deletedAt.Valid || deletedAt.String == "" {
		return fmt.Errorf("purge workspace %s: refusing to purge live (non-soft-deleted) workspace", workspaceID)
	}

	// NULL self-referential RESTRICT columns before the bulk deletes so a
	// single-statement DELETE can't hit a parent-before-child ordering
	// violation on SQLite.
	selfRefNulls := []struct{ what, query string }{
		{"detach item parents", `UPDATE items SET parent_id = NULL WHERE workspace_id = ?`},
		{"detach comment parents", `UPDATE comments SET parent_id = NULL WHERE workspace_id = ?`},
	}
	for _, stmt := range selfRefNulls {
		if _, err := tx.Exec(s.q(stmt.query), workspaceID); err != nil {
			return fmt.Errorf("purge workspace %s: %s: %w", workspaceID, stmt.what, err)
		}
	}

	for _, stmt := range purgeWorkspaceChildDeletes {
		if _, err := tx.Exec(s.q(stmt.query), workspaceID); err != nil {
			return fmt.Errorf("purge workspace %s: %s: %w", workspaceID, stmt.what, err)
		}
	}

	// Finally the workspace row. Guard on deleted_at again so a concurrent
	// (hypothetical) un-delete between the read above and here can't let a
	// live workspace slip through.
	res, err := tx.Exec(s.q(`DELETE FROM workspaces WHERE id = ? AND deleted_at IS NOT NULL`), workspaceID)
	if err != nil {
		return fmt.Errorf("purge workspace %s: delete workspace row: %w", workspaceID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("purge workspace %s: workspace row not deleted (no longer soft-deleted?)", workspaceID)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("purge workspace %s: commit: %w", workspaceID, err)
	}
	return nil
}
