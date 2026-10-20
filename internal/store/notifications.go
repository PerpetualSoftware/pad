package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The push queue's enqueue side (TASK-3937). Every function here runs on the
// CALLER's transaction, so a notification commits with the write that caused
// it or not at all. The settings a recipient has chosen are the dispatcher's
// to apply, not the enqueue's.

const (
	notifyAssigned = "assigned"
	notifyComment  = "comment"
	notifyReply    = "reply"
	notifyReminder = "reminder"
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
	for _, f := range facts {
		if f.UserID == "" {
			continue
		}
		if _, err := tx.Exec(q,
			newID(), f.UserID, f.WorkspaceID, f.ItemID, f.CommentID, f.Kind, f.ActorUserID,
			s.dialect.BoolToInt(f.ActorIsAgent), f.DedupeKey, ts, f.UserID); err != nil {
			return fmt.Errorf("enqueue %s notification: %w", f.Kind, err)
		}
	}
	return nil
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
// person who is both a reply target and an assignee gets the one "reply".
// A tombstoned parent still names its author; the dispatcher filters later.
func (s *Store) enqueueCommentNotificationsTx(tx *sql.Tx, c *models.Comment, actorUserID string, actorIsAgent bool) error {
	var assigned, creator sql.NullString
	if err := tx.QueryRow(s.q(`SELECT assigned_user_id, created_by_user_id FROM items WHERE id = ?`), c.ItemID).
		Scan(&assigned, &creator); err != nil {
		return fmt.Errorf("read comment notification recipients: %w", err)
	}
	// The parent's author is added first, so a person who is also the assignee
	// keeps the "reply" and gets no second row.
	var to commentRecipients
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
	facts := make([]notifyFact, 0, len(to.order))
	for _, u := range to.order {
		facts = append(facts, notifyFact{
			UserID: u, WorkspaceID: c.WorkspaceID, ItemID: c.ItemID, CommentID: c.ID, Kind: to.kinds[u],
			ActorUserID: actorUserID, ActorIsAgent: actorIsAgent,
			DedupeKey: "comment:" + c.ID,
		})
	}
	return s.enqueueNotificationsTx(tx, facts)
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
