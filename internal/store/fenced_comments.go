package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// App comment writes inside a FencedTx (SPEC-6 U2b, DOC-3371 §4; TASK-3391).
//
// Compared with the human comment writes:
//   - the item must be a live companion item, and a comment addressed through
//     an item must BE on that item, or it is answered as missing;
//   - edit and delete are author-only with no admin bypass: the comment's
//     user AND the install that wrote it must both be the caller's;
//   - a pad-attachment: reference must name a live attachment already bound
//     to the comment's own item (checkItemAttachmentRefs). Nothing is
//     stamped; the human stamp (stampAttachmentRefsTx) is never reached;
//   - a delete refuses a thread whose ancestor chain leaves the item (a
//     legacy cross-item parent), because the tombstone reap walks that chain.
//
// LOCK ORDER. Each method takes the workspace seq lock FIRST, as the U2a item
// writes do. Every human path that moves, deletes or restores an item takes
// it first too, so the companion check cannot be invalidated before commit
// (codex round 3: an unlocked check let a concurrent move into a private
// collection land under an app edit). Then the comment rows (root first on a
// delete). No attachment row is locked.

// FencedCommentCreate is an app comment, already validated by appstore.
type FencedCommentCreate struct {
	ItemID   string
	ParentID string // optional: a live comment on the same item
	Body     string
	Author   string // display name; the actor kind when empty, as on the human path
	Actor    FencedActor
}

var (
	// ErrAppCommentNotFound: no live comment of that id on that item. A
	// comment on another item, or a missing parent, answers the same.
	ErrAppCommentNotFound = errors.New("app write: comment not found")
	// ErrAppNotCommentAuthor: the comment exists on the item but this install,
	// acting as this user, did not write it.
	ErrAppNotCommentAuthor = errors.New("app write: not the comment's author")
	// ErrAppAttachmentUnavailable: a pad-attachment: reference names no live
	// attachment bound to the comment's item. Unknown, foreign and unbound
	// ids answer the same, so the refusal is not an existence oracle.
	ErrAppAttachmentUnavailable = errors.New("app write: attachment unavailable")
	// ErrAppCrossItemThread: the comment's ancestor chain leaves its item, so
	// a delete could reap a comment on another item.
	ErrAppCrossItemThread = errors.New("app write: comment thread crosses items")
)

// appAttachmentRefRE finds references the way the human matcher does
// (attachmentRefRE), case-insensitively: every spelling the human path would
// stamp is checked here, and a spelling it would not is checked too.
var appAttachmentRefRE = regexp.MustCompile(`(?i)pad-attachment:([0-9A-Za-z][0-9A-Za-z_-]*)`)

// CreateComment writes an app comment and its "commented" activity, linked as
// CreateCommentWithActivity links them.
func (f *FencedTx) CreateComment(in FencedCommentCreate) (*models.Comment, error) {
	if !in.Actor.valid() {
		return nil, ErrAppBadActor
	}
	if err := f.LockWorkspaceSeq(); err != nil {
		return nil, err
	}
	if _, err := f.requireCompanionItem(in.ItemID); err != nil {
		return nil, err
	}
	if in.ParentID != "" {
		// The human parent check, verbatim: same item, same workspace, live;
		// FOR KEY SHARE on Postgres so a concurrent tombstone is seen.
		parentQ := `SELECT CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END FROM comments WHERE id = ? AND workspace_id = ? AND item_id = ?`
		if f.s.dialect.Driver() == DriverPostgres {
			parentQ += ` FOR KEY SHARE`
		}
		var parentDeleted int
		switch err := f.tx.QueryRow(f.s.q(parentQ), in.ParentID, f.workspaceID, in.ItemID).Scan(&parentDeleted); {
		case errors.Is(err, sql.ErrNoRows):
			return nil, ErrAppCommentNotFound
		case err != nil:
			return nil, fmt.Errorf("fenced comment: read parent: %w", err)
		case parentDeleted == 1:
			return nil, ErrCommentDeleted
		}
	}
	if err := f.checkItemAttachmentRefs(in.ItemID, in.Body); err != nil {
		return nil, err
	}

	activityID, err := f.s.createActivityQ(f.tx, models.Activity{
		WorkspaceID: f.workspaceID, DocumentID: in.ItemID, Action: "commented",
		Actor: in.Actor.Kind, Source: appSource, Metadata: agentMeta(in.Actor.AgentName),
		UserID: in.Actor.UserID, ViaApp: f.installID,
	})
	if err != nil {
		return nil, fmt.Errorf("fenced comment activity: %w", err)
	}
	author := in.Author
	if author == "" {
		author = in.Actor.Kind
	}
	id := newID()
	ts := now()
	if _, err := f.tx.Exec(f.s.q(`
		INSERT INTO comments (id, item_id, workspace_id, author, user_id, body, created_by, source, activity_id, parent_id, created_at, updated_at, via_app)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, in.ItemID, f.workspaceID, author, in.Actor.UserID, in.Body, in.Actor.Kind, appSource,
		activityID, nilIfEmpty(in.ParentID), ts, ts, f.installID,
	); err != nil {
		return nil, fmt.Errorf("fenced comment insert: %w", err)
	}
	created, err := f.s.getCommentQ(f.tx, id)
	if err != nil {
		return nil, err
	}
	if err := f.s.emitCommentEventAsInstallTx(f.tx, kernelevents.CommentCreated, created, f.installID); err != nil {
		return nil, err
	}
	if err := f.s.enqueueDecisionJobsForItemTx(f.tx, in.ItemID); err != nil {
		return nil, err
	}
	return created, nil
}

// lockOwnComment reads the comment the caller addresses through itemID, row
// locked on Postgres as the human edit locks it, and refuses unless it is on
// that item and was written by this install as this user. The item binding is
// checked BEFORE authorship, so a comment on another item is "not found",
// never "not yours".
func (f *FencedTx) lockOwnComment(itemID, commentID string, actor FencedActor) (body string, deleted bool, err error) {
	q := `SELECT body, COALESCE(user_id, ''), COALESCE(via_app, ''), CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END
		FROM comments WHERE id = ? AND item_id = ? AND workspace_id = ?`
	if f.s.dialect.Driver() == DriverPostgres {
		q += ` FOR NO KEY UPDATE`
	}
	var userID, viaApp string
	var del int
	switch err := f.tx.QueryRow(f.s.q(q), commentID, itemID, f.workspaceID).Scan(&body, &userID, &viaApp, &del); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, ErrAppCommentNotFound
	case err != nil:
		return "", false, fmt.Errorf("fenced comment: read: %w", err)
	}
	if userID != actor.UserID || viaApp != f.installID {
		return "", false, ErrAppNotCommentAuthor
	}
	return body, del == 1, nil
}

// UpdateComment replaces the body of a comment this install wrote as this
// user. A tombstone is refused. The event and the decision job follow only a
// changed body, as on the human path.
func (f *FencedTx) UpdateComment(itemID, commentID, body string, actor FencedActor) (*models.Comment, error) {
	if !actor.valid() {
		return nil, ErrAppBadActor
	}
	if err := f.LockWorkspaceSeq(); err != nil {
		return nil, err
	}
	if _, err := f.requireCompanionItem(itemID); err != nil {
		return nil, err
	}
	before, deleted, err := f.lockOwnComment(itemID, commentID, actor)
	if err != nil {
		return nil, err
	}
	if deleted {
		return nil, ErrCommentDeleted
	}
	if err := f.checkItemAttachmentRefs(itemID, body); err != nil {
		return nil, err
	}
	if _, err := f.tx.Exec(f.s.q(`UPDATE comments SET body = ?, updated_at = ? WHERE id = ?`), body, now(), commentID); err != nil {
		return nil, fmt.Errorf("fenced comment update: %w", err)
	}
	updated, err := f.s.getCommentQ(f.tx, commentID)
	if err != nil {
		return nil, err
	}
	if body != before {
		if err := f.s.emitCommentEventAsInstallTx(f.tx, kernelevents.CommentUpdated, updated, f.installID); err != nil {
			return nil, err
		}
		if err := f.s.enqueueDecisionJobsForItemTx(f.tx, itemID); err != nil {
			return nil, err
		}
	}
	return updated, nil
}

// DeleteComment deletes a comment this install wrote as this user, exactly as
// the human DeleteComment does: the ancestor chain locked root first, a
// tombstone when replies remain, otherwise a delete and a reap of emptied
// tombstones above it, the app projection built before either. It refuses a
// chain that leaves the item, since the reap walks it.
func (f *FencedTx) DeleteComment(itemID, commentID string, actor FencedActor) error {
	if !actor.valid() {
		return ErrAppBadActor
	}
	if err := f.LockWorkspaceSeq(); err != nil {
		return err
	}
	if _, err := f.requireCompanionItem(itemID); err != nil {
		return err
	}
	// An unlocked binding check first, so a comment on another item is
	// refused before its chain is locked. It cannot be a row lock: the chain
	// lock below must take the root first (lockCommentForDeleteTx). The
	// binding is re-checked under that lock.
	var onItem int
	if err := f.tx.QueryRow(f.s.q(`SELECT COUNT(*) FROM comments WHERE id = ? AND item_id = ? AND workspace_id = ?`),
		commentID, itemID, f.workspaceID).Scan(&onItem); err != nil {
		return fmt.Errorf("fenced comment delete: read: %w", err)
	}
	if onItem == 0 {
		return ErrAppCommentNotFound
	}
	target, err := f.s.lockCommentForDeleteTx(f.tx, commentID)
	if err != nil {
		return err
	}
	if target == nil || target.itemID != itemID || target.workspaceID != f.workspaceID {
		return ErrAppCommentNotFound
	}
	if _, _, err := f.lockOwnComment(itemID, commentID, actor); err != nil {
		return err
	}
	if target.deleted {
		return ErrAppCommentNotFound
	}
	if err := f.requireThreadOnItem(itemID, target.parentID); err != nil {
		return err
	}

	var authorID sql.NullString
	var author, createdBy string
	if err := f.tx.QueryRow(f.s.q(`SELECT user_id, COALESCE(author, ''), created_by FROM comments WHERE id = ?`), commentID).Scan(&authorID, &author, &createdBy); err != nil {
		return fmt.Errorf("fenced comment delete: read author: %w", err)
	}
	proj, err := f.s.buildCommentAppProjectionTx(f.tx, commentID, itemID, authorID.String, author, createdBy, target.parentID)
	if err != nil {
		return err
	}
	if proj != nil {
		proj.ActorViaApp = f.installID // TASK-3411
	}

	var replies int
	if err := f.tx.QueryRow(f.s.q(`SELECT COUNT(*) FROM comments WHERE parent_id = ?`), commentID).Scan(&replies); err != nil {
		return fmt.Errorf("fenced comment delete: count replies: %w", err)
	}
	if replies > 0 {
		if _, err := f.tx.Exec(f.s.q(`DELETE FROM comment_reactions WHERE comment_id = ?`), commentID); err != nil {
			return fmt.Errorf("fenced comment delete: drop reactions: %w", err)
		}
		if _, err := f.tx.Exec(f.s.q(`UPDATE comments SET body = '', deleted_at = ? WHERE id = ?`), now(), commentID); err != nil {
			return fmt.Errorf("fenced comment delete: tombstone: %w", err)
		}
	} else {
		if _, err := f.tx.Exec(f.s.q(`DELETE FROM comments WHERE id = ?`), commentID); err != nil {
			return fmt.Errorf("fenced comment delete: %w", err)
		}
		if err := f.s.reapTombstonesTx(f.tx, target.parentID); err != nil {
			return err
		}
	}
	if err := f.s.emitRefOnlyDeletionWithProjectionTx(f.tx, kernelevents.CommentDeleted, f.workspaceID, commentID, itemID, target.parentID, proj); err != nil {
		return err
	}
	return f.s.enqueueDecisionJobsForItemTx(f.tx, itemID)
}

// requireThreadOnItem walks the ancestors from parentID up and refuses if any
// of them is on another item. lockCommentForDeleteTx has already locked the
// chain, so the walk reads what the reap will walk.
func (f *FencedTx) requireThreadOnItem(itemID, parentID string) error {
	seen := map[string]bool{}
	for cur := parentID; cur != "" && !seen[cur]; {
		seen[cur] = true
		var onItem, next string
		err := f.tx.QueryRow(f.s.q(`SELECT item_id, COALESCE(parent_id, '') FROM comments WHERE id = ?`), cur).Scan(&onItem, &next)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("fenced comment delete: read ancestor: %w", err)
		}
		if onItem != itemID {
			return ErrAppCrossItemThread
		}
		cur = next
	}
	return nil
}

// checkItemAttachmentRefs refuses a comment body unless every
// pad-attachment: reference in it names a live attachment bound to itemID.
// Unknown, foreign, unbound and deleted ids all answer
// ErrAppAttachmentUnavailable, so the refusal is not an existence oracle.
//
// It is a CHECK and writes nothing: there is no stamp. The human path stamps
// last_referenced_at (stampAttachmentRefsTx) so that the orphan GC's
// never-attached claim cannot reap a row a body has just started to
// reference. That claim (ClaimNeverAttachedAttachment) requires
// item_id IS NULL, and no store path ever sets a bound attachment's item_id
// back to NULL (the only writers go NULL -> item). Every row this check
// admits is bound to the item, so a stamp on it would protect nothing. A
// stamp would also be the only reason to lock attachment rows here, and
// those locks deadlock against the human stamp, which locks originals and
// variants in no fixed order (codex round 4; ruling revised on DOC-3371).
//
// Unlocked: an attachment deleted just after this read leaves the committed
// body with a dangling reference, the same outcome as a delete a moment after
// commit, and the residual the human stamp already accepts.
func (f *FencedTx) checkItemAttachmentRefs(itemID, body string) error {
	if !strings.Contains(strings.ToLower(body), attachmentRefPrefix) {
		return nil
	}
	seen := map[string]bool{}
	var ids []string
	for _, m := range appAttachmentRefRE.FindAllStringSubmatch(body, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			ids = append(ids, m[1])
		}
	}
	// Chunked like the human stamp, to stay under the bind-parameter limit.
	const chunkSize = 400
	for start := 0; start < len(ids); start += chunkSize {
		part := ids[start:min(start+chunkSize, len(ids))]
		args := []any{f.workspaceID, itemID}
		for _, id := range part {
			args = append(args, id)
		}
		var found int
		if err := f.tx.QueryRow(f.s.q(`SELECT COUNT(*) FROM attachments
			WHERE workspace_id = ? AND item_id = ? AND deleted_at IS NULL AND id IN (`+
			strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")+`)`), args...).Scan(&found); err != nil {
			return fmt.Errorf("fenced attachment check: %w", err)
		}
		if found != len(part) {
			return ErrAppAttachmentUnavailable
		}
	}
	return nil
}
