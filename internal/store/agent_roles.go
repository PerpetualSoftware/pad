package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/xarmian/pad/internal/models"
)

func (s *Store) CreateAgentRole(workspaceID string, input models.AgentRoleCreate) (*models.AgentRole, error) {
	id := newID()
	ts := now()

	slug := input.Slug
	if slug == "" {
		slug = slugify(input.Name)
	}
	if slug == "" {
		slug = "role"
	}
	slug, err := s.uniqueSlug("agent_roles", "workspace_id", workspaceID, slug)
	if err != nil {
		return nil, fmt.Errorf("unique slug: %w", err)
	}

	_, err = s.db.Exec(s.q(`
		INSERT INTO agent_roles (id, workspace_id, slug, name, description, icon, tools, sort_order, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
	`), id, workspaceID, slug, input.Name, input.Description, input.Icon, input.Tools, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("create agent role: %w", err)
	}

	return s.GetAgentRole(workspaceID, id)
}

func (s *Store) GetAgentRole(workspaceID, idOrSlug string) (*models.AgentRole, error) {
	var role models.AgentRole
	var createdAt, updatedAt string

	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, slug, name, description, icon, tools, sort_order, created_at, updated_at
		FROM agent_roles
		WHERE workspace_id = ? AND (id = ? OR slug = ?)
	`), workspaceID, idOrSlug, idOrSlug).Scan(
		&role.ID, &role.WorkspaceID, &role.Slug, &role.Name, &role.Description,
		&role.Icon, &role.Tools, &role.SortOrder, &createdAt, &updatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get agent role: %w", err)
	}

	role.CreatedAt = parseTime(createdAt)
	role.UpdatedAt = parseTime(updatedAt)
	return &role, nil
}

func (s *Store) ListAgentRoles(workspaceID string) ([]models.AgentRole, error) {
	rows, err := s.db.Query(s.q(`
		SELECT r.id, r.workspace_id, r.slug, r.name, r.description, r.icon, r.tools, r.sort_order,
		       r.created_at, r.updated_at,
		       COUNT(i.id) as item_count
		FROM agent_roles r
		LEFT JOIN items i ON i.agent_role_id = r.id AND i.deleted_at IS NULL
		WHERE r.workspace_id = ?
		GROUP BY r.id
		ORDER BY r.sort_order ASC, r.name ASC
	`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list agent roles: %w", err)
	}
	defer rows.Close()

	var roles []models.AgentRole
	for rows.Next() {
		var role models.AgentRole
		var createdAt, updatedAt string
		if err := rows.Scan(
			&role.ID, &role.WorkspaceID, &role.Slug, &role.Name, &role.Description,
			&role.Icon, &role.Tools, &role.SortOrder, &createdAt, &updatedAt, &role.ItemCount,
		); err != nil {
			return nil, err
		}
		role.CreatedAt = parseTime(createdAt)
		role.UpdatedAt = parseTime(updatedAt)
		roles = append(roles, role)
	}
	if roles == nil {
		roles = []models.AgentRole{}
	}
	return roles, rows.Err()
}

func (s *Store) UpdateAgentRole(workspaceID, id string, input models.AgentRoleUpdate) (*models.AgentRole, error) {
	existing, err := s.GetAgentRole(workspaceID, id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}

	ts := now()
	sets := []string{"updated_at = ?"}
	args := []interface{}{ts}

	if input.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *input.Name)
	}
	if input.Slug != nil {
		sets = append(sets, "slug = ?")
		args = append(args, *input.Slug)
	}
	if input.Description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *input.Description)
	}
	if input.Icon != nil {
		sets = append(sets, "icon = ?")
		args = append(args, *input.Icon)
	}
	if input.Tools != nil {
		sets = append(sets, "tools = ?")
		args = append(args, *input.Tools)
	}
	if input.SortOrder != nil {
		sets = append(sets, "sort_order = ?")
		args = append(args, *input.SortOrder)
	}

	args = append(args, existing.ID)
	query := fmt.Sprintf("UPDATE agent_roles SET %s WHERE id = ?", strings.Join(sets, ", "))
	_, err = s.db.Exec(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("update agent role: %w", err)
	}

	return s.GetAgentRole(workspaceID, existing.ID)
}

func (s *Store) DeleteAgentRole(workspaceID, id string) error {
	result, err := s.db.Exec(s.q(`
		DELETE FROM agent_roles WHERE workspace_id = ? AND (id = ? OR slug = ?)
	`), workspaceID, id, id)
	if err != nil {
		return fmt.Errorf("delete agent role: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RoleBreakdown is a summary of items per role for the dashboard.
type RoleBreakdown struct {
	RoleID    *string  `json:"role_id"`
	RoleName  string   `json:"role_name"`
	RoleSlug  string   `json:"role_slug"`
	RoleIcon  string   `json:"role_icon"`
	Tools     string   `json:"tools"`
	ItemCount int      `json:"item_count"`
	Users     []string `json:"assigned_users"`
}

// GetRoleBreakdown returns item counts and assigned users grouped by agent role.
// Includes an entry for unassigned items (role_id = nil).
func (s *Store) GetRoleBreakdown(workspaceID string) ([]RoleBreakdown, error) {
	// Get all roles first (even those with 0 items)
	roles, err := s.ListAgentRoles(workspaceID)
	if err != nil {
		return nil, err
	}

	// Count non-terminal items per role (exclude done/completed/etc. to match board view)
	termPlaceholders, termArgs := models.DefaultTerminalStatusPlaceholders()
	roleCountArgs := append([]any{workspaceID}, termArgs...)
	jsonExtractStatus := s.dialect.JSONExtractText("i.fields", "status")
	groupConcatUsers := s.dialect.GroupConcat("u.name", true)
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT i.agent_role_id, COUNT(*) as cnt, %s as users
		FROM items i
		LEFT JOIN users u ON u.id = i.assigned_user_id
		WHERE i.workspace_id = ? AND i.deleted_at IS NULL
		  AND LOWER(COALESCE(%s, '')) NOT IN
		      (%s)
		GROUP BY i.agent_role_id
	`, groupConcatUsers, jsonExtractStatus, termPlaceholders)), roleCountArgs...)
	if err != nil {
		return nil, fmt.Errorf("role breakdown: %w", err)
	}
	defer rows.Close()

	type countRow struct {
		roleID *string
		count  int
		users  string
	}
	counts := make(map[string]countRow) // key: role ID or ""
	var unassigned countRow

	for rows.Next() {
		var roleID *string
		var cnt int
		var users *string
		if err := rows.Scan(&roleID, &cnt, &users); err != nil {
			return nil, err
		}
		u := ""
		if users != nil {
			u = *users
		}
		if roleID == nil {
			unassigned = countRow{nil, cnt, u}
		} else {
			counts[*roleID] = countRow{roleID, cnt, u}
		}
	}

	var result []RoleBreakdown
	for _, role := range roles {
		cr := counts[role.ID]
		var userList []string
		if cr.users != "" {
			userList = strings.Split(cr.users, ",")
		}
		roleID := role.ID // local copy to avoid pointer to range variable
		result = append(result, RoleBreakdown{
			RoleID:    &roleID,
			RoleName:  role.Name,
			RoleSlug:  role.Slug,
			RoleIcon:  role.Icon,
			Tools:     role.Tools,
			ItemCount: cr.count,
			Users:     userList,
		})
	}

	// Add unassigned
	if unassigned.count > 0 {
		var userList []string
		if unassigned.users != "" {
			userList = strings.Split(unassigned.users, ",")
		}
		result = append(result, RoleBreakdown{
			RoleID:    nil,
			RoleName:  "",
			RoleSlug:  "",
			RoleIcon:  "",
			ItemCount: unassigned.count,
			Users:     userList,
		})
	}

	return result, rows.Err()
}

// RoleBoardLane represents a single lane in the role board view.
type RoleBoardLane struct {
	Role  *models.AgentRole `json:"role"` // nil for unassigned
	Items []models.Item     `json:"items"`
	Users []string          `json:"assigned_users"`
}

// RoleBoardParams configures the role board query.
type RoleBoardParams struct {
	AssignedUserID string
	CollectionIDs  []string // permission filter: restrict to these collection IDs (nil = no filter)
	ItemIDs        []string // permission filter: additionally allow these specific item IDs (for item-level grants)
}

// GetRoleBoardItems returns non-terminal items across all collections, grouped by role.
func (s *Store) GetRoleBoardItems(workspaceID string, params RoleBoardParams) ([]RoleBoardLane, error) {
	// Get all roles
	roles, err := s.ListAgentRoles(workspaceID)
	if err != nil {
		return nil, err
	}

	// Fetch all non-archived items
	listParams := models.ItemListParams{
		IncludeArchived: false,
		CollectionIDs:   params.CollectionIDs,
		ItemIDs:         params.ItemIDs,
	}
	if params.AssignedUserID != "" {
		listParams.AssignedUserID = params.AssignedUserID
	}
	allItems, err := s.ListItems(workspaceID, listParams)
	if err != nil {
		return nil, err
	}

	// Filter out terminal-status items
	var activeItems []models.Item
	for _, item := range allItems {
		status := ""
		if item.Fields != "" && item.Fields != "{}" {
			var fields map[string]interface{}
			if err := json.Unmarshal([]byte(item.Fields), &fields); err == nil {
				if v, ok := fields["status"].(string); ok {
					status = v
				}
			}
		}
		if !models.IsTerminalStatusDefault(status) {
			activeItems = append(activeItems, item)
		}
	}

	// Group by role
	roleMap := make(map[string]*RoleBoardLane) // role ID → lane
	var unassignedLane RoleBoardLane

	for _, role := range roles {
		r := role // copy
		roleMap[role.ID] = &RoleBoardLane{
			Role:  &r,
			Items: []models.Item{},
			Users: []string{},
		}
	}

	userSets := make(map[string]map[string]bool) // role ID → set of user names

	for _, item := range activeItems {
		var lane *RoleBoardLane
		roleKey := ""
		if item.AgentRoleID != nil && *item.AgentRoleID != "" {
			roleKey = *item.AgentRoleID
			lane = roleMap[roleKey]
		}
		if lane == nil {
			roleKey = ""
			unassignedLane.Items = append(unassignedLane.Items, item)
			if item.AssignedUserName != "" {
				if unassignedLane.Users == nil {
					unassignedLane.Users = []string{}
				}
			}
			continue
		}
		lane.Items = append(lane.Items, item)
		if item.AssignedUserName != "" {
			if userSets[roleKey] == nil {
				userSets[roleKey] = make(map[string]bool)
			}
			userSets[roleKey][item.AssignedUserName] = true
		}
	}

	// Build result in role order, sorting items within each lane by role_sort_order
	var result []RoleBoardLane
	for _, role := range roles {
		lane := roleMap[role.ID]
		if us, ok := userSets[role.ID]; ok {
			for u := range us {
				lane.Users = append(lane.Users, u)
			}
		}
		sort.Slice(lane.Items, func(i, j int) bool {
			return lane.Items[i].RoleSortOrder < lane.Items[j].RoleSortOrder
		})
		result = append(result, *lane)
	}

	// Add unassigned lane (sorted by role_sort_order)
	sort.Slice(unassignedLane.Items, func(i, j int) bool {
		return unassignedLane.Items[i].RoleSortOrder < unassignedLane.Items[j].RoleSortOrder
	})
	if len(unassignedLane.Items) > 0 {
		// Dedupe users
		userSet := make(map[string]bool)
		for _, item := range unassignedLane.Items {
			if item.AssignedUserName != "" {
				userSet[item.AssignedUserName] = true
			}
		}
		unassignedLane.Users = []string{}
		for u := range userSet {
			unassignedLane.Users = append(unassignedLane.Users, u)
		}
		result = append(result, unassignedLane)
	}

	return result, nil
}

// RoleOrderUpdate represents a single role's sort_order update for lane reordering.
type RoleOrderUpdate struct {
	RoleID    string `json:"role_id"`
	SortOrder int    `json:"sort_order"`
}

// UpdateAgentRoleOrder batch-updates sort_order for a list of roles.
func (s *Store) UpdateAgentRoleOrder(workspaceID string, updates []RoleOrderUpdate) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(s.q("UPDATE agent_roles SET sort_order = ?, updated_at = ? WHERE id = ? AND workspace_id = ?"))
	if err != nil {
		return fmt.Errorf("prepare role order update: %w", err)
	}
	defer stmt.Close()

	ts := now()
	for _, u := range updates {
		if _, err := stmt.Exec(u.SortOrder, ts, u.RoleID, workspaceID); err != nil {
			return fmt.Errorf("update sort order for role %s: %w", u.RoleID, err)
		}
	}

	return tx.Commit()
}

// RoleSortUpdate represents a single item's role_sort_order update.
type RoleSortUpdate struct {
	ItemID        string `json:"item_id"`
	RoleSortOrder int    `json:"role_sort_order"`
}

// UpdateRoleSortOrder batch-updates role_sort_order for a list of items.
func (s *Store) UpdateRoleSortOrder(workspaceID string, updates []RoleSortUpdate) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(s.q("UPDATE items SET role_sort_order = ? WHERE id = ? AND workspace_id = ?"))
	if err != nil {
		return fmt.Errorf("prepare role sort update: %w", err)
	}
	defer stmt.Close()

	for _, u := range updates {
		if _, err := stmt.Exec(u.RoleSortOrder, u.ItemID, workspaceID); err != nil {
			return fmt.Errorf("update role sort for %s: %w", u.ItemID, err)
		}
	}

	return tx.Commit()
}

// ResolveAgentRoleID resolves a role identifier (ID or slug) to its UUID.
// Returns empty string if the role doesn't exist.
func (s *Store) ResolveAgentRoleID(workspaceID, idOrSlug string) (string, error) {
	role, err := s.GetAgentRole(workspaceID, idOrSlug)
	if err != nil {
		return "", err
	}
	if role == nil {
		return "", nil
	}
	return role.ID, nil
}

