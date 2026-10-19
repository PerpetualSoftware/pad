package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/links"
)

// reindexCommentMentionsTx makes comment_mentions agree with a comment's
// body, inside the caller's transaction (TASK-3917, IDEA-3906).
//
// live is false for a tombstone (and for any caller that knows the comment is
// gone): every row for the comment is deleted. Otherwise the body's
// @-mentions are parsed (links.ExtractMentions), resolved to human users in
// ONE query over LOWER(username), and DIFFED against the stored rows: rows no
// longer mentioned are deleted, new ones inserted, and a mention that
// survives an edit keeps its original created_at.
//
// Resolution happens at WRITE time. A later username rename does not
// reindex, so a row names the user the handle resolved to when the comment
// was written or last edited. App principals (kind='app') are never
// indexed, an unknown name is skipped, and disabled users and self-mentions
// are indexed as-is: eligibility belongs to whoever routes on the index.
//
// ts is the created_at of NEW rows: the comment's created_at on create and
// in the backfill, the edit time on an edit (kept rows are unchanged).
// Pass "" with live=false.
//
// Known limits, by decision (TASK-3917 r1): imported comments are NEVER
// indexed, by import, by the backfill (imported = 0) or by an edit of an
// imported comment (the reindex covers the whole body, so unedited
// source-instance handles would bind): the caller skips the helper (TASK-3917
// lead ruling bb0c9fc6). Accepted cost (TASK-3917 lead ruling, day 20): a
// mention a local user ADDS while editing an imported comment is not indexed
// either. A comment mentioning more than
// links.MaxMentionsPerComment distinct names indexes only the first that many
// in sorted order. Usernames shorter than 3 characters (legacy) are never
// matched, and "end.@alice" reads as an email and is skipped.
//
// Every writer of comments.body or deleted_at calls this (the site guard in
// task3917_comment_mentions_test.go classifies each one); hard deletes and
// reaps rely on the comment_id ON DELETE CASCADE.
func (s *Store) reindexCommentMentionsTx(tx *sql.Tx, commentID, workspaceID, itemID, body string, live bool, ts string) error {
	var names []string
	if live {
		names = links.ExtractMentions(body)
		if len(names) > links.MaxMentionsPerComment {
			names = names[:links.MaxMentionsPerComment]
		}
	}

	want := map[string]bool{} // user ids the body resolves to
	if len(names) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
		args := make([]any, len(names))
		for i, n := range names {
			args[i] = n
		}
		// On Postgres the rows are locked FOR KEY SHARE so a concurrent
		// eraseUserTx cannot delete a user between this read and the insert
		// below (an FK violation otherwise).
		resolveQ := `SELECT id FROM users WHERE kind = 'human' AND LOWER(username) IN (` + ph + `)`
		if s.dialect.Driver() == DriverPostgres {
			resolveQ += ` FOR KEY SHARE`
		}
		rows, err := tx.Query(s.q(resolveQ), args...)
		if err != nil {
			return fmt.Errorf("resolve comment mentions: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("scan comment mention user: %w", err)
			}
			want[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("resolve comment mentions: %w", err)
		}
		rows.Close()
	}

	have := map[string]bool{}
	rows, err := tx.Query(s.q(`SELECT user_id FROM comment_mentions WHERE comment_id = ?`), commentID)
	if err != nil {
		return fmt.Errorf("read comment mentions: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan comment mention: %w", err)
		}
		have[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read comment mentions: %w", err)
	}
	rows.Close()

	for id := range have {
		if want[id] {
			continue
		}
		if _, err := tx.Exec(s.q(`DELETE FROM comment_mentions WHERE comment_id = ? AND user_id = ?`), commentID, id); err != nil {
			return fmt.Errorf("delete comment mention: %w", err)
		}
	}
	for id := range want {
		if have[id] {
			continue
		}
		if _, err := tx.Exec(s.q(`
			INSERT INTO comment_mentions (comment_id, user_id, item_id, workspace_id, created_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (comment_id, user_id) DO NOTHING`),
			commentID, id, itemID, workspaceID, ts); err != nil {
			return fmt.Errorf("insert comment mention: %w", err)
		}
	}
	return nil
}
