package store

import (
	"database/sql"
	"fmt"

	"github.com/xarmian/pad/internal/models"
)

// --- Collection Grants ---

// CreateCollectionGrant creates a direct grant on a collection for a user.
func (s *Store) CreateCollectionGrant(workspaceID, collectionID, userID, permission, grantedBy string) (*models.CollectionGrant, error) {
	id := newID()
	ts := now()

	_, err := s.db.Exec(s.q(`
		INSERT INTO collection_grants (id, collection_id, workspace_id, user_id, permission, granted_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`), id, collectionID, workspaceID, userID, permission, grantedBy, ts)
	if err != nil {
		return nil, fmt.Errorf("create collection grant: %w", err)
	}

	return s.GetCollectionGrant(id)
}

// GetCollectionGrant retrieves a collection grant by ID.
func (s *Store) GetCollectionGrant(id string) (*models.CollectionGrant, error) {
	var g models.CollectionGrant
	var createdAt string

	err := s.db.QueryRow(s.q(`
		SELECT cg.id, cg.collection_id, cg.workspace_id, cg.user_id, cg.permission, cg.granted_by, cg.created_at,
		       COALESCE(u.name, ''), COALESCE(u.email, ''), COALESCE(u.username, '')
		FROM collection_grants cg
		LEFT JOIN users u ON u.id = cg.user_id
		WHERE cg.id = ?
	`), id).Scan(
		&g.ID, &g.CollectionID, &g.WorkspaceID, &g.UserID, &g.Permission, &g.GrantedBy, &createdAt,
		&g.UserName, &g.UserEmail, &g.UserUsername,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get collection grant: %w", err)
	}

	g.CreatedAt = parseTime(createdAt)
	return &g, nil
}

// ListCollectionGrants returns all grants on a collection.
func (s *Store) ListCollectionGrants(collectionID string) ([]models.CollectionGrant, error) {
	rows, err := s.db.Query(s.q(`
		SELECT cg.id, cg.collection_id, cg.workspace_id, cg.user_id, cg.permission, cg.granted_by, cg.created_at,
		       COALESCE(u.name, ''), COALESCE(u.email, ''), COALESCE(u.username, '')
		FROM collection_grants cg
		LEFT JOIN users u ON u.id = cg.user_id
		WHERE cg.collection_id = ?
		ORDER BY cg.created_at ASC
	`), collectionID)
	if err != nil {
		return nil, fmt.Errorf("list collection grants: %w", err)
	}
	defer rows.Close()

	var result []models.CollectionGrant
	for rows.Next() {
		var g models.CollectionGrant
		var createdAt string
		if err := rows.Scan(
			&g.ID, &g.CollectionID, &g.WorkspaceID, &g.UserID, &g.Permission, &g.GrantedBy, &createdAt,
			&g.UserName, &g.UserEmail, &g.UserUsername,
		); err != nil {
			return nil, err
		}
		g.CreatedAt = parseTime(createdAt)
		result = append(result, g)
	}
	return result, rows.Err()
}

// DeleteCollectionGrant revokes a collection grant by ID.
func (s *Store) DeleteCollectionGrant(id string) error {
	result, err := s.db.Exec(s.q("DELETE FROM collection_grants WHERE id = ?"), id)
	if err != nil {
		return fmt.Errorf("delete collection grant: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// --- Item Grants ---

// CreateItemGrant creates a direct grant on an item for a user.
func (s *Store) CreateItemGrant(workspaceID, itemID, userID, permission, grantedBy string) (*models.ItemGrant, error) {
	id := newID()
	ts := now()

	_, err := s.db.Exec(s.q(`
		INSERT INTO item_grants (id, item_id, workspace_id, user_id, permission, granted_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`), id, itemID, workspaceID, userID, permission, grantedBy, ts)
	if err != nil {
		return nil, fmt.Errorf("create item grant: %w", err)
	}

	return s.GetItemGrant(id)
}

// GetItemGrant retrieves an item grant by ID.
func (s *Store) GetItemGrant(id string) (*models.ItemGrant, error) {
	var g models.ItemGrant
	var createdAt string

	err := s.db.QueryRow(s.q(`
		SELECT ig.id, ig.item_id, ig.workspace_id, ig.user_id, ig.permission, ig.granted_by, ig.created_at,
		       COALESCE(u.name, ''), COALESCE(u.email, ''), COALESCE(u.username, '')
		FROM item_grants ig
		LEFT JOIN users u ON u.id = ig.user_id
		WHERE ig.id = ?
	`), id).Scan(
		&g.ID, &g.ItemID, &g.WorkspaceID, &g.UserID, &g.Permission, &g.GrantedBy, &createdAt,
		&g.UserName, &g.UserEmail, &g.UserUsername,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item grant: %w", err)
	}

	g.CreatedAt = parseTime(createdAt)
	return &g, nil
}

// ListItemGrants returns all grants on an item.
func (s *Store) ListItemGrants(itemID string) ([]models.ItemGrant, error) {
	rows, err := s.db.Query(s.q(`
		SELECT ig.id, ig.item_id, ig.workspace_id, ig.user_id, ig.permission, ig.granted_by, ig.created_at,
		       COALESCE(u.name, ''), COALESCE(u.email, ''), COALESCE(u.username, '')
		FROM item_grants ig
		LEFT JOIN users u ON u.id = ig.user_id
		WHERE ig.item_id = ?
		ORDER BY ig.created_at ASC
	`), itemID)
	if err != nil {
		return nil, fmt.Errorf("list item grants: %w", err)
	}
	defer rows.Close()

	var result []models.ItemGrant
	for rows.Next() {
		var g models.ItemGrant
		var createdAt string
		if err := rows.Scan(
			&g.ID, &g.ItemID, &g.WorkspaceID, &g.UserID, &g.Permission, &g.GrantedBy, &createdAt,
			&g.UserName, &g.UserEmail, &g.UserUsername,
		); err != nil {
			return nil, err
		}
		g.CreatedAt = parseTime(createdAt)
		result = append(result, g)
	}
	return result, rows.Err()
}

// DeleteItemGrant revokes an item grant by ID.
func (s *Store) DeleteItemGrant(id string) error {
	result, err := s.db.Exec(s.q("DELETE FROM item_grants WHERE id = ?"), id)
	if err != nil {
		return fmt.Errorf("delete item grant: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// --- Cross-cutting queries ---

// ListUserGrants returns all collection and item grants for a user in a workspace.
func (s *Store) ListUserGrants(workspaceID, userID string) ([]models.CollectionGrant, []models.ItemGrant, error) {
	collGrants, err := s.listUserCollectionGrants(workspaceID, userID)
	if err != nil {
		return nil, nil, err
	}
	itemGrants, err := s.listUserItemGrants(workspaceID, userID)
	if err != nil {
		return nil, nil, err
	}
	return collGrants, itemGrants, nil
}

func (s *Store) listUserCollectionGrants(workspaceID, userID string) ([]models.CollectionGrant, error) {
	rows, err := s.db.Query(s.q(`
		SELECT cg.id, cg.collection_id, cg.workspace_id, cg.user_id, cg.permission, cg.granted_by, cg.created_at,
		       COALESCE(u.name, ''), COALESCE(u.email, ''), COALESCE(u.username, '')
		FROM collection_grants cg
		LEFT JOIN users u ON u.id = cg.user_id
		WHERE cg.workspace_id = ? AND cg.user_id = ?
		ORDER BY cg.created_at ASC
	`), workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("list user collection grants: %w", err)
	}
	defer rows.Close()

	var result []models.CollectionGrant
	for rows.Next() {
		var g models.CollectionGrant
		var createdAt string
		if err := rows.Scan(
			&g.ID, &g.CollectionID, &g.WorkspaceID, &g.UserID, &g.Permission, &g.GrantedBy, &createdAt,
			&g.UserName, &g.UserEmail, &g.UserUsername,
		); err != nil {
			return nil, err
		}
		g.CreatedAt = parseTime(createdAt)
		result = append(result, g)
	}
	return result, rows.Err()
}

func (s *Store) listUserItemGrants(workspaceID, userID string) ([]models.ItemGrant, error) {
	rows, err := s.db.Query(s.q(`
		SELECT ig.id, ig.item_id, ig.workspace_id, ig.user_id, ig.permission, ig.granted_by, ig.created_at,
		       COALESCE(u.name, ''), COALESCE(u.email, ''), COALESCE(u.username, '')
		FROM item_grants ig
		LEFT JOIN users u ON u.id = ig.user_id
		WHERE ig.workspace_id = ? AND ig.user_id = ?
		ORDER BY ig.created_at ASC
	`), workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("list user item grants: %w", err)
	}
	defer rows.Close()

	var result []models.ItemGrant
	for rows.Next() {
		var g models.ItemGrant
		var createdAt string
		if err := rows.Scan(
			&g.ID, &g.ItemID, &g.WorkspaceID, &g.UserID, &g.Permission, &g.GrantedBy, &createdAt,
			&g.UserName, &g.UserEmail, &g.UserUsername,
		); err != nil {
			return nil, err
		}
		g.CreatedAt = parseTime(createdAt)
		result = append(result, g)
	}
	return result, rows.Err()
}

// RevokeAllUserGrants deletes all collection and item grants for a user in a workspace.
// Used when removing a member with "revoke all access" option.
func (s *Store) RevokeAllUserGrants(workspaceID, userID string) error {
	_, err := s.db.Exec(s.q("DELETE FROM collection_grants WHERE workspace_id = ? AND user_id = ?"), workspaceID, userID)
	if err != nil {
		return fmt.Errorf("revoke collection grants: %w", err)
	}
	_, err = s.db.Exec(s.q("DELETE FROM item_grants WHERE workspace_id = ? AND user_id = ?"), workspaceID, userID)
	if err != nil {
		return fmt.Errorf("revoke item grants: %w", err)
	}
	return nil
}

// ResolveUserPermission resolves the effective permission for a user on a specific
// item, following the permission resolution order from DOC-406:
// 1. Owner bypass → 2. Item grant → 3. Collection grant → 4. Membership → 5. Deny
// Returns the permission string ("view", "edit", "admin", "owner") or "" if denied.
func (s *Store) ResolveUserPermission(workspaceID, userID, itemID, collectionID string) (string, error) {
	// 1. Is user the workspace owner?
	var ownerID string
	err := s.db.QueryRow(s.q("SELECT owner_id FROM workspaces WHERE id = ?"), workspaceID).Scan(&ownerID)
	if err != nil {
		return "", fmt.Errorf("check workspace owner: %w", err)
	}
	if ownerID == userID {
		return "owner", nil
	}

	// 2. Item-level grant?
	if itemID != "" {
		var perm string
		err := s.db.QueryRow(s.q(
			"SELECT permission FROM item_grants WHERE item_id = ? AND user_id = ?"),
			itemID, userID).Scan(&perm)
		if err == nil {
			return perm, nil
		}
		if err != sql.ErrNoRows {
			return "", fmt.Errorf("check item grant: %w", err)
		}
	}

	// 3. Collection-level grant?
	if collectionID != "" {
		var perm string
		err := s.db.QueryRow(s.q(
			"SELECT permission FROM collection_grants WHERE collection_id = ? AND user_id = ?"),
			collectionID, userID).Scan(&perm)
		if err == nil {
			return perm, nil
		}
		if err != sql.ErrNoRows {
			return "", fmt.Errorf("check collection grant: %w", err)
		}
	}

	// 4. Workspace membership?
	member, err := s.GetWorkspaceMember(workspaceID, userID)
	if err != nil {
		return "", fmt.Errorf("check membership: %w", err)
	}
	if member != nil {
		// Check collection visibility for members with "specific" access
		if collectionID != "" && member.CollectionAccess == "specific" {
			visIDs, err := s.VisibleCollectionIDs(workspaceID, userID)
			if err != nil {
				return "", err
			}
			visible := false
			for _, id := range visIDs {
				if id == collectionID {
					visible = true
					break
				}
			}
			if !visible {
				return "", nil // Collection not visible → deny
			}
		}
		return member.Role, nil // "owner", "editor", or "viewer"
	}

	// 5. Deny
	return "", nil
}
