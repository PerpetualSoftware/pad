package store

import (
	"fmt"

	"github.com/xarmian/pad/internal/models"
)

// AddReaction adds an emoji reaction to a comment. Returns the created reaction,
// or the existing one if the same user+emoji already exists.
func (s *Store) AddReaction(commentID, userID, actor, emoji string) (*models.Reaction, error) {
	id := newID()
	ts := now()

	// Store empty string (not NULL) for anonymous users so the UNIQUE constraint
	// on (comment_id, user_id, emoji) works correctly — SQLite treats NULL != NULL.
	_, err := s.db.Exec(s.q(`
		INSERT INTO comment_reactions (id, comment_id, user_id, actor, emoji, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(comment_id, user_id, emoji) DO NOTHING`),
		id, commentID, userID, actor, emoji, ts,
	)
	if err != nil {
		return nil, fmt.Errorf("add reaction: %w", err)
	}

	// Return the reaction (may be existing if ON CONFLICT hit).
	return s.getReaction(commentID, userID, emoji)
}

func (s *Store) getReaction(commentID, userID, emoji string) (*models.Reaction, error) {
	var r models.Reaction
	var createdAt string
	err := s.db.QueryRow(s.q(`
		SELECT id, comment_id, COALESCE(user_id, ''), actor, emoji, created_at
		FROM comment_reactions
		WHERE comment_id = ? AND user_id = ? AND emoji = ?`),
		commentID, userID, emoji,
	).Scan(&r.ID, &r.CommentID, &r.UserID, &r.Actor, &r.Emoji, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("get reaction: %w", err)
	}
	r.CreatedAt = parseTime(createdAt)
	return &r, nil
}

// RemoveReaction removes a specific emoji reaction by a user from a comment.
func (s *Store) RemoveReaction(commentID, userID, emoji string) error {
	result, err := s.db.Exec(s.q(`
		DELETE FROM comment_reactions
		WHERE comment_id = ? AND user_id = ? AND emoji = ?`),
		commentID, userID, emoji,
	)
	if err != nil {
		return fmt.Errorf("remove reaction: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("reaction not found")
	}
	return nil
}

// ListReactionsByComments loads all reactions for a set of comment IDs, keyed by comment ID.
func (s *Store) ListReactionsByComments(commentIDs []string) (map[string][]models.Reaction, error) {
	if len(commentIDs) == 0 {
		return nil, nil
	}

	// Build query with placeholders.
	query := `
		SELECT cr.id, cr.comment_id, COALESCE(cr.user_id, ''), cr.actor, cr.emoji, cr.created_at,
		       COALESCE(u.name, '') as actor_name
		FROM comment_reactions cr
		LEFT JOIN users u ON u.id = cr.user_id
		WHERE cr.comment_id IN (`
	args := make([]interface{}, len(commentIDs))
	for i, id := range commentIDs {
		if i > 0 {
			query += ", "
		}
		query += "?"
		args[i] = id
	}
	query += `) ORDER BY cr.created_at ASC`

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list reactions: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]models.Reaction)
	for rows.Next() {
		var r models.Reaction
		var createdAt string
		if err := rows.Scan(&r.ID, &r.CommentID, &r.UserID, &r.Actor, &r.Emoji, &createdAt, &r.ActorName); err != nil {
			return nil, fmt.Errorf("scan reaction: %w", err)
		}
		r.CreatedAt = parseTime(createdAt)
		result[r.CommentID] = append(result[r.CommentID], r)
	}
	return result, rows.Err()
}
