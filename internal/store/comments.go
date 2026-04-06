package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/xarmian/pad/internal/models"
)

// CreateComment adds a new comment to an item.
func (s *Store) CreateComment(workspaceID, itemID string, input models.CommentCreate) (*models.Comment, error) {
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

	_, err := s.db.Exec(s.q(`
		INSERT INTO comments (id, item_id, workspace_id, author, body, created_by, source, activity_id, parent_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, itemID, workspaceID, author, input.Body, createdBy, source,
		nilIfEmpty(input.ActivityID), nilIfEmpty(input.ParentID), ts, ts,
	)
	if err != nil {
		return nil, fmt.Errorf("insert comment: %w", err)
	}

	return s.GetComment(id)
}

// GetComment returns a single comment by ID.
func (s *Store) GetComment(id string) (*models.Comment, error) {
	row := s.db.QueryRow(s.q(`
		SELECT c.id, c.item_id, c.workspace_id, c.author, c.body,
		       c.created_by, c.source, COALESCE(c.activity_id, ''), COALESCE(c.parent_id, ''),
		       c.created_at, c.updated_at,
		       i.title, i.slug
		FROM comments c
		JOIN items i ON i.id = c.item_id
		WHERE c.id = ?`), id)

	var c models.Comment
	var createdAt, updatedAt string
	err := row.Scan(
		&c.ID, &c.ItemID, &c.WorkspaceID, &c.Author, &c.Body,
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

// ListComments returns all comments for an item, ordered chronologically.
func (s *Store) ListComments(itemID string) ([]models.Comment, error) {
	rows, err := s.db.Query(s.q(`
		SELECT c.id, c.item_id, c.workspace_id, c.author, c.body,
		       c.created_by, c.source, COALESCE(c.activity_id, ''), COALESCE(c.parent_id, ''),
		       c.created_at, c.updated_at
		FROM comments c
		WHERE c.item_id = ?
		ORDER BY c.created_at ASC`), itemID)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	defer rows.Close()

	var comments []models.Comment
	for rows.Next() {
		var c models.Comment
		var createdAt, updatedAt string
		if err := rows.Scan(
			&c.ID, &c.ItemID, &c.WorkspaceID, &c.Author, &c.Body,
			&c.CreatedBy, &c.Source, &c.ActivityID, &c.ParentID,
			&createdAt, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		c.CreatedAt = parseTime(createdAt)
		c.UpdatedAt = parseTime(updatedAt)
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

// ListCommentsBeforeTime returns comments for an item created before the given time,
// ordered newest-first, limited to `limit` results. Used for cursor-based timeline pagination.
func (s *Store) ListCommentsBeforeTime(itemID string, before time.Time, beforeID string, limit int) ([]models.Comment, error) {
	ts := before.Format(time.RFC3339)
	rows, err := s.db.Query(s.q(`
		SELECT c.id, c.item_id, c.workspace_id, c.author, c.body,
		       c.created_by, c.source, COALESCE(c.activity_id, ''), COALESCE(c.parent_id, ''),
		       c.created_at, c.updated_at
		FROM comments c
		WHERE c.item_id = ? AND (c.created_at < ? OR (c.created_at = ? AND c.id < ?))
		ORDER BY c.created_at DESC, c.id DESC
		LIMIT ?`), itemID, ts, ts, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("list comments before time: %w", err)
	}
	defer rows.Close()

	var comments []models.Comment
	for rows.Next() {
		var c models.Comment
		var createdAt, updatedAt string
		if err := rows.Scan(
			&c.ID, &c.ItemID, &c.WorkspaceID, &c.Author, &c.Body,
			&c.CreatedBy, &c.Source, &c.ActivityID, &c.ParentID,
			&createdAt, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		c.CreatedAt = parseTime(createdAt)
		c.UpdatedAt = parseTime(updatedAt)
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

// DeleteComment removes a comment by ID.
func (s *Store) DeleteComment(id string) error {
	result, err := s.db.Exec(s.q("DELETE FROM comments WHERE id = ?"), id)
	if err != nil {
		return fmt.Errorf("delete comment: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CountComments returns the number of comments for an item.
func (s *Store) CountComments(itemID string) (int, error) {
	var count int
	err := s.db.QueryRow(s.q("SELECT COUNT(*) FROM comments WHERE item_id = ?"), itemID).Scan(&count)
	return count, err
}
