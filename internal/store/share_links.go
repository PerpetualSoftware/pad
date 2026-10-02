package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"golang.org/x/crypto/bcrypt"
)

// generateShareToken creates a cryptographically random URL-safe token
// with at least 128 bits of entropy and returns both the raw token
// and its SHA-256 hash for storage.
func generateShareToken() (raw string, hash string, err error) {
	b := make([]byte, 24) // 192 bits → 32 URL-safe base64 chars
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate share token: %w", err)
	}
	raw = base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
	h := sha256.Sum256([]byte(raw))
	hash = hex.EncodeToString(h[:])
	return raw, hash, nil
}

// hashShareToken computes the SHA-256 hash of a raw share token for lookup.
func hashShareToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// ShareLinkOptions holds optional constraint fields for creating a share link.
type ShareLinkOptions struct {
	Password        string  // Raw password (will be bcrypt-hashed if non-empty)
	ExpiresAt       *string // ISO 8601 timestamp
	MaxViews        *int
	RequireAuth     bool
	RestrictToEmail string
}

// CreateShareLink creates a new share link and returns it with the raw token.
// The raw token is only available in this response — it is not stored.
func (s *Store) CreateShareLink(workspaceID, targetType, targetID, permission, createdBy string, opts *ShareLinkOptions) (*models.ShareLink, error) {
	rawToken, tokenHash, err := generateShareToken()
	if err != nil {
		return nil, err
	}

	id := newID()
	ts := now()

	var passwordHash *string
	var expiresAt *string
	var maxViews *int
	requireAuth := false
	var restrictToEmail *string

	if opts != nil {
		if opts.Password != "" {
			hash, err := bcrypt.GenerateFromPassword([]byte(opts.Password), 10)
			if err != nil {
				return nil, fmt.Errorf("hash share link password: %w", err)
			}
			h := string(hash)
			passwordHash = &h
		}
		expiresAt = opts.ExpiresAt
		maxViews = opts.MaxViews
		requireAuth = opts.RequireAuth
		if opts.RestrictToEmail != "" {
			restrictToEmail = &opts.RestrictToEmail
		}
	}

	// A share link is a credential a disabled account cannot mint (BUG-3349,
	// requireActiveUserTx). Suspension on resolve covers links made before.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("create share link: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if createdBy != "" {
		if err := s.requireActiveUserTx(tx, createdBy); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(s.q(`
		INSERT INTO share_links (id, token_hash, target_type, target_id, workspace_id, permission, created_by,
		                         password_hash, expires_at, max_views, require_auth, restrict_to_email, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), id, tokenHash, targetType, targetID, workspaceID, permission, createdBy,
		passwordHash, expiresAt, maxViews, s.dialect.BoolToInt(requireAuth), restrictToEmail, ts); err != nil {
		return nil, fmt.Errorf("create share link: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create share link: commit: %w", err)
	}

	link, err := s.GetShareLink(id)
	if err != nil {
		return nil, err
	}
	if link == nil {
		// Deleted between the insert and this read, e.g. by its creator's
		// account deletion, which deletes the links it created (BUG-3289).
		// GetShareLink answers nil, nil for a missing row.
		return nil, fmt.Errorf("create share link: deleted concurrently: %w", sql.ErrNoRows)
	}
	link.Token = rawToken // Only set on creation
	return link, nil
}

// GetShareLink retrieves a share link by ID.
func (s *Store) GetShareLink(id string) (*models.ShareLink, error) {
	var link models.ShareLink
	var createdAt string
	var expiresAt, lastViewedAt, passwordHash, restrictToEmail *string
	var maxViews *int
	var requireAuth bool

	err := s.db.QueryRow(s.q(`
		SELECT id, token_hash, target_type, target_id, workspace_id, permission, created_by,
		       password_hash, expires_at, max_views, require_auth, restrict_to_email,
		       view_count, unique_viewers, last_viewed_at, created_at
		FROM share_links WHERE id = ?
	`), id).Scan(
		&link.ID, &link.TokenHash, &link.TargetType, &link.TargetID, &link.WorkspaceID,
		&link.Permission, &link.CreatedBy,
		&passwordHash, &expiresAt, &maxViews, &requireAuth, &restrictToEmail,
		&link.ViewCount, &link.UniqueViewers, &lastViewedAt, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get share link: %w", err)
	}

	link.RequireAuth = requireAuth
	link.PasswordHash = passwordHash
	link.HasPassword = passwordHash != nil && *passwordHash != ""
	link.CreatedAt = parseTime(createdAt)
	link.ExpiresAt = parseTimePtr(expiresAt)
	link.LastViewedAt = parseTimePtr(lastViewedAt)
	if maxViews != nil {
		link.MaxViews = maxViews
	}
	if restrictToEmail != nil {
		link.RestrictToEmail = *restrictToEmail
	}
	return &link, nil
}

// GetShareLinkByToken looks up a share link by its raw token (hashes it
// first). A link whose workspace is soft-deleted is not found (BUG-3339):
// deleting a workspace only marks it, and its links must stop serving at once
// rather than for the whole restore window. Restoring the workspace brings
// the link back. Every public share route resolves the token here.
//
// A link whose creator is disabled is suspended the same way (BUG-3349): not
// found while the account is disabled, serving again if it is re-enabled. It
// is a content artifact others rely on, and a disable can be a mistake, so it
// is not deleted.
func (s *Store) GetShareLinkByToken(token string) (*models.ShareLink, error) {
	hash := hashShareToken(token)
	var id string
	err := s.db.QueryRow(s.q(`
		SELECT sl.id FROM share_links sl
		JOIN workspaces w ON w.id = sl.workspace_id
		WHERE sl.token_hash = ? AND w.deleted_at IS NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM users u WHERE u.id = sl.created_by AND u.disabled_at IS NOT NULL
		  )
	`), hash).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get share link by token: %w", err)
	}
	return s.GetShareLink(id)
}

// ListShareLinks returns all share links for a target (item or collection).
func (s *Store) ListShareLinks(targetType, targetID string) ([]models.ShareLink, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id, token_hash, target_type, target_id, workspace_id, permission, created_by,
		       password_hash, expires_at, max_views, require_auth, restrict_to_email,
		       view_count, unique_viewers, last_viewed_at, created_at
		FROM share_links
		WHERE target_type = ? AND target_id = ?
		ORDER BY created_at DESC
	`), targetType, targetID)
	if err != nil {
		return nil, fmt.Errorf("list share links: %w", err)
	}
	defer rows.Close()

	var result []models.ShareLink
	for rows.Next() {
		var link models.ShareLink
		var createdAt string
		var expiresAt, lastViewedAt, passwordHash, restrictToEmail *string
		var maxViews *int
		var requireAuth bool

		if err := rows.Scan(
			&link.ID, &link.TokenHash, &link.TargetType, &link.TargetID, &link.WorkspaceID,
			&link.Permission, &link.CreatedBy,
			&passwordHash, &expiresAt, &maxViews, &requireAuth, &restrictToEmail,
			&link.ViewCount, &link.UniqueViewers, &lastViewedAt, &createdAt,
		); err != nil {
			return nil, err
		}

		link.RequireAuth = requireAuth
		link.HasPassword = passwordHash != nil && *passwordHash != ""
		link.CreatedAt = parseTime(createdAt)
		link.ExpiresAt = parseTimePtr(expiresAt)
		link.LastViewedAt = parseTimePtr(lastViewedAt)
		if maxViews != nil {
			link.MaxViews = maxViews
		}
		if restrictToEmail != nil {
			link.RestrictToEmail = *restrictToEmail
		}
		result = append(result, link)
	}
	return result, rows.Err()
}

// DeleteShareLink deletes a share link by ID, scoped to a workspace.
func (s *Store) DeleteShareLink(id, workspaceID string) error {
	result, err := s.db.Exec(s.q("DELETE FROM share_links WHERE id = ? AND workspace_id = ?"), id, workspaceID)
	if err != nil {
		return fmt.Errorf("delete share link: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RecordShareLinkView atomically increments the view counter (respecting max_views)
// and records a view entry inside a single transaction. Returns true if the view
// was allowed, false if the max_views limit has been reached. If any step after
// the increment fails, the entire transaction is rolled back so a view is never
// consumed without being fully recorded.
func (s *Store) RecordShareLinkView(linkID, fingerprint, userID string, maxViews *int) (bool, error) {
	ts := now()

	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin share view tx: %w", err)
	}
	defer tx.Rollback() // no-op after Commit

	// Atomically increment view_count, respecting max_views if set.
	// The WHERE condition ensures we never exceed the limit.
	var result sql.Result
	if maxViews != nil {
		result, err = tx.Exec(s.q(`
			UPDATE share_links SET view_count = view_count + 1, last_viewed_at = ?
			WHERE id = ? AND view_count < ?
		`), ts, linkID, *maxViews)
	} else {
		result, err = tx.Exec(s.q(`
			UPDATE share_links SET view_count = view_count + 1, last_viewed_at = ?
			WHERE id = ?
		`), ts, linkID)
	}
	if err != nil {
		return false, fmt.Errorf("increment view count: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		// max_views limit reached — no row was updated
		return false, nil
	}

	// Check unique viewers (by fingerprint or user ID)
	var existingCount int
	if userID != "" {
		if err := tx.QueryRow(s.q(
			"SELECT COUNT(*) FROM share_link_views WHERE share_link_id = ? AND viewer_user_id = ?"),
			linkID, userID).Scan(&existingCount); err != nil {
			return false, fmt.Errorf("check unique viewer: %w", err)
		}
	} else if fingerprint != "" {
		if err := tx.QueryRow(s.q(
			"SELECT COUNT(*) FROM share_link_views WHERE share_link_id = ? AND viewer_fingerprint = ?"),
			linkID, fingerprint).Scan(&existingCount); err != nil {
			return false, fmt.Errorf("check unique viewer: %w", err)
		}
	}

	if existingCount == 0 {
		if _, err := tx.Exec(s.q(`
			UPDATE share_links SET unique_viewers = unique_viewers + 1 WHERE id = ?
		`), linkID); err != nil {
			return false, fmt.Errorf("increment unique viewers: %w", err)
		}
	}

	// Record the view
	viewID := newID()
	viewerUserID := sql.NullString{String: userID, Valid: userID != ""}
	if _, err := tx.Exec(s.q(`
		INSERT INTO share_link_views (id, share_link_id, viewer_fingerprint, viewer_user_id, viewed_at)
		VALUES (?, ?, ?, ?, ?)
	`), viewID, linkID, fingerprint, viewerUserID, ts); err != nil {
		return false, fmt.Errorf("record share link view: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit share view tx: %w", err)
	}
	return true, nil
}

// ValidateShareLinkPassword checks a password against the share link's stored hash.
func (s *Store) ValidateShareLinkPassword(link *models.ShareLink, password string) bool {
	if link.PasswordHash == nil || *link.PasswordHash == "" {
		return true // No password required
	}
	return bcrypt.CompareHashAndPassword([]byte(*link.PasswordHash), []byte(password)) == nil
}

// ListShareLinkViews returns view history for a share link.
func (s *Store) ListShareLinkViews(linkID string, limit int) ([]models.ShareLinkView, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(s.q(`
		SELECT id, share_link_id, COALESCE(viewer_fingerprint, ''), COALESCE(viewer_user_id, ''), viewed_at
		FROM share_link_views
		WHERE share_link_id = ?
		ORDER BY viewed_at DESC
		LIMIT ?
	`), linkID, limit)
	if err != nil {
		return nil, fmt.Errorf("list share link views: %w", err)
	}
	defer rows.Close()

	var result []models.ShareLinkView
	for rows.Next() {
		var v models.ShareLinkView
		var viewedAt string
		if err := rows.Scan(&v.ID, &v.ShareLinkID, &v.ViewerFingerprint, &v.ViewerUserID, &viewedAt); err != nil {
			return nil, err
		}
		v.ViewedAt = parseTime(viewedAt)
		result = append(result, v)
	}
	return result, rows.Err()
}

// ValidateShareLink checks if a share link is valid (not expired).
// Max views enforcement is handled atomically by RecordShareLinkView.
// Returns nil error if valid, or an error describing why it's invalid.
func (s *Store) ValidateShareLink(link *models.ShareLink) error {
	if link == nil {
		return fmt.Errorf("share link not found")
	}

	// Check expiry
	if link.ExpiresAt != nil && time.Now().After(*link.ExpiresAt) {
		return fmt.Errorf("share link has expired")
	}

	return nil
}
