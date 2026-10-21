package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// BackfillCommentMentionsResult reports one BackfillCommentMentions pass.
type BackfillCommentMentionsResult struct {
	Skipped         bool // the completion marker was already set
	CommentsScanned int
	Errors          int
}

// commentMentionsBackfilledFlag is the platform_settings key recording that a
// full comment_mentions derivation has completed (the
// relation_links_backfilled pattern).
const commentMentionsBackfilledFlag = "comment_mentions_backfilled"

// BackfillCommentMentions derives comment_mentions for comments written
// before the table existed (TASK-3917, IDEA-3906). It scans live comments
// whose body contains "@" and reindexes each in its own transaction that
// RE-READS the body (FOR NO KEY UPDATE on Postgres, UpdateComment's lock), so
// a pass never writes rows from a stale snapshot over a concurrent edit.
// Ids only are held in memory. Imported comments are skipped (imported = 0),
// as at import and on edit: TASK-3917 lead ruling bb0c9fc6. A failed comment
// is logged with its id and counted. Names resolve against the users that
// exist now: for a legacy row the backfill IS the write-time resolution.
//
// The marker is written only after a full pass with no failed comment, so an
// interrupted or partly failed run is retried at the next boot. A second run
// after completion is a no-op. Resolution is as at write time: names resolve
// against the users that exist now.
func (s *Store) BackfillCommentMentions() (*BackfillCommentMentionsResult, error) {
	result := &BackfillCommentMentionsResult{}

	var marker string
	err := s.db.QueryRow(s.q(`SELECT value FROM platform_settings WHERE key = ?`), commentMentionsBackfilledFlag).Scan(&marker)
	if err == nil && marker == "1" {
		result.Skipped = true
		return result, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("probe comment mentions backfill marker: %w", err)
	}

	rows, err := s.db.Query(s.q(`SELECT id FROM comments WHERE deleted_at IS NULL AND imported = 0 AND body LIKE '%@%'`))
	if err != nil {
		return nil, fmt.Errorf("scan comments for mentions backfill: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan mentions backfill row: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate mentions backfill rows: %w", err)
	}
	rows.Close()

	for _, id := range ids {
		result.CommentsScanned++
		if err := s.backfillCommentMentionsOne(id); err != nil {
			result.Errors++
			slog.Warn("comment mentions backfill: comment failed", "comment_id", id, "error", err)
		}
	}
	if result.Errors > 0 {
		return result, fmt.Errorf("comment mentions backfill: %d of %d comments failed; marker not set", result.Errors, len(ids))
	}

	if _, err := s.db.Exec(s.q(`
		INSERT INTO platform_settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`), commentMentionsBackfilledFlag, "1", now()); err != nil {
		return nil, fmt.Errorf("persist comment mentions backfill marker: %w", err)
	}
	return result, nil
}

func (s *Store) backfillCommentMentionsOne(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	q := `SELECT workspace_id, item_id, body, CASE WHEN deleted_at IS NULL THEN 1 ELSE 0 END, created_at, imported FROM comments WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		q += ` FOR NO KEY UPDATE`
	}
	var workspaceID, itemID, body, createdAt string
	var live, imported int
	if err := tx.QueryRow(s.q(q), id).Scan(&workspaceID, &itemID, &body, &live, &createdAt, &imported); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // deleted since the scan
		}
		return err
	}
	if imported == 1 {
		return nil // never indexed (TASK-3917 lead ruling bb0c9fc6)
	}
	// The added list is discarded: a backfill notifies nobody (TASK-3946).
	if _, err := s.reindexCommentMentionsTx(tx, id, workspaceID, itemID, body, live == 1, createdAt); err != nil {
		return err
	}
	return tx.Commit()
}
