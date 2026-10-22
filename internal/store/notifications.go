package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The push queue's enqueue side (TASK-3937). Every function here runs on the
// CALLER's transaction, so a notification commits with the write that caused
// it or not at all. The settings a recipient has chosen are the dispatcher's
// to apply, not the enqueue's.
//
// A row is written ONLY for a user with at least one push_devices row (the
// EXISTS in enqueueNotificationsTx), so until device registration lands
// (TASK-3938) the enqueue writes nothing. dedupe_key's ON CONFLICT makes it
// idempotent per (user, key). Migration 155's header predates this file and
// calls the enqueue owed: merged migrations are immutable, so this comment,
// not that one, is the current description.
//
// Recipients are NOT access-checked here. Removing a member leaves them an
// item's created_by and possibly its assignee, so a row can be written for
// someone who can no longer see the item. Once the dispatcher is wired
// into the server (TASK-3939 part B2c; B1 and B2a only build it), it re-checks
// current membership and item visibility at send and skips the row: that is
// the privacy gate.
//
// @mention notifications (TASK-3946) are built from the users
// reindexCommentMentionsTx newly indexed, at the comment create and edit
// sites only: never on a tombstone, an import, a cross-workspace copy (which
// copies no comments) or the backfill.

const (
	notifyAssigned = "assigned"
	notifyComment  = "comment"
	notifyReply    = "reply"
	notifyReminder = "reminder"
	notifyMention  = "mention"
)

// notifyFact is one notification owed to one user.
type notifyFact struct {
	UserID       string
	WorkspaceID  string
	ItemID       string
	CommentID    string
	Kind         string
	ActorUserID  string
	ActorIsAgent bool
	// DedupeKey is unique per user: enqueuing the same fact twice writes one
	// row. Built from a kind and server-minted ids, never from request text.
	DedupeKey string
}

// enqueueNotificationsTx writes each fact for its user, and only for a user
// who has at least one registered device: a person who never installed the
// app accumulates nothing. The EXISTS gate is in the statement, not a prior
// read, so a device registered in the same instant cannot be missed or
// double-counted. The SELECT form carries a WHERE because SQLite misparses
// INSERT ... SELECT ... ON CONFLICT without one.
func (s *Store) enqueueNotificationsTx(tx *sql.Tx, facts []notifyFact) error {
	_, err := s.enqueueNotificationsCountTx(tx, facts)
	return err
}

// enqueueNotificationsCountTx is enqueueNotificationsTx reporting which facts
// wrote a row: a fact gated out by the device EXISTS, or already present
// under its dedupe key, reports false.
func (s *Store) enqueueNotificationsCountTx(tx *sql.Tx, facts []notifyFact) ([]bool, error) {
	agentParam := "CAST(? AS INTEGER)"
	if s.dialect.Driver() == DriverPostgres {
		agentParam = "CAST(? AS BOOLEAN)"
	}
	q := s.q(`
		INSERT INTO user_notifications
			(id, user_id, workspace_id, item_id, comment_id, kind, actor_user_id, actor_is_agent, dedupe_key, created_at)
		SELECT ?, ?, ?, ?, NULLIF(?, ''), ?, NULLIF(?, ''), ` + agentParam + `, ?, ?
		WHERE EXISTS (SELECT 1 FROM push_devices WHERE user_id = ?)
		ON CONFLICT (user_id, dedupe_key) DO NOTHING`)
	ts := now()
	wrote := make([]bool, len(facts))
	for i, f := range facts {
		if f.UserID == "" {
			continue
		}
		res, err := tx.Exec(q,
			newID(), f.UserID, f.WorkspaceID, f.ItemID, f.CommentID, f.Kind, f.ActorUserID,
			s.dialect.BoolToInt(f.ActorIsAgent), f.DedupeKey, ts, f.UserID)
		if err != nil {
			return nil, fmt.Errorf("enqueue %s notification: %w", f.Kind, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("enqueue %s notification: %w", f.Kind, err)
		}
		wrote[i] = n == 1
	}
	return wrote, nil
}

// enqueueAssignedNotificationTx tells toUser they were given item. Assigning
// yourself notifies nobody; an empty actor (a system write) is not "you".
func (s *Store) enqueueAssignedNotificationTx(tx *sql.Tx, item *models.Item, toUser, actorUserID string, actorIsAgent bool) error {
	if toUser == "" || toUser == actorUserID {
		return nil
	}
	return s.enqueueNotificationsTx(tx, []notifyFact{{
		UserID: toUser, WorkspaceID: item.WorkspaceID, ItemID: item.ID, Kind: notifyAssigned,
		ActorUserID: actorUserID, ActorIsAgent: actorIsAgent,
		DedupeKey: fmt.Sprintf("assigned:%s:%d", item.ID, item.Seq),
	}})
}

// commentRecipients collects who a comment concerns, first kind wins. It is a
// type rather than a closure so the appstore boundary walk can resolve it.
type commentRecipients struct {
	order []string
	kinds map[string]string
}

// add records user under kind unless they are the actor, empty, or already in.
func (r *commentRecipients) add(user, kind, actorUserID string) {
	if user == "" || user == actorUserID {
		return
	}
	if r.kinds == nil {
		r.kinds = map[string]string{}
	}
	if _, seen := r.kinds[user]; !seen {
		r.order = append(r.order, user)
		r.kinds[user] = kind
	}
}

// enqueueCommentNotificationsTx tells the people a new comment concerns: the
// item's assignee and creator ("comment"), and the author of the comment it
// replies to ("reply"). The author is never told about their own comment. A
// person who is both a reply target and an assignee gets the one "reply",
// unless they are also mentioned (below).
// A tombstoned parent still names its author; the dispatcher filters later.
//
// mentioned is the users the comment's body newly indexed (TASK-3946). Those
// who can see the item are added first, so a mentioned person who is also a
// reply target, assignee or creator gets the one "mention" and no second row.
// A mention of someone who cannot see the item writes nothing, and they keep
// whatever other role they have.
func (s *Store) enqueueCommentNotificationsTx(tx *sql.Tx, c *models.Comment, mentioned []string, actorUserID string, actorIsAgent bool) error {
	var assigned, creator sql.NullString
	if err := tx.QueryRow(s.q(`SELECT assigned_user_id, created_by_user_id FROM items WHERE id = ?`), c.ItemID).
		Scan(&assigned, &creator); err != nil {
		return fmt.Errorf("read comment notification recipients: %w", err)
	}
	visible, err := s.mentionRecipientsTx(tx, c, mentioned)
	if err != nil {
		return err
	}
	var to commentRecipients
	for _, u := range visible {
		to.add(u, notifyMention, actorUserID)
	}
	// The parent's author is added before the item's people, so a person who
	// is also the assignee keeps the "reply" and gets no second row.
	if c.ParentID != "" {
		var parentAuthor sql.NullString
		err := tx.QueryRow(s.q(`SELECT user_id FROM comments WHERE id = ?`), c.ParentID).Scan(&parentAuthor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read parent comment author: %w", err)
		}
		to.add(parentAuthor.String, notifyReply, actorUserID)
	}
	to.add(assigned.String, notifyComment, actorUserID)
	to.add(creator.String, notifyComment, actorUserID)
	return s.enqueueNotificationsTx(tx, to.facts(c, actorUserID, actorIsAgent))
}

// facts turns the collected recipients into one fact each for comment c.
func (r *commentRecipients) facts(c *models.Comment, actorUserID string, actorIsAgent bool) []notifyFact {
	out := make([]notifyFact, 0, len(r.order))
	for _, u := range r.order {
		kind := r.kinds[u]
		key := "comment:" + c.ID
		if kind == notifyMention {
			key = mentionDedupeKey(c.ID, u)
		}
		out = append(out, notifyFact{
			UserID: u, WorkspaceID: c.WorkspaceID, ItemID: c.ItemID, CommentID: c.ID, Kind: kind,
			ActorUserID: actorUserID, ActorIsAgent: actorIsAgent,
			DedupeKey: key,
		})
	}
	return out
}

func mentionDedupeKey(commentID, userID string) string {
	return "mention:" + commentID + ":" + userID
}

// enqueueMentionNotificationsTx tells the users an EDIT newly mentioned. The
// actor is the EDITOR, who wrote the mention: an admin adding @bob to Alice's
// comment is the admin mentioning bob, and an admin adding @alice notifies
// Alice (TASK-3946 ruling e6a86278). The editor is never told about
// mentioning themselves.
//
// Mention wins (TASK-3946 lead ruling f0fc5933): when the mention row is
// actually written, the same user's still-pending comment/reply row for this
// comment is deleted, by a conditional DELETE and never a read first, so a
// dispatcher claim that lands in between leaves both rows. A row already
// sent, skipped or expired stays, and the mention is written anyway. A
// mention that writes nothing (no device, an existing mention row under the
// dedupe key, or a user who cannot see the item) deletes nothing.
func (s *Store) enqueueMentionNotificationsTx(tx *sql.Tx, c *models.Comment, added []string, actorUserID string, actorIsAgent bool) error {
	visible, err := s.mentionRecipientsTx(tx, c, added)
	if err != nil || len(visible) == 0 {
		return err
	}
	var to commentRecipients
	for _, u := range visible {
		to.add(u, notifyMention, actorUserID)
	}
	facts := to.facts(c, actorUserID, actorIsAgent)
	wrote, err := s.enqueueNotificationsCountTx(tx, facts)
	if err != nil {
		return err
	}
	for i, f := range facts {
		if !wrote[i] {
			continue
		}
		if _, err := tx.Exec(s.q(`
			DELETE FROM user_notifications
			WHERE comment_id = ? AND user_id = ? AND kind IN ('comment', 'reply') AND push_state = 'pending'`),
			c.ID, f.UserID); err != nil {
			return fmt.Errorf("supersede comment notification: %w", err)
		}
	}
	return nil
}

// mentionRecipientsTx keeps the mentioned users who can see c's item
// (TASK-3946, binding rule 645fab20): comment_mentions indexes any resolved
// human, so a non-member, a no-grant guest, or a restricted member outside
// the item's collection must not be told. Visibility is resolved as for a
// bearer caller, so a platform admin is judged by membership and grants,
// never by the admin role.
func (s *Store) mentionRecipientsTx(tx *sql.Tx, c *models.Comment, users []string) ([]string, error) {
	if len(users) == 0 {
		return nil, nil
	}
	var collectionID string
	if err := tx.QueryRow(s.q(`SELECT collection_id FROM items WHERE id = ?`), c.ItemID).Scan(&collectionID); err != nil {
		return nil, fmt.Errorf("read mention item collection: %w", err)
	}
	var out []string
	for _, u := range users {
		// nil/nil below means unrestricted, but the helper also returns it
		// for a user that no longer exists, so existence is checked first.
		if user, err := s.GetUserQ(tx, u); err != nil {
			return nil, fmt.Errorf("read mentioned user: %w", err)
		} else if user == nil {
			continue
		}
		full, granted, err := s.ResolveBacklinksVisibilityQ(tx, u, c.WorkspaceID, false, true)
		if err != nil {
			return nil, fmt.Errorf("resolve mention visibility: %w", err)
		}
		if (full == nil && granted == nil) || slices.Contains(full, collectionID) || slices.Contains(granted, c.ItemID) {
			out = append(out, u)
		}
	}
	return out, nil
}

// enqueueReminderNotificationTx tells the item's assignee, or its creator when
// nobody holds it, that a reminder fired. A reminder has no owner column, and
// the key carries fired_at so a re-armed reminder that fires again is a new
// notification. There is no actor: the clock fired it.
func (s *Store) enqueueReminderNotificationTx(tx *sql.Tx, r *models.Reminder, item *models.Item) error {
	to := ""
	if item.AssignedUserID != nil {
		to = *item.AssignedUserID
	}
	if to == "" {
		var creator sql.NullString
		if err := tx.QueryRow(s.q(`SELECT created_by_user_id FROM items WHERE id = ?`), item.ID).Scan(&creator); err != nil {
			return fmt.Errorf("read reminder notification recipient: %w", err)
		}
		to = creator.String
	}
	fired := ""
	if r.FiredAt != nil {
		fired = *r.FiredAt
	}
	return s.enqueueNotificationsTx(tx, []notifyFact{{
		UserID: to, WorkspaceID: r.WorkspaceID, ItemID: r.ItemID, Kind: notifyReminder,
		DedupeKey: "reminder:" + r.ID + ":" + fired,
	}})
}
