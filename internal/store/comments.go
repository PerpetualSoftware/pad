package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// CreateComment adds a new comment to an item. userID is the authenticated
// user authoring the comment (empty for agent/system comments); it's stored
// as the canonical author identity for the comment-edit permission check —
// the caller passes it explicitly rather than via the request body so it
// can't be spoofed.
func (s *Store) CreateComment(workspaceID, itemID, userID string, input models.CommentCreate) (*models.Comment, error) {
	id := newID()
	ts := now()

	createdBy := input.CreatedBy
	if createdBy == "" {
		createdBy = "user"
	}
	source := input.Source
	if source == "" {
		source = "web"
	}
	author := input.Author
	if author == "" {
		author = createdBy
	}

	// Transactional so the pad-attachment: reference stamp (BUG-2415)
	// commits atomically with the body that carries the reference —
	// the orphan-GC claim must never observe one without the other.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin comment tx: %w", err)
	}
	defer tx.Rollback()

	// Stamp BEFORE the INSERT — see the ORDERING note on
	// stampAttachmentRefsTx (BUG-2415, codex round 3).
	if err := stampAttachmentRefsTx(tx, s, workspaceID, input.Body); err != nil {
		return nil, err
	}
	_, err = tx.Exec(s.q(`
		INSERT INTO comments (id, item_id, workspace_id, author, user_id, body, created_by, source, activity_id, parent_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, itemID, workspaceID, author, nilIfEmpty(userID), input.Body, createdBy, source,
		nilIfEmpty(input.ActivityID), nilIfEmpty(input.ParentID), ts, ts,
	)
	if err != nil {
		return nil, fmt.Errorf("insert comment: %w", err)
	}

	// The choke point (SPEC-3 / TASK-2658): comment.created commits with the
	// comment it describes. Read back in-tx so the payload is the stored row
	// rather than the caller's input.
	created, err := s.getCommentQ(tx, id)
	if err != nil {
		return nil, err
	}
	if err := s.emitCommentEventTx(tx, kernelevents.CommentCreated, created); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit comment: %w", err)
	}

	return s.GetComment(id)
}

// UpdateComment replaces a comment's body and bumps updated_at. The
// comments_fts_update trigger re-indexes the new body. Returns
// sql.ErrNoRows when no live comment matches. Permission (author or
// admin) is enforced by the handler, not here.
func (s *Store) UpdateComment(id, body string) (*models.Comment, error) {
	ts := now()
	// Transactional for the same BUG-2415 reason as CreateComment: the
	// new body and its pad-attachment: reference stamp commit together.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin comment tx: %w", err)
	}
	defer tx.Rollback()

	var workspaceID, bodyBefore string
	if err := tx.QueryRow(s.q(`SELECT workspace_id, body FROM comments WHERE id = ?`), id).Scan(&workspaceID, &bodyBefore); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("resolve comment workspace: %w", err)
	}
	// Stamp BEFORE the UPDATE — see the ORDERING note on
	// stampAttachmentRefsTx (BUG-2415, codex round 3).
	if err := stampAttachmentRefsTx(tx, s, workspaceID, body); err != nil {
		return nil, err
	}
	res, err := tx.Exec(s.q(`UPDATE comments SET body = ?, updated_at = ? WHERE id = ?`), body, ts, id)
	if err != nil {
		return nil, fmt.Errorf("update comment: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, sql.ErrNoRows
	}

	// Two gates, and the second one is the real no-op gate.
	//
	// The zero-row return above only catches a MISSING comment: the UPDATE
	// matches on id alone, so re-saving an identical body still touches the
	// row (updated_at moves) and still reports one row affected. An earlier
	// version of this comment claimed that path suppressed a no-op edit; it
	// does not (Codex round 4). Comparing the body is what does — and it keeps
	// comment.updated consistent with the item events, which emit only when a
	// slice the taxonomy names actually moved.
	if body != bodyBefore {
		updated, err := s.getCommentQ(tx, id)
		if err != nil {
			return nil, err
		}
		if err := s.emitCommentEventTx(tx, kernelevents.CommentUpdated, updated); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit comment update: %w", err)
	}
	return s.GetComment(id)
}

// GetComment returns a single comment by ID.
func (s *Store) GetComment(id string) (*models.Comment, error) {
	return s.getCommentQ(s.db, id)
}

// getCommentQ is GetComment against any Queryer, so a caller holding a
// transaction can read the row it just wrote.
//
// The reason is correctness before it is anything else: a read issued on s.db
// takes a DIFFERENT connection, which cannot see the transaction's uncommitted
// write. s.GetComment(id) called before COMMIT returns the pre-write row, or
// no row at all for a comment being created — so an event built from it would
// describe a state that is not the one committing. Event emission needs an
// in-tx snapshot by design, so it must have an in-tx read to get one.
//
// The pool-contention hazard BUG-2409 covers is real too but secondary here,
// and worth stating precisely rather than from memory: this store bounds
// SQLite at sqliteMaxOpenConns (16), not one connection, so a pool read from
// inside a transaction is a contention and lock-ordering risk under load, not
// an unconditional deadlock.
func (s *Store) getCommentQ(q Queryer, id string) (*models.Comment, error) {
	row := q.QueryRow(s.q(`
		SELECT c.id, c.item_id, c.workspace_id, c.author, COALESCE(c.user_id, ''), c.body,
		       c.created_by, c.source, COALESCE(c.activity_id, ''), COALESCE(c.parent_id, ''),
		       c.created_at, c.updated_at,
		       i.title, i.slug
		FROM comments c
		JOIN items i ON i.id = c.item_id
		WHERE c.id = ?`), id)

	var c models.Comment
	var createdAt, updatedAt string
	err := row.Scan(
		&c.ID, &c.ItemID, &c.WorkspaceID, &c.Author, &c.UserID, &c.Body,
		&c.CreatedBy, &c.Source, &c.ActivityID, &c.ParentID,
		&createdAt, &updatedAt,
		&c.ItemTitle, &c.ItemSlug,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get comment: %w", err)
	}
	c.CreatedAt = parseTime(createdAt)
	c.UpdatedAt = parseTime(updatedAt)
	return &c, nil
}

// commentListCols / commentAgentJoin / scanComments are the ONE read path
// for comment lists (ListComments and ListCommentsBeforeTime), so every list
// surface — the comments endpoint, `pad item comments`, the item timeline —
// carries the same shape, including Comment.AgentName.
//
// The LEFT JOIN is how a comment learns which agent wrote it. The name is
// stamped only on the `commented` activity the comment's activity_id points
// at (handlers_comments.go links them at create), and the timeline drops that
// activity from its payload because the comment card stands in for it. Doing
// the join HERE, on the comment row, makes the lookup exact by construction:
// the alternative — matching comments to activities inside the timeline
// handler — reads the two through separately paginated windows, so it misses
// at page edges and whenever other activity crowds the linked row out, and
// its failure mode is the same agent's name present on one page and absent on
// the next, indistinguishable from "no name was sent" (TASK-2760 recon).
//
// `a.metadata` is selected raw and parsed in Go (models.AgentNameFromMetadata)
// rather than via json_extract / ->>, which differ between SQLite and
// Postgres; and it is scanned through sql.NullString, not COALESCE'd, because
// on Postgres the column is jsonb and COALESCE(jsonb, ”) would try to parse
// ” as JSON. NULL (no linked activity) and an empty/stampless blob both read
// as "no name".
const commentListCols = `c.id, c.item_id, c.workspace_id, c.author, COALESCE(c.user_id, ''), c.body,
		       c.created_by, c.source, COALESCE(c.activity_id, ''), COALESCE(c.parent_id, ''),
		       c.created_at, c.updated_at, a.metadata`

const commentAgentJoin = `LEFT JOIN activities a ON a.id = c.activity_id`

func scanComments(rows *sql.Rows) ([]models.Comment, error) {
	var comments []models.Comment
	for rows.Next() {
		var c models.Comment
		var createdAt, updatedAt string
		var activityMeta sql.NullString
		if err := rows.Scan(
			&c.ID, &c.ItemID, &c.WorkspaceID, &c.Author, &c.UserID, &c.Body,
			&c.CreatedBy, &c.Source, &c.ActivityID, &c.ParentID,
			&createdAt, &updatedAt, &activityMeta,
		); err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		c.CreatedAt = parseTime(createdAt)
		c.UpdatedAt = parseTime(updatedAt)
		if activityMeta.Valid {
			c.AgentName = models.AgentNameFromMetadata(activityMeta.String)
		}
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

// ListComments returns all comments for an item, ordered chronologically.
func (s *Store) ListComments(itemID string) ([]models.Comment, error) {
	rows, err := s.db.Query(s.q(`
		SELECT `+commentListCols+`
		FROM comments c
		`+commentAgentJoin+`
		WHERE c.item_id = ?
		ORDER BY c.created_at ASC`), itemID)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	defer rows.Close()

	return scanComments(rows)
}

// ListCommentsBeforeTime returns comments for an item created before the given time,
// ordered newest-first, limited to `limit` results. Used for cursor-based timeline pagination.
//
// When beforeID is empty (first page / no cursor), the secondary id tie-breaker
// is omitted. Earlier code passed a "\xff" sentinel intended to sort after any
// UUID, but Postgres rejects that as an invalid UTF-8 byte sequence in a TEXT
// bind parameter (SQLSTATE 22021). See BUG-1086.
func (s *Store) ListCommentsBeforeTime(itemID string, before time.Time, beforeID string, limit int) ([]models.Comment, error) {
	ts := before.Format(time.RFC3339)
	const orderLimit = `ORDER BY c.created_at DESC, c.id DESC LIMIT ?`

	var rows *sql.Rows
	var err error
	if beforeID == "" {
		rows, err = s.db.Query(s.q(`
			SELECT `+commentListCols+`
			FROM comments c
			`+commentAgentJoin+`
			WHERE c.item_id = ? AND c.created_at < ?
			`+orderLimit), itemID, ts, limit)
	} else {
		rows, err = s.db.Query(s.q(`
			SELECT `+commentListCols+`
			FROM comments c
			`+commentAgentJoin+`
			WHERE c.item_id = ? AND (c.created_at < ? OR (c.created_at = ? AND c.id < ?))
			`+orderLimit), itemID, ts, ts, beforeID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list comments before time: %w", err)
	}
	defer rows.Close()

	return scanComments(rows)
}

// DeleteComment removes a comment by ID.
// DeleteComment hard-deletes a comment and emits the ref-only
// comment.deleted event in the same transaction (SPEC-3 v1.4 / TASK-2658).
//
// Transactional as of TASK-2658 — it was a bare Exec. The delete marker is
// what resolves the conflict round 7 exposed: without it, a hard-deleted
// comment's undispatched created/updated rows were the ONLY record it ever
// existed, which forced a false choice between dropping committed events
// (breaking the outbox guarantee) and delivering the deleted body forever.
// With it, the created event still delivers, the deletion is announced
// ref-only, and retention prunes both — privacy of a frozen payload is
// temporal, not achieved by deleting rows out from under a consumer.
//
// The identifiers are read BEFORE the DELETE, in-tx, because after it there is
// no row to read them from.
func (s *Store) DeleteComment(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("delete comment: %w", err)
	}
	defer tx.Rollback()

	var workspaceID, itemID string
	var parentID sql.NullString
	switch err := tx.QueryRow(s.q(`SELECT workspace_id, item_id, parent_id FROM comments WHERE id = ?`), id).
		Scan(&workspaceID, &itemID, &parentID); {
	case errors.Is(err, sql.ErrNoRows):
		return sql.ErrNoRows
	case err != nil:
		return fmt.Errorf("delete comment: read refs: %w", err)
	}

	result, err := tx.Exec(s.q("DELETE FROM comments WHERE id = ?"), id)
	if err != nil {
		return fmt.Errorf("delete comment: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}

	if err := s.emitRefOnlyDeletionTx(tx, kernelevents.CommentDeleted, workspaceID, id, itemID, parentID.String); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete comment: %w", err)
	}
	return nil
}

// CountComments returns the number of comments for an item.
func (s *Store) CountComments(itemID string) (int, error) {
	var count int
	err := s.db.QueryRow(s.q("SELECT COUNT(*) FROM comments WHERE item_id = ?"), itemID).Scan(&count)
	return count, err
}
