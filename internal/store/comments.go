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
	// Transactional so the pad-attachment: reference stamp (BUG-2415)
	// commits atomically with the body that carries the reference —
	// the orphan-GC claim must never observe one without the other.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin comment tx: %w", err)
	}
	defer tx.Rollback()

	id, err := s.createCommentTx(tx, workspaceID, itemID, userID, input)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit comment: %w", err)
	}
	return s.GetComment(id)
}

// CreateCommentWithActivity writes the activity row a comment links to AND
// the comment in ONE transaction, so both commit or neither does (BUG-2716).
//
// The order inside is forced — comments.activity_id carries a foreign key, so
// the activity must exist before the comment can reference it — and that is
// exactly why the two could not simply be reordered at the call sites: the
// activity used to commit on its own first, and a comment failure left an
// orphan "commented" entry on the timeline with nothing behind it. Here a
// failed comment INSERT (or outbox emit) rolls the activity back with it.
//
// The activity is inserted plainly, never debounced: the callers are the two
// "commented" sites, and the debounce only ever applies to "updated". The
// item-update-with-comment site is deliberately NOT a caller — its "updated"
// activity records a write that has already committed and must survive a
// comment failure, so its current two-step order is the correct one there.
func (s *Store) CreateCommentWithActivity(workspaceID, itemID, userID string, activity models.Activity, input models.CommentCreate) (*models.Comment, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin comment tx: %w", err)
	}
	defer tx.Rollback()

	activityID, err := s.createActivityQ(tx, activity)
	if err != nil {
		return nil, fmt.Errorf("insert activity for comment: %w", err)
	}
	input.ActivityID = activityID

	id, err := s.createCommentTx(tx, workspaceID, itemID, userID, input)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit comment: %w", err)
	}
	return s.GetComment(id)
}

// createCommentTx is the body of CreateComment against a caller's
// transaction: stamp, INSERT, read back, emit. It returns the new id and
// leaves the commit to the caller.
func (s *Store) createCommentTx(tx *sql.Tx, workspaceID, itemID, userID string, input models.CommentCreate) (string, error) {
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

	// The parent must be a comment on this same item, in this workspace
	// (BUG-3346): the route authorized the item, and a parent anywhere else
	// is answered exactly like one that does not exist. A reply to a
	// tombstone is refused (BUG-3252). On Postgres the parent
	// is read FOR KEY SHARE, the lock the reply's foreign-key check takes
	// anyway, so a delete tombstoning it (FOR UPDATE) either commits first
	// and is seen here, or waits for this reply and then counts it.
	if input.ParentID != "" {
		parentQ := `SELECT CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END FROM comments WHERE id = ? AND workspace_id = ? AND item_id = ?`
		if s.dialect.Driver() == DriverPostgres {
			parentQ += ` FOR KEY SHARE`
		}
		var parentDeleted int
		switch err := tx.QueryRow(s.q(parentQ), input.ParentID, workspaceID, itemID).Scan(&parentDeleted); {
		case errors.Is(err, sql.ErrNoRows):
			return "", fmt.Errorf("insert comment: parent %s: %w", input.ParentID, sql.ErrNoRows)
		case err != nil:
			return "", fmt.Errorf("insert comment: read parent: %w", err)
		case parentDeleted == 1:
			return "", ErrCommentDeleted
		}
	}
	// Stamp BEFORE the INSERT — see the ORDERING note on
	// stampAttachmentRefsTx (BUG-2415, codex round 3).
	if err := stampAttachmentRefsTx(tx, s, workspaceID, input.Body); err != nil {
		return "", err
	}
	// Routed through the seam so a test can make the comment INSERT fail
	// after the activity row is in the same transaction (BUG-2716). Nil in
	// production. Placed at the INSERT rather than the commit because the
	// property under test is that a failed COMMENT write takes the activity
	// with it, on every path that writes one.
	if s.failCommentInsert != nil {
		if err := s.failCommentInsert(input); err != nil {
			return "", fmt.Errorf("insert comment: %w", err)
		}
	}
	_, err := tx.Exec(s.q(`
		INSERT INTO comments (id, item_id, workspace_id, author, user_id, body, created_by, source, activity_id, parent_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, itemID, workspaceID, author, nilIfEmpty(userID), input.Body, createdBy, source,
		nilIfEmpty(input.ActivityID), nilIfEmpty(input.ParentID), ts, ts,
	)
	if err != nil {
		return "", fmt.Errorf("insert comment: %w", err)
	}

	// The choke point (SPEC-3 / TASK-2658): comment.created commits with the
	// comment it describes. Read back in-tx so the payload is the stored row
	// rather than the caller's input.
	created, err := s.getCommentQ(tx, id)
	if err != nil {
		return "", err
	}
	if err := s.emitCommentEventTx(tx, kernelevents.CommentCreated, created); err != nil {
		return "", err
	}
	// The recent trail is part of the state a decision reads, so a new
	// comment makes an evaluation owed (TASK-3117).
	if err := s.enqueueDecisionJobsForItemTx(tx, itemID); err != nil {
		return "", err
	}
	return id, nil
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
	var deleted int
	lockQ := `SELECT workspace_id, body, CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END FROM comments WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		// A tombstoning delete holds this row FOR UPDATE; waiting on it
		// here means the check below sees the tombstone it commits.
		lockQ += ` FOR NO KEY UPDATE`
	}
	if err := tx.QueryRow(s.q(lockQ), id).Scan(&workspaceID, &bodyBefore, &deleted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("resolve comment workspace: %w", err)
	}
	if deleted == 1 {
		return nil, ErrCommentDeleted
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
		// An edited comment can be inside the recent trail a decision reads,
		// so an edit that changed the body is a door (TASK-3117 ruling 3:
		// user-initiated single-item writes that change hashed state). If
		// the comment is older than the trail window the runner finds the
		// state unchanged and makes no call.
		if err := s.enqueueDecisionJobsForItemTx(tx, updated.ItemID); err != nil {
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
		       CASE WHEN c.deleted_at IS NULL THEN 0 ELSE 1 END, c.imported,
		       i.title, i.slug
		FROM comments c
		JOIN items i ON i.id = c.item_id
		WHERE c.id = ?`), id)

	var c models.Comment
	var createdAt, updatedAt string
	var deleted, imported int
	err := row.Scan(
		&c.ID, &c.ItemID, &c.WorkspaceID, &c.Author, &c.UserID, &c.Body,
		&c.CreatedBy, &c.Source, &c.ActivityID, &c.ParentID,
		&createdAt, &updatedAt, &deleted, &imported,
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
	c.Deleted = deleted == 1
	c.Imported = imported == 1
	return &c, nil
}

// commentListCols / commentAgentJoin / scanComments are the ONE read path
// for comment lists (ListComments and ListCommentsBeforeTime), so every list
// surface — the comments endpoint, `pad item comments`, the item timeline —
// carries the same shape, including Comment.AgentName.
//
// The LEFT JOIN is how a comment learns which agent wrote it. The name is
// stamped only on the activity the comment's activity_id points at — the
// `commented` row a comment or reply logs (handlers_comments.go), or the
// `updated` row of an item update that carried the comment
// (handlers_items.go) — and the timeline drops that activity from its
// payload because the comment card stands in for it. The join is scoped to
// the comment's own item as well as the id: nothing in the schema forbids a
// comment pointing at another item's activity, and a name read across items
// would be wrong with no way to see it. Doing the join HERE, on the comment
// row, makes the lookup exact by construction:
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
		       c.created_at, c.updated_at,
		       CASE WHEN c.deleted_at IS NULL THEN 0 ELSE 1 END, c.imported, a.metadata`

const commentAgentJoin = `LEFT JOIN activities a ON a.id = c.activity_id AND a.document_id = c.item_id`

func scanComments(rows *sql.Rows) ([]models.Comment, error) {
	var comments []models.Comment
	for rows.Next() {
		var c models.Comment
		var createdAt, updatedAt string
		var activityMeta sql.NullString
		var deleted, imported int
		if err := rows.Scan(
			&c.ID, &c.ItemID, &c.WorkspaceID, &c.Author, &c.UserID, &c.Body,
			&c.CreatedBy, &c.Source, &c.ActivityID, &c.ParentID,
			&createdAt, &updatedAt, &deleted, &imported, &activityMeta,
		); err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		c.CreatedAt = parseTime(createdAt)
		c.UpdatedAt = parseTime(updatedAt)
		c.Deleted = deleted == 1
		c.Imported = imported == 1
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
// UUID, but a UTF8 Postgres rejects that as an invalid UTF-8 byte sequence in a
// TEXT bind parameter (SQLSTATE 22021). See BUG-1086.
//
// The UTF8 qualifier is load-bearing: a SQL_ASCII database ACCEPTS those bytes
// (BUG-2784 measured both; the table is in internal/server's bindableText
// comment). So the sentinel was not universally fatal — it was fatal on the
// encoding most deployments run, and silently fine on the other, which is
// exactly the kind of dialect divergence that makes a bug look unreproducible.
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

// ErrCommentDeleted refuses a write that addresses a tombstone (BUG-3252):
// an edit, a reply or a reaction. A tombstone has no body left to edit or
// answer; its only remaining job is to hold its replies' parent.
var ErrCommentDeleted = errors.New("this comment was deleted")

// DeleteComment deletes a comment and emits the ref-only comment.deleted
// event in the same transaction (SPEC-3 v1.4 / TASK-2658).
//
// A comment with no replies is hard-deleted. A comment that still has
// replies becomes a TOMBSTONE (BUG-3252, ruled tombstone over cascade): its
// body is blanked, its reactions go, deleted_at is set, and the row stays so
// its replies keep their parent. Author and timestamps are kept, and
// updated_at is NOT moved, so the derived edited marker reads as it did.
// Deleting a tombstone answers sql.ErrNoRows, as for a missing comment.
//
// Deleting a reply whose parent is a tombstone left with no other reply also
// hard-deletes that tombstone, in this transaction, and so on up the chain.
//
// Locking: the parent row, when there is one, is locked before this row, so
// every delete takes the two in the same order. On Postgres the target row
// is locked FOR UPDATE, which conflicts with the FOR KEY SHARE a concurrent
// reply INSERT's foreign-key check takes, so no reply can appear between the
// count and the decision. A reply deleted concurrently is seen by the count
// after the lock (READ COMMITTED reads each statement fresh), and its own
// delete reaps a tombstone only after it holds this row's lock, so the two
// serialize. SQLite needs none of it: every transaction is BEGIN IMMEDIATE.
//
// Transactional as of TASK-2658 — it was a bare Exec. The delete marker is
// what resolves the conflict round 7 exposed: without it, a hard-deleted
// comment's undispatched created/updated rows were the ONLY record it ever
// existed, which forced a false choice between dropping committed events
// (breaking the outbox guarantee) and delivering the deleted body forever.
// With it, the created event still delivers, the deletion is announced
// ref-only, and retention prunes both — privacy of a frozen payload is
// temporal, not achieved by deleting rows out from under a consumer. A
// tombstone announces the same deletion: its words are gone, its row is not.
func (s *Store) DeleteComment(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("delete comment: %w", err)
	}
	defer tx.Rollback()

	target, err := s.lockCommentForDeleteTx(tx, id)
	if err != nil {
		return err
	}
	if target == nil || target.deleted {
		return sql.ErrNoRows
	}

	// The app-projection block for comment.deleted (TASK-3389) is computed
	// HERE, before the tombstone or the delete, and before reapTombstonesTx
	// can remove the parent: its same-item parent check needs the parent row.
	var authorID sql.NullString
	var author, createdBy string
	if err := tx.QueryRow(s.q(`SELECT user_id, COALESCE(author, ''), created_by FROM comments WHERE id = ?`), id).Scan(&authorID, &author, &createdBy); err != nil {
		return fmt.Errorf("delete comment: read author: %w", err)
	}
	proj, err := s.buildCommentAppProjectionTx(tx, id, target.itemID, authorID.String, author, createdBy, target.parentID)
	if err != nil {
		return err
	}

	var replies int
	if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM comments WHERE parent_id = ?`), id).Scan(&replies); err != nil {
		return fmt.Errorf("delete comment: count replies: %w", err)
	}
	if replies > 0 {
		if _, err := tx.Exec(s.q(`DELETE FROM comment_reactions WHERE comment_id = ?`), id); err != nil {
			return fmt.Errorf("delete comment: drop reactions: %w", err)
		}
		if _, err := tx.Exec(s.q(`UPDATE comments SET body = '', deleted_at = ? WHERE id = ?`), now(), id); err != nil {
			return fmt.Errorf("delete comment: tombstone: %w", err)
		}
	} else {
		if _, err := tx.Exec(s.q(`DELETE FROM comments WHERE id = ?`), id); err != nil {
			return fmt.Errorf("delete comment: %w", err)
		}
		if err := s.reapTombstonesTx(tx, target.parentID); err != nil {
			return err
		}
	}

	if err := s.emitRefOnlyDeletionWithProjectionTx(tx, kernelevents.CommentDeleted, target.workspaceID, id, target.itemID, target.parentID, proj); err != nil {
		return err
	}
	// Deleting a comment changes the recent trail (TASK-3117 ruling 3); see
	// UpdateComment for why an out-of-window comment costs nothing.
	if err := s.enqueueDecisionJobsForItemTx(tx, target.itemID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete comment: %w", err)
	}
	return nil
}

type commentDeleteRow struct {
	workspaceID, itemID, parentID string
	deleted                       bool
}

// lockCommentForDeleteTx reads the row DeleteComment acts on. On Postgres
// it first locks the row's whole ancestor chain, root first, then the row:
// reapTombstonesTx may walk up that chain, and every delete taking the chain
// in one order is what keeps two deletes in one thread from deadlocking. It
// returns nil for a missing comment.
func (s *Store) lockCommentForDeleteTx(tx *sql.Tx, id string) (*commentDeleteRow, error) {
	// parent_id is never rewritten after insert (only a workspace purge
	// clears it), so the chain read here is the chain the reap walks.
	chain := []string{id}
	seen := map[string]bool{id: true}
	for cur := id; ; {
		var parentID sql.NullString
		err := tx.QueryRow(s.q(`SELECT parent_id FROM comments WHERE id = ?`), cur).Scan(&parentID)
		if errors.Is(err, sql.ErrNoRows) {
			if cur == id {
				return nil, nil
			}
			break
		}
		if err != nil {
			return nil, fmt.Errorf("delete comment: read parent: %w", err)
		}
		if !parentID.Valid || parentID.String == "" || seen[parentID.String] {
			break
		}
		seen[parentID.String] = true
		chain = append(chain, parentID.String)
		cur = parentID.String
	}
	var row *commentDeleteRow
	for i := len(chain) - 1; i >= 0; i-- {
		r, err := s.lockCommentRowTx(tx, chain[i])
		if err != nil {
			return nil, err
		}
		row = r
	}
	return row, nil
}

// lockCommentRowTx reads one comment row, FOR UPDATE on Postgres. It returns
// nil when the row does not exist.
func (s *Store) lockCommentRowTx(tx *sql.Tx, id string) (*commentDeleteRow, error) {
	query := `SELECT workspace_id, item_id, COALESCE(parent_id, ''),
		CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END
		FROM comments WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		query += ` FOR UPDATE`
	}
	var row commentDeleteRow
	var deleted int
	switch err := tx.QueryRow(s.q(query), id).Scan(&row.workspaceID, &row.itemID, &row.parentID, &deleted); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("delete comment: lock row: %w", err)
	}
	row.deleted = deleted == 1
	return &row, nil
}

// reapTombstonesTx hard-deletes the tombstone at id if it has no reply left,
// then does the same for its parent, and so on up. The caller has just
// deleted a reply of id, and lockCommentForDeleteTx has already locked the
// whole chain above it.
//
// A reaped tombstone emits no event of its own: its comment.deleted was
// emitted when it became a tombstone.
func (s *Store) reapTombstonesTx(tx *sql.Tx, id string) error {
	for id != "" {
		row, err := s.lockCommentRowTx(tx, id)
		if err != nil {
			return err
		}
		if row == nil || !row.deleted {
			return nil
		}
		var replies int
		if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM comments WHERE parent_id = ?`), id).Scan(&replies); err != nil {
			return fmt.Errorf("delete comment: count tombstone replies: %w", err)
		}
		if replies > 0 {
			return nil
		}
		if _, err := tx.Exec(s.q(`DELETE FROM comments WHERE id = ?`), id); err != nil {
			return fmt.Errorf("delete comment: reap tombstone: %w", err)
		}
		id = row.parentID
	}
	return nil
}

// CountComments returns the number of comments for an item. Tombstones
// (BUG-3252) are not counted: nothing is left of them to read.
func (s *Store) CountComments(itemID string) (int, error) {
	var count int
	err := s.db.QueryRow(s.q("SELECT COUNT(*) FROM comments WHERE item_id = ? AND deleted_at IS NULL"), itemID).Scan(&count)
	return count, err
}
