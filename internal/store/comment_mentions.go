package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/links"
)

// reindexCommentMentionsTx makes comment_mentions agree with a comment's
// body, inside the caller's transaction (TASK-3917, IDEA-3906).
//
// live is false for a tombstone (and for any caller that knows the comment is
// gone): every row for the comment is deleted. Otherwise the body's
// @-mentions are parsed (links.ExtractMentions), resolved to human users
// over LOWER(username) in chunks, and DIFFED against the stored rows: rows no
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
// links.MaxMentionsPerComment distinct RESOLVED names indexes only the first
// that many in sorted order (names that resolve to no user do not count; at
// most links.MaxMentionCandidates parsed names are tried, in chunks).
// `pad db migrate` (SQLite to PG) goes through ImportWorkspace, so every
// migrated comment is imported = 1 and is never indexed (users are not
// migrated either, migrated.go). The backfill resolves against the users that
// exist NOW: for a legacy row the backfill IS its write-time resolution
// (TASK-3917 r2). Usernames shorter than 3 characters (legacy) are never
// matched, and "end.@alice" reads as an email and is skipped.
//
// Every writer of comments.body or deleted_at calls this (the site guard in
// task3917_comment_mentions_test.go classifies each one); hard deletes and
// reaps rely on the comment_id ON DELETE CASCADE.
//
// It returns the users it NEWLY indexed, sorted. A create or edit enqueues a
// 'mention' notification for them (TASK-3946); the tombstone and backfill
// callers discard the list, so the enqueue lives at the call sites and never
// here.
func (s *Store) reindexCommentMentionsTx(tx *sql.Tx, commentID, workspaceID, itemID, body string, live bool, ts string) ([]string, error) {
	var names []string
	if live {
		names = links.ExtractMentions(body)
		if len(names) > links.MaxMentionCandidates {
			names = names[:links.MaxMentionCandidates]
		}
	}

	// Resolve in chunks (bounded IN-list), in sorted name order, and stop at
	// MaxMentionsPerComment RESOLVED users: unresolvable names that sort early
	// cannot evict real ones (TASK-3917 r2).
	want := map[string]bool{} // user ids the body resolves to
	resolved := 0
	for i := 0; i < len(names) && resolved < links.MaxMentionsPerComment; i += links.MentionResolveChunk {
		chunk := names[i:min(i+links.MentionResolveChunk, len(names))]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, len(chunk))
		for j, n := range chunk {
			args[j] = n
		}
		// On Postgres the rows are locked FOR KEY SHARE so a concurrent
		// eraseUserTx cannot delete a user between this read and the insert
		// below (an FK violation otherwise).
		resolveQ := `SELECT id, LOWER(username) FROM users WHERE kind = 'human' AND LOWER(username) IN (` + ph + `)`
		if s.dialect.Driver() == DriverPostgres {
			resolveQ += ` FOR KEY SHARE`
		}
		rows, err := tx.Query(s.q(resolveQ), args...)
		if err != nil {
			return nil, fmt.Errorf("resolve comment mentions: %w", err)
		}
		byName := map[string][]string{}
		for rows.Next() {
			var id, name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan comment mention user: %w", err)
			}
			byName[name] = append(byName[name], id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("resolve comment mentions: %w", err)
		}
		rows.Close()
		for _, n := range chunk {
			if resolved >= links.MaxMentionsPerComment {
				break
			}
			if ids := byName[n]; len(ids) > 0 {
				for _, id := range ids {
					want[id] = true
				}
				resolved++
			}
		}
	}

	have := map[string]bool{}
	rows, err := tx.Query(s.q(`SELECT user_id FROM comment_mentions WHERE comment_id = ?`), commentID)
	if err != nil {
		return nil, fmt.Errorf("read comment mentions: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan comment mention: %w", err)
		}
		have[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read comment mentions: %w", err)
	}
	rows.Close()

	for id := range have {
		if want[id] {
			continue
		}
		if _, err := tx.Exec(s.q(`DELETE FROM comment_mentions WHERE comment_id = ? AND user_id = ?`), commentID, id); err != nil {
			return nil, fmt.Errorf("delete comment mention: %w", err)
		}
	}
	var added []string
	for id := range want {
		if have[id] {
			continue
		}
		res, err := tx.Exec(s.q(`
			INSERT INTO comment_mentions (comment_id, user_id, item_id, workspace_id, created_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (comment_id, user_id) DO NOTHING`),
			commentID, id, itemID, workspaceID, ts)
		if err != nil {
			return nil, fmt.Errorf("insert comment mention: %w", err)
		}
		// Only a row this call wrote is new: a conflict means it was
		// already indexed, and its mention was owed then.
		if n, err := res.RowsAffected(); err != nil {
			return nil, fmt.Errorf("insert comment mention: %w", err)
		} else if n == 1 {
			added = append(added, id)
		}
	}
	sort.Strings(added)
	return added, nil
}
