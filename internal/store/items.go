package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/xarmian/pad/internal/diff"
	"github.com/xarmian/pad/internal/models"
)

// childLinkTypes lists the link types that establish a parent→child relationship
// for progress tracking. Both 'parent' and 'implements' links count as children.
var childLinkTypes = []string{"parent", "implements"}

// childLinkTypeSQL returns a SQL IN clause fragment like "'parent','implements'"
// for filtering item_links by child relationship types.
func childLinkTypeSQL() string {
	quoted := make([]string, len(childLinkTypes))
	for i, t := range childLinkTypes {
		quoted[i] = "'" + t + "'"
	}
	return strings.Join(quoted, ",")
}

// ItemSearchResult holds FTS search results for items.
type ItemSearchResult struct {
	Item    models.Item `json:"item"`
	Snippet string      `json:"snippet"`
	Rank    float64     `json:"rank"`
}

// validateAssignmentScope checks that the assigned user and agent role belong to the
// same workspace as the item. This prevents cross-workspace assignment leaks.
func (s *Store) validateAssignmentScope(workspaceID string, assignedUserID, agentRoleID *string) error {
	if assignedUserID != nil && *assignedUserID != "" {
		isMember, err := s.IsWorkspaceMember(workspaceID, *assignedUserID)
		if err != nil {
			return fmt.Errorf("validate assigned user: %w", err)
		}
		if !isMember {
			return fmt.Errorf("assigned user is not a member of this workspace")
		}
	}
	if agentRoleID != nil && *agentRoleID != "" {
		role, err := s.GetAgentRole(workspaceID, *agentRoleID)
		if err != nil {
			return fmt.Errorf("validate agent role: %w", err)
		}
		if role == nil {
			return fmt.Errorf("agent role does not belong to this workspace")
		}
	}
	return nil
}

// maxItemNumberRetries is the number of times CreateItem will retry when a
// concurrent insert claims the same workspace-global item_number.
const maxItemNumberRetries = 10

func (s *Store) CreateItem(workspaceID, collectionID string, input models.ItemCreate) (*models.Item, error) {
	// Validate assignment scope before writing
	if err := s.validateAssignmentScope(workspaceID, input.AssignedUserID, input.AgentRoleID); err != nil {
		return nil, err
	}

	id := newID()
	ts := now()

	fields := input.Fields
	if fields == "" {
		fields = "{}"
	}
	tags := input.Tags
	if tags == "" {
		tags = "[]"
	}
	createdBy := input.CreatedBy
	if createdBy == "" {
		createdBy = "user"
	}
	source := input.Source
	if source == "" {
		source = "web"
	}

	baseSlug := slugify(input.Title)
	if baseSlug == "" {
		baseSlug = "untitled"
	}
	slug, err := s.uniqueSlug("items", "workspace_id", workspaceID, baseSlug)
	if err != nil {
		return nil, fmt.Errorf("unique slug: %w", err)
	}

	// Retry loop: if a concurrent insert claims the same item_number we
	// roll back and re-read MAX(item_number) on the next attempt.
	var lastErr error
	for attempt := 0; attempt < maxItemNumberRetries; attempt++ {
		lastErr = s.tryCreateItem(id, workspaceID, collectionID, slug, ts, fields, tags, createdBy, source, input)
		if lastErr == nil {
			return s.GetItem(id)
		}
		// Only retry on unique-constraint violations (item_number conflict)
		if !isUniqueViolation(lastErr) {
			return nil, fmt.Errorf("insert item: %w", lastErr)
		}
	}
	return nil, fmt.Errorf("insert item after %d retries: %w", maxItemNumberRetries, lastErr)
}

// tryCreateItem attempts a single transactional insert of an item with the
// next available workspace-global item_number. The item_number is computed
// atomically via a subquery in the INSERT to avoid races between concurrent
// inserts reading the same MAX(item_number).
func (s *Store) tryCreateItem(id, workspaceID, collectionID, slug, ts, fields, tags, createdBy, source string, input models.ItemCreate) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// PostgreSQL: take an advisory lock keyed on the workspace to serialize
	// item_number assignment. This eliminates the race between concurrent
	// transactions reading the same MAX(item_number). The lock is released
	// automatically when the transaction commits or rolls back.
	// SQLite: single-writer by design, no advisory locks needed.
	if s.dialect.Driver() == DriverPostgres {
		// Use a hash of the workspace ID as the advisory lock key.
		_, err = tx.Exec("SELECT pg_advisory_xact_lock(hashtext($1))", workspaceID)
		if err != nil {
			return fmt.Errorf("advisory lock: %w", err)
		}
	}

	// Compute and insert the next item_number atomically within the lock.
	_, err = tx.Exec(s.q(`
		INSERT INTO items (id, workspace_id, collection_id, title, slug, content, fields, tags,
		                   pinned, sort_order, parent_id, assigned_user_id, agent_role_id, role_sort_order,
		                   created_by, last_modified_by, source, item_number, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, 0, ?, ?, ?,
		        (SELECT COALESCE(MAX(item_number), 0) + 1 FROM items WHERE workspace_id = ?),
		        ?, ?)
	`), id, workspaceID, collectionID, input.Title, slug, input.Content, fields, tags,
		s.dialect.BoolToInt(input.Pinned), input.ParentID, input.AssignedUserID, input.AgentRoleID,
		createdBy, createdBy, source, workspaceID, ts, ts)
	if err != nil {
		return err
	}

	// Create initial version if there's content
	if input.Content != "" {
		vid := newID()
		_, err = tx.Exec(s.q(`
			INSERT INTO item_versions (id, item_id, content, change_summary, created_by, source, is_diff, created_at)
			VALUES (?, ?, ?, '', ?, ?, ?, ?)
		`), vid, id, input.Content, createdBy, source, s.dialect.BoolToInt(false), ts)
		if err != nil {
			return fmt.Errorf("create initial version: %w", err)
		}
	}

	return tx.Commit()
}

// isUniqueViolation checks whether an error is a unique constraint violation.
// Works for both SQLite (UNIQUE constraint failed) and PostgreSQL (duplicate key).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value violates unique constraint")
}

func (s *Store) GetItem(id string) (*models.Item, error) {
	var item models.Item
	var createdAt, updatedAt string
	var deletedAt *string
	var pinned bool

	err := s.db.QueryRow(s.q(`
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.created_at, i.updated_at, i.deleted_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE i.id = ? AND i.deleted_at IS NULL
	`), id).Scan(
		&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
		&item.Content, &item.Fields, &item.Tags,
		&pinned, &item.SortOrder, &item.ParentID, &item.AssignedUserID, &item.AgentRoleID, &item.RoleSortOrder,
		&item.CreatedBy, &item.LastModifiedBy, &item.Source,
		&item.ItemNumber, &createdAt, &updatedAt, &deletedAt,
		&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
		&item.AssignedUserName, &item.AssignedUserEmail,
		&item.AgentRoleName, &item.AgentRoleSlug, &item.AgentRoleIcon,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item: %w", err)
	}

	item.Pinned = pinned
	item.CreatedAt = parseTime(createdAt)
	item.UpdatedAt = parseTime(updatedAt)
	item.DeletedAt = parseTimePtr(deletedAt)
	hydrateItemComputedMetadata(&item)
	return &item, nil
}

func (s *Store) GetItemBySlug(workspaceID, slug string) (*models.Item, error) {
	var id string
	err := s.db.QueryRow(s.q(`
		SELECT id FROM items
		WHERE workspace_id = ? AND slug = ? AND deleted_at IS NULL
	`), workspaceID, slug).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item by slug: %w", err)
	}
	return s.GetItem(id)
}

// GetItemByRef looks up an item by its PREFIX-NUMBER reference (e.g. "IDEA-15").
// Since item numbers are workspace-unique, this first tries an exact prefix match
// and falls back to a number-only lookup. This allows old refs to still resolve
// after an item has been moved to a different collection (e.g. PLAN-42 still
// finds the item even after it became TASK-42).
func (s *Store) GetItemByRef(workspaceID, prefix string, number int) (*models.Item, error) {
	var id string
	// Try exact prefix + number match first
	err := s.db.QueryRow(s.q(`
		SELECT i.id FROM items i
		JOIN collections c ON c.id = i.collection_id
		WHERE i.workspace_id = ? AND c.prefix = ? AND i.item_number = ? AND i.deleted_at IS NULL
	`), workspaceID, prefix, number).Scan(&id)
	if err == nil {
		return s.GetItem(id)
	}
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("get item by ref: %w", err)
	}

	// Fallback: item numbers are workspace-unique, so look up by number alone.
	// This handles the case where an item was moved to a different collection
	// but is still being referenced by its old prefix.
	err = s.db.QueryRow(s.q(`
		SELECT id FROM items
		WHERE workspace_id = ? AND item_number = ? AND deleted_at IS NULL
	`), workspaceID, number).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item by number: %w", err)
	}
	return s.GetItem(id)
}

// ResolveItem looks up an item by UUID, PREFIX-NUMBER ref (e.g. "IDEA-15"),
// or slug. UUID is tried first, then ref, then slug.
func (s *Store) ResolveItem(workspaceID, identifier string) (*models.Item, error) {
	// Try UUID lookup first (8-4-4-4-12 hex format)
	if isUUID(identifier) {
		item, err := s.GetItem(identifier)
		if err != nil {
			return nil, err
		}
		if item != nil && item.WorkspaceID == workspaceID {
			return item, nil
		}
	}
	// Try PREFIX-NUMBER ref
	if prefix, number, ok := parseItemRef(identifier); ok {
		item, err := s.GetItemByRef(workspaceID, prefix, number)
		if err != nil {
			return nil, err
		}
		if item != nil {
			return item, nil
		}
	}
	// Fall back to slug lookup
	return s.GetItemBySlug(workspaceID, identifier)
}

// isUUID checks if a string looks like a UUID (8-4-4-4-12 hex).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// ResolveItemIncludeDeleted is like ResolveItem but includes soft-deleted items.
func (s *Store) ResolveItemIncludeDeleted(workspaceID, slugOrRef string) (*models.Item, error) {
	if prefix, number, ok := parseItemRef(slugOrRef); ok {
		var item models.Item
		var createdAt, updatedAt string
		var deletedAt *string
		var pinned bool

		err := s.db.QueryRow(s.q(`
			SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
			       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
			       i.created_by, i.last_modified_by, i.source,
			       i.item_number, i.created_at, i.updated_at, i.deleted_at,
			       c.slug, c.name, c.icon, c.prefix,
			       COALESCE(au.name, ''), COALESCE(au.email, ''),
			       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
			FROM items i
			JOIN collections c ON c.id = i.collection_id
			LEFT JOIN users au ON au.id = i.assigned_user_id
			LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
			WHERE i.workspace_id = ? AND c.prefix = ? AND i.item_number = ?
		`), workspaceID, prefix, number).Scan(
			&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
			&item.Content, &item.Fields, &item.Tags,
			&pinned, &item.SortOrder, &item.ParentID, &item.AssignedUserID, &item.AgentRoleID, &item.RoleSortOrder,
			&item.CreatedBy, &item.LastModifiedBy, &item.Source,
			&item.ItemNumber, &createdAt, &updatedAt, &deletedAt,
			&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
			&item.AssignedUserName, &item.AssignedUserEmail,
			&item.AgentRoleName, &item.AgentRoleSlug, &item.AgentRoleIcon,
		)
		if err == nil {
			item.Pinned = pinned
			item.CreatedAt = parseTime(createdAt)
			item.UpdatedAt = parseTime(updatedAt)
			item.DeletedAt = parseTimePtr(deletedAt)
			hydrateItemComputedMetadata(&item)
			return &item, nil
		}
		if err != sql.ErrNoRows {
			return nil, fmt.Errorf("resolve ref (include deleted): %w", err)
		}
	}
	return s.GetItemBySlugIncludeDeleted(workspaceID, slugOrRef)
}

// parseItemRef parses "PREFIX-123" into ("PREFIX", 123, true).
// Returns false if the string is not a valid item ref.
// Case-insensitive: "task-5", "Task-5", and "TASK-5" all parse to ("TASK", 5, true).
func parseItemRef(s string) (string, int, bool) {
	s = strings.ToUpper(s)
	idx := strings.LastIndex(s, "-")
	if idx <= 0 || idx == len(s)-1 {
		return "", 0, false
	}
	prefix := s[:idx]
	// Prefix must be all uppercase letters
	for _, c := range prefix {
		if c < 'A' || c > 'Z' {
			return "", 0, false
		}
	}
	numStr := s[idx+1:]
	num := 0
	for _, c := range numStr {
		if c < '0' || c > '9' {
			return "", 0, false
		}
		num = num*10 + int(c-'0')
	}
	if num == 0 {
		return "", 0, false
	}
	return prefix, num, true
}

// GetItemBySlugIncludeDeleted finds an item by slug including soft-deleted items.
// Used for restore operations where the item is archived.
func (s *Store) GetItemBySlugIncludeDeleted(workspaceID, slug string) (*models.Item, error) {
	var item models.Item
	var createdAt, updatedAt string
	var deletedAt *string
	var pinned bool

	err := s.db.QueryRow(s.q(`
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.created_at, i.updated_at, i.deleted_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE i.workspace_id = ? AND i.slug = ?
	`), workspaceID, slug).Scan(
		&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
		&item.Content, &item.Fields, &item.Tags,
		&pinned, &item.SortOrder, &item.ParentID, &item.AssignedUserID, &item.AgentRoleID, &item.RoleSortOrder,
		&item.CreatedBy, &item.LastModifiedBy, &item.Source,
		&item.ItemNumber, &createdAt, &updatedAt, &deletedAt,
		&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
		&item.AssignedUserName, &item.AssignedUserEmail,
		&item.AgentRoleName, &item.AgentRoleSlug, &item.AgentRoleIcon,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item by slug (include deleted): %w", err)
	}

	item.Pinned = pinned
	item.CreatedAt = parseTime(createdAt)
	item.UpdatedAt = parseTime(updatedAt)
	item.DeletedAt = parseTimePtr(deletedAt)
	hydrateItemComputedMetadata(&item)
	return &item, nil
}

func (s *Store) ListItems(workspaceID string, params models.ItemListParams) ([]models.Item, error) {
	// Non-nil empty CollectionIDs means "no visible collections" — return
	// empty results immediately, unless ItemIDs are also provided (item-level
	// grants may still allow access to specific items even without full
	// collection access).
	if params.CollectionIDs != nil && len(params.CollectionIDs) == 0 && len(params.ItemIDs) == 0 {
		return nil, nil
	}

	// When search is specified, use FTS
	if params.Search != "" {
		return s.listItemsFTS(workspaceID, params)
	}

	query := `
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.created_at, i.updated_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE i.workspace_id = ?
	`
	args := []interface{}{workspaceID}

	if !params.IncludeArchived {
		query += " AND i.deleted_at IS NULL"
	}

	if params.CollectionSlug != "" {
		query += " AND c.slug = ?"
		args = append(args, params.CollectionSlug)
	}

	if len(params.CollectionIDs) > 0 && len(params.ItemIDs) > 0 {
		// Guest with both collection-level and item-level grants:
		// item must be in a fully-granted collection OR be a specifically granted item
		collPlaceholders := make([]string, len(params.CollectionIDs))
		for i, id := range params.CollectionIDs {
			collPlaceholders[i] = "?"
			args = append(args, id)
		}
		itemPlaceholders := make([]string, len(params.ItemIDs))
		for i, id := range params.ItemIDs {
			itemPlaceholders[i] = "?"
			args = append(args, id)
		}
		query += " AND (i.collection_id IN (" + strings.Join(collPlaceholders, ",") + ") OR i.id IN (" + strings.Join(itemPlaceholders, ",") + "))"
	} else if len(params.CollectionIDs) > 0 {
		placeholders := make([]string, len(params.CollectionIDs))
		for i, id := range params.CollectionIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND i.collection_id IN (" + strings.Join(placeholders, ",") + ")"
	} else if len(params.ItemIDs) > 0 {
		// Guest with only item-level grants (no collection-level grants)
		placeholders := make([]string, len(params.ItemIDs))
		for i, id := range params.ItemIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND i.id IN (" + strings.Join(placeholders, ",") + ")"
	}

	if params.Tag != "" {
		tagExpr, tagArg := s.dialect.JSONArrayContains("i.tags", params.Tag)
		query += " AND " + tagExpr
		args = append(args, tagArg)
	}

	if params.ParentID != "" {
		query += " AND i.parent_id = ?"
		args = append(args, params.ParentID)
	}

	if params.AssignedUserID != "" {
		query += " AND i.assigned_user_id = ?"
		args = append(args, params.AssignedUserID)
	}

	if params.AgentRoleID != "" {
		query += " AND (i.agent_role_id = ? OR ar.slug = ?)"
		args = append(args, params.AgentRoleID, params.AgentRoleID)
	}

	// Parent link filter via item_links. Joins items so we ignore links pointing
	// to a soft-deleted parent — slug/ref filtering already rejects deleted
	// parents upstream, but raw-UUID input bypasses that path. See BUG-734 /
	// Codex review on PR #259.
	if params.ParentLinkID != "" {
		query += " AND EXISTS (SELECT 1 FROM item_links il JOIN items p ON p.id = il.target_id AND p.deleted_at IS NULL WHERE il.source_id = i.id AND il.link_type = 'parent' AND il.target_id = ?)"
		args = append(args, params.ParentLinkID)
	}

	// Field filters — supports comma-separated values as OR
	for key, value := range params.Fields {
		// Sanitize the key to prevent SQL injection — field names must be
		// alphanumeric/underscore only (user-controlled from query params).
		if !isValidFieldKey(key) {
			continue
		}
		jsonExpr := s.dialect.JSONExtractText("i.fields", key)
		if strings.Contains(value, ",") {
			values := strings.Split(value, ",")
			placeholders := make([]string, len(values))
			for i, v := range values {
				placeholders[i] = "?"
				args = append(args, strings.TrimSpace(v))
			}
			query += " AND " + jsonExpr + " IN (" + strings.Join(placeholders, ",") + ")"
		} else {
			query += " AND " + jsonExpr + " = ?"
			args = append(args, value)
		}
	}

	// Sorting
	query += buildItemSort(params.Sort, s.dialect)

	// Pagination
	if params.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, params.Limit)
		if params.Offset > 0 {
			query += " OFFSET ?"
			args = append(args, params.Offset)
		}
	}

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list items: %w", err)
	}
	defer rows.Close()

	return scanItems(rows)
}

func (s *Store) listItemsFTS(workspaceID string, params models.ItemListParams) ([]models.Item, error) {
	var query string
	var args []interface{}
	var ftsRank string

	if s.dialect.Driver() == DriverPostgres {
		// PostgreSQL: search_vector lives on the items table (aliased as "i").
		ftsMatch := s.dialect.FTSMatch("i", "search_vector")
		ftsRank = s.dialect.FTSRank("i", "search_vector")

		query = fmt.Sprintf(`
			SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
			       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
			       i.created_by, i.last_modified_by, i.source,
			       i.item_number, i.created_at, i.updated_at,
			       c.slug, c.name, c.icon, c.prefix,
			       COALESCE(au.name, ''), COALESCE(au.email, ''),
			       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
			FROM items i
			JOIN collections c ON c.id = i.collection_id
			LEFT JOIN users au ON au.id = i.assigned_user_id
			LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
			WHERE i.workspace_id = ? AND i.deleted_at IS NULL
			AND %s
		`, ftsMatch)
		args = []interface{}{workspaceID, params.Search}
	} else {
		// SQLite: uses FTS5 virtual table "items_fts".
		ftsMatch := s.dialect.FTSMatch("items_fts", "search_vector")
		ftsRank = s.dialect.FTSRank("items_fts", "search_vector")

		query = fmt.Sprintf(`
			SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
			       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
			       i.created_by, i.last_modified_by, i.source,
			       i.item_number, i.created_at, i.updated_at,
			       c.slug, c.name, c.icon, c.prefix,
			       COALESCE(au.name, ''), COALESCE(au.email, ''),
			       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
			FROM items i
			JOIN items_fts fts ON i.rowid = fts.rowid
			JOIN collections c ON c.id = i.collection_id
			LEFT JOIN users au ON au.id = i.assigned_user_id
			LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
			WHERE i.workspace_id = ? AND i.deleted_at IS NULL
			AND %s
		`, ftsMatch)
		args = []interface{}{workspaceID, params.Search}
	}

	if params.CollectionSlug != "" {
		query += " AND c.slug = ?"
		args = append(args, params.CollectionSlug)
	}

	// Parent link filter — mirrors the non-FTS path so combining
	// `parent=<UUID>&search=<q>` doesn't silently drop the parent constraint
	// (and, by extension, the soft-deleted-parent rejection from BUG-734).
	if params.ParentLinkID != "" {
		query += " AND EXISTS (SELECT 1 FROM item_links il JOIN items p ON p.id = il.target_id AND p.deleted_at IS NULL WHERE il.source_id = i.id AND il.link_type = 'parent' AND il.target_id = ?)"
		args = append(args, params.ParentLinkID)
	}

	if len(params.CollectionIDs) > 0 && len(params.ItemIDs) > 0 {
		collPlaceholders := make([]string, len(params.CollectionIDs))
		for i, id := range params.CollectionIDs {
			collPlaceholders[i] = "?"
			args = append(args, id)
		}
		itemPlaceholders := make([]string, len(params.ItemIDs))
		for i, id := range params.ItemIDs {
			itemPlaceholders[i] = "?"
			args = append(args, id)
		}
		query += " AND (i.collection_id IN (" + strings.Join(collPlaceholders, ",") + ") OR i.id IN (" + strings.Join(itemPlaceholders, ",") + "))"
	} else if len(params.CollectionIDs) > 0 {
		placeholders := make([]string, len(params.CollectionIDs))
		for i, id := range params.CollectionIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND i.collection_id IN (" + strings.Join(placeholders, ",") + ")"
	} else if len(params.ItemIDs) > 0 {
		placeholders := make([]string, len(params.ItemIDs))
		for i, id := range params.ItemIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND i.id IN (" + strings.Join(placeholders, ",") + ")"
	}

	// Filter parity with the non-FTS path. Without these, `?search=...` combined
	// with any of these filter params silently drops the filter and over-returns
	// items. See BUG-812.

	if params.Tag != "" {
		tagExpr, tagArg := s.dialect.JSONArrayContains("i.tags", params.Tag)
		query += " AND " + tagExpr
		args = append(args, tagArg)
	}

	if params.ParentID != "" {
		query += " AND i.parent_id = ?"
		args = append(args, params.ParentID)
	}

	if params.AssignedUserID != "" {
		query += " AND i.assigned_user_id = ?"
		args = append(args, params.AssignedUserID)
	}

	if params.AgentRoleID != "" {
		query += " AND (i.agent_role_id = ? OR ar.slug = ?)"
		args = append(args, params.AgentRoleID, params.AgentRoleID)
	}

	// Field filters — supports comma-separated values as OR. Field keys are
	// user-controlled (query params), so isValidFieldKey gates SQL composition.
	for key, value := range params.Fields {
		if !isValidFieldKey(key) {
			continue
		}
		jsonExpr := s.dialect.JSONExtractText("i.fields", key)
		if strings.Contains(value, ",") {
			values := strings.Split(value, ",")
			placeholders := make([]string, len(values))
			for i, v := range values {
				placeholders[i] = "?"
				args = append(args, strings.TrimSpace(v))
			}
			query += " AND " + jsonExpr + " IN (" + strings.Join(placeholders, ",") + ")"
		} else {
			query += " AND " + jsonExpr + " = ?"
			args = append(args, value)
		}
	}

	// SQLite bm25(): more negative = more relevant → ASC (default).
	// PostgreSQL ts_rank(): higher = more relevant → DESC.
	// PostgreSQL FTSRank embeds a plainto_tsquery(?) that needs the search term.
	if s.dialect.Driver() == DriverPostgres {
		query += " ORDER BY " + ftsRank + " DESC"
		args = append(args, params.Search)
	} else {
		query += " ORDER BY " + ftsRank
	}

	if params.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, params.Limit)
	}

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("search items: %w", err)
	}
	defer rows.Close()

	return scanItems(rows)
}

func (s *Store) UpdateItem(id string, input models.ItemUpdate) (*models.Item, error) {
	existing, err := s.GetItem(id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}

	// Validate assignment scope before writing
	if err := s.validateAssignmentScope(existing.WorkspaceID, input.AssignedUserID, input.AgentRoleID); err != nil {
		return nil, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	ts := now()

	// Create version if content is changing
	if input.Content != nil && *input.Content != existing.Content {
		createdBy := input.LastModifiedBy
		if createdBy == "" {
			createdBy = "user"
		}
		source := input.Source
		if source == "" {
			source = "web"
		}

		forceVersion := input.Title != nil && *input.Title != existing.Title
		shouldVersion := forceVersion
		if !shouldVersion {
			shouldVersion, err = s.shouldCreateItemVersion(id, createdBy, source)
			if err != nil {
				return nil, fmt.Errorf("check version throttle: %w", err)
			}
		}

		if shouldVersion {
			vid := newID()
			versionContent := existing.Content
			isDiff := false
			patch := diff.CreateReversePatch(existing.Content, *input.Content)
			if diff.IsDiffSmaller(patch, existing.Content) {
				versionContent = patch
				isDiff = true
			}

			_, err = tx.Exec(s.q(`
				INSERT INTO item_versions (id, item_id, content, change_summary, created_by, source, is_diff, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			`), vid, id, versionContent, input.ChangeSummary, createdBy, source, s.dialect.BoolToInt(isDiff), ts)
			if err != nil {
				return nil, fmt.Errorf("create version: %w", err)
			}
		}
	}

	// Build update query
	sets := []string{"updated_at = ?"}
	args := []interface{}{ts}

	if input.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *input.Title)
		baseSlug := slugify(*input.Title)
		if baseSlug == "" {
			baseSlug = "untitled"
		}
		newSlug, err := s.uniqueSlugExcluding("items", "workspace_id", existing.WorkspaceID, baseSlug, id)
		if err != nil {
			return nil, fmt.Errorf("unique slug: %w", err)
		}
		sets = append(sets, "slug = ?")
		args = append(args, newSlug)
	}
	if input.Content != nil {
		sets = append(sets, "content = ?")
		args = append(args, *input.Content)
	}
	if input.Fields != nil {
		sets = append(sets, "fields = ?")
		args = append(args, *input.Fields)
	}
	if input.Tags != nil {
		sets = append(sets, "tags = ?")
		args = append(args, *input.Tags)
	}
	if input.Pinned != nil {
		sets = append(sets, "pinned = ?")
		args = append(args, s.dialect.BoolToInt(*input.Pinned))
	}
	if input.SortOrder != nil {
		sets = append(sets, "sort_order = ?")
		args = append(args, *input.SortOrder)
	}
	if input.ParentID != nil {
		sets = append(sets, "parent_id = ?")
		args = append(args, *input.ParentID)
	}
	if input.AssignedUserID != nil {
		sets = append(sets, "assigned_user_id = ?")
		args = append(args, *input.AssignedUserID)
	} else if input.ClearAssignedUser {
		sets = append(sets, "assigned_user_id = NULL")
	}
	if input.AgentRoleID != nil {
		sets = append(sets, "agent_role_id = ?")
		args = append(args, *input.AgentRoleID)
	} else if input.ClearAgentRole {
		sets = append(sets, "agent_role_id = NULL")
	}
	if input.LastModifiedBy != "" {
		sets = append(sets, "last_modified_by = ?")
		args = append(args, input.LastModifiedBy)
	}
	if input.Source != "" {
		sets = append(sets, "source = ?")
		args = append(args, input.Source)
	}

	args = append(args, id)
	query := fmt.Sprintf("UPDATE items SET %s WHERE id = ?", strings.Join(sets, ", "))
	_, err = tx.Exec(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("update item: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetItem(id)
}

func (s *Store) DeleteItem(id string) error {
	ts := now()
	result, err := s.db.Exec(s.q(`
		UPDATE items SET deleted_at = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL
	`), ts, ts, id)
	if err != nil {
		return fmt.Errorf("delete item: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) RestoreItem(id string) (*models.Item, error) {
	ts := now()
	result, err := s.db.Exec(s.q(`
		UPDATE items SET deleted_at = NULL, updated_at = ?
		WHERE id = ? AND deleted_at IS NOT NULL
	`), ts, id)
	if err != nil {
		return nil, fmt.Errorf("restore item: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return nil, sql.ErrNoRows
	}
	return s.GetItem(id)
}

func (s *Store) SearchItems(workspaceID, query string) ([]ItemSearchResult, error) {
	var sqlQuery string
	var args []interface{}

	if s.dialect.Driver() == DriverPostgres {
		// PostgreSQL: search_vector lives on the items table (aliased as "i").
		ftsSnippet := s.dialect.FTSSnippet("i", 1, "i.content")
		ftsMatch := s.dialect.FTSMatch("i", "search_vector")
		ftsRank := s.dialect.FTSRank("i", "search_vector")

		sqlQuery = fmt.Sprintf(`
			SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
			       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
			       i.created_by, i.last_modified_by, i.source,
			       i.item_number, i.created_at, i.updated_at,
			       c.slug, c.name, c.icon, c.prefix,
			       COALESCE(au.name, ''), COALESCE(au.email, ''),
			       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, ''),
			       %s as snippet,
			       %s as rank_score
			FROM items i
			JOIN collections c ON c.id = i.collection_id
			LEFT JOIN users au ON au.id = i.assigned_user_id
			LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
			WHERE %s
			AND i.deleted_at IS NULL
		`, ftsSnippet, ftsRank, ftsMatch)
		// PostgreSQL: FTSSnippet, FTSRank, and FTSMatch each consume a "?" for plainto_tsquery
		args = []interface{}{query, query, query}
	} else {
		// SQLite: uses FTS5 virtual table "items_fts".
		ftsSnippet := s.dialect.FTSSnippet("items_fts", 1, "i.content")
		ftsMatch := s.dialect.FTSMatch("items_fts", "search_vector")
		ftsRank := s.dialect.FTSRank("items_fts", "search_vector")

		sqlQuery = fmt.Sprintf(`
			SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
			       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
			       i.created_by, i.last_modified_by, i.source,
			       i.item_number, i.created_at, i.updated_at,
			       c.slug, c.name, c.icon, c.prefix,
			       COALESCE(au.name, ''), COALESCE(au.email, ''),
			       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, ''),
			       %s as snippet,
			       %s as rank_score
			FROM items_fts fts
			JOIN items i ON i.rowid = fts.rowid
			JOIN collections c ON c.id = i.collection_id
			LEFT JOIN users au ON au.id = i.assigned_user_id
			LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
			WHERE %s
			AND i.deleted_at IS NULL
		`, ftsSnippet, ftsRank, ftsMatch)
		args = []interface{}{query}
	}

	if workspaceID != "" {
		sqlQuery += " AND i.workspace_id = ?"
		args = append(args, workspaceID)
	}

	if s.dialect.Driver() == DriverPostgres {
		sqlQuery += " ORDER BY rank_score DESC LIMIT 50"
	} else {
		sqlQuery += " ORDER BY rank_score LIMIT 50"
	}

	rows, err := s.db.Query(s.q(sqlQuery), args...)
	if err != nil {
		return nil, fmt.Errorf("search items: %w", err)
	}
	defer rows.Close()

	var results []ItemSearchResult
	for rows.Next() {
		var r ItemSearchResult
		var createdAt, updatedAt string
		var pinned bool
		if err := rows.Scan(
			&r.Item.ID, &r.Item.WorkspaceID, &r.Item.CollectionID, &r.Item.Title, &r.Item.Slug,
			&r.Item.Content, &r.Item.Fields, &r.Item.Tags,
			&pinned, &r.Item.SortOrder, &r.Item.ParentID, &r.Item.AssignedUserID, &r.Item.AgentRoleID, &r.Item.RoleSortOrder,
			&r.Item.CreatedBy, &r.Item.LastModifiedBy,
			&r.Item.Source, &r.Item.ItemNumber, &createdAt, &updatedAt,
			&r.Item.CollectionSlug, &r.Item.CollectionName, &r.Item.CollectionIcon, &r.Item.CollectionPrefix,
			&r.Item.AssignedUserName, &r.Item.AssignedUserEmail,
			&r.Item.AgentRoleName, &r.Item.AgentRoleSlug, &r.Item.AgentRoleIcon,
			&r.Snippet, &r.Rank,
		); err != nil {
			return nil, err
		}
		r.Item.Pinned = pinned
		r.Item.CreatedAt = parseTime(createdAt)
		r.Item.UpdatedAt = parseTime(updatedAt)
		r.Item.ComputeRef()
		r.Item.Content = "" // Don't include full content in search results
		results = append(results, r)
	}
	return results, rows.Err()
}

// --- Item Links ---

func (s *Store) CreateItemLink(workspaceID string, input models.ItemLinkCreate, sourceID string) (*models.ItemLink, error) {
	id := newID()
	ts := now()

	linkType, err := models.NormalizeItemLinkType(input.LinkType)
	if err != nil {
		return nil, err
	}
	if sourceID == input.TargetID {
		return nil, fmt.Errorf("cannot link an item to itself")
	}
	createdBy := input.CreatedBy
	if createdBy == "" {
		createdBy = "user"
	}

	_, err = s.db.Exec(s.q(`
		INSERT INTO item_links (id, workspace_id, source_id, target_id, link_type, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`), id, workspaceID, sourceID, input.TargetID, linkType, createdBy, ts)
	if err != nil {
		return nil, fmt.Errorf("create item link: %w", err)
	}

	return s.getItemLink(id)
}

// getItemLink is the unfiltered post-insert readback used by CreateItemLink to
// hydrate the freshly-inserted row with collection/source/target metadata. It
// intentionally does NOT filter on items.deleted_at IS NULL: the only caller
// is the immediate readback after INSERT, and a delete race against either
// endpoint would otherwise cause the just-successful insert to return nil
// (Codex review on PR #259). User-facing surfaces all read links via
// GetItemLinks (plural) or GetParentForItem, both of which DO filter.
func (s *Store) getItemLink(id string) (*models.ItemLink, error) {
	var link models.ItemLink
	var createdAt string

	var sourcePrefix, targetPrefix string
	var sourceItemNumber, targetItemNumber sql.NullInt64
	var sourceStatus, targetStatus sql.NullString

	srcStatus := s.dialect.JSONExtractText("s.fields", "status")
	tgtStatus := s.dialect.JSONExtractText("t.fields", "status")
	err := s.db.QueryRow(s.q(fmt.Sprintf(`
		SELECT l.id, l.workspace_id, l.source_id, l.target_id, l.link_type, l.created_by, l.created_at,
		       s.title, t.title, s.slug, t.slug, sc.slug, tc.slug, sc.prefix, tc.prefix,
		       s.item_number, t.item_number,
		       %s, %s
		FROM item_links l
		JOIN items s ON s.id = l.source_id
		JOIN items t ON t.id = l.target_id
		JOIN collections sc ON sc.id = s.collection_id
		JOIN collections tc ON tc.id = t.collection_id
		WHERE l.id = ?
	`, srcStatus, tgtStatus)), id).Scan(
		&link.ID, &link.WorkspaceID, &link.SourceID, &link.TargetID,
		&link.LinkType, &link.CreatedBy, &createdAt,
		&link.SourceTitle, &link.TargetTitle,
		&link.SourceSlug, &link.TargetSlug,
		&link.SourceCollectionSlug, &link.TargetCollectionSlug,
		&sourcePrefix, &targetPrefix,
		&sourceItemNumber, &targetItemNumber,
		&sourceStatus, &targetStatus,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item link: %w", err)
	}
	link.CreatedAt = parseTime(createdAt)
	if sourceItemNumber.Valid && sourcePrefix != "" {
		link.SourceRef = fmt.Sprintf("%s-%d", sourcePrefix, sourceItemNumber.Int64)
	}
	if targetItemNumber.Valid && targetPrefix != "" {
		link.TargetRef = fmt.Sprintf("%s-%d", targetPrefix, targetItemNumber.Int64)
	}
	if sourceStatus.Valid {
		link.SourceStatus = sourceStatus.String
	}
	if targetStatus.Valid {
		link.TargetStatus = targetStatus.String
	}
	return &link, nil
}

// GetItemLinks returns links where the given item is either source or target.
// Links pointing to or from soft-deleted items are filtered out so callers (e.g.
// `pad item related`, the lineage panel, the dashboard enrichment pass) don't
// surface dangling endpoints. The link rows themselves are preserved on disk —
// restoring a soft-deleted item resurrects its relationships automatically. See
// BUG-734.
func (s *Store) GetItemLinks(itemID string) ([]models.ItemLink, error) {
	srcStatusExpr := s.dialect.JSONExtractText("s.fields", "status")
	tgtStatusExpr := s.dialect.JSONExtractText("t.fields", "status")
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT l.id, l.workspace_id, l.source_id, l.target_id, l.link_type, l.created_by, l.created_at,
		       s.title, t.title, s.slug, t.slug, sc.slug, tc.slug, sc.prefix, tc.prefix,
		       s.item_number, t.item_number,
		       %s, %s
		FROM item_links l
		JOIN items s ON s.id = l.source_id AND s.deleted_at IS NULL
		JOIN items t ON t.id = l.target_id AND t.deleted_at IS NULL
		JOIN collections sc ON sc.id = s.collection_id
		JOIN collections tc ON tc.id = t.collection_id
		WHERE l.source_id = ? OR l.target_id = ?
		ORDER BY l.created_at DESC
	`, srcStatusExpr, tgtStatusExpr)), itemID, itemID)
	if err != nil {
		return nil, fmt.Errorf("get item links: %w", err)
	}
	defer rows.Close()

	var links []models.ItemLink
	for rows.Next() {
		var link models.ItemLink
		var createdAt string
		var sourcePrefix, targetPrefix string
		var sourceItemNumber, targetItemNumber sql.NullInt64
		var sourceStatus, targetStatus sql.NullString
		if err := rows.Scan(
			&link.ID, &link.WorkspaceID, &link.SourceID, &link.TargetID,
			&link.LinkType, &link.CreatedBy, &createdAt,
			&link.SourceTitle, &link.TargetTitle,
			&link.SourceSlug, &link.TargetSlug,
			&link.SourceCollectionSlug, &link.TargetCollectionSlug,
			&sourcePrefix, &targetPrefix,
			&sourceItemNumber, &targetItemNumber,
			&sourceStatus, &targetStatus,
		); err != nil {
			return nil, err
		}
		link.CreatedAt = parseTime(createdAt)
		if sourceItemNumber.Valid && sourcePrefix != "" {
			link.SourceRef = fmt.Sprintf("%s-%d", sourcePrefix, sourceItemNumber.Int64)
		}
		if targetItemNumber.Valid && targetPrefix != "" {
			link.TargetRef = fmt.Sprintf("%s-%d", targetPrefix, targetItemNumber.Int64)
		}
		if sourceStatus.Valid {
			link.SourceStatus = sourceStatus.String
		}
		if targetStatus.Valid {
			link.TargetStatus = targetStatus.String
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// GetItemLinkByID returns a single item link by its ID, or nil if not found.
func (s *Store) GetItemLinkByID(id string) (*models.ItemLink, error) {
	var link models.ItemLink
	var createdAt string
	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, source_id, target_id, link_type, created_by, created_at
		FROM item_links WHERE id = ?
	`), id).Scan(&link.ID, &link.WorkspaceID, &link.SourceID, &link.TargetID,
		&link.LinkType, &link.CreatedBy, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item link by id: %w", err)
	}
	link.CreatedAt = parseTime(createdAt)
	return &link, nil
}

func (s *Store) DeleteItemLink(id string) error {
	result, err := s.db.Exec(s.q("DELETE FROM item_links WHERE id = ?"), id)
	if err != nil {
		return fmt.Errorf("delete item link: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// --- Phase Links ---

// SetParentLink sets the parent for an item. Since an item can belong to at most
// one parent, this deletes any existing parent link for the item first.
// Includes cycle detection to prevent A→B→A or deeper ancestor loops.
func (s *Store) SetParentLink(workspaceID, itemID, parentID, createdBy string) (*models.ItemLink, error) {
	// Cycle detection: walk the ancestor chain from parentID to ensure itemID is not an ancestor.
	if err := s.checkParentCycle(itemID, parentID); err != nil {
		return nil, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Delete existing parent link for this item (if any)
	if _, err := tx.Exec(s.q(`DELETE FROM item_links WHERE source_id = ? AND link_type = 'parent'`), itemID); err != nil {
		return nil, fmt.Errorf("delete existing parent link: %w", err)
	}

	// Insert new parent link
	id := newID()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(s.q(`
		INSERT INTO item_links (id, workspace_id, source_id, target_id, link_type, created_by, created_at)
		VALUES (?, ?, ?, ?, 'parent', ?, ?)
	`), id, workspaceID, itemID, parentID, createdBy, now); err != nil {
		return nil, fmt.Errorf("insert parent link: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit parent link: %w", err)
	}

	// Return the full link with enriched fields. Use the unfiltered readback
	// helper so that a delete race against either endpoint between commit and
	// readback doesn't cause the successful insert to surface as nil.
	return s.getItemLink(id)
}

// checkParentCycle walks the ancestor chain from parentID and returns an error
// if itemID is found (which would create a cycle).
func (s *Store) checkParentCycle(itemID, parentID string) error {
	visited := map[string]bool{itemID: true}
	current := parentID
	for {
		if visited[current] {
			return fmt.Errorf("cannot set parent: would create a cycle")
		}
		visited[current] = true

		// Look up the parent of current
		var targetID sql.NullString
		err := s.db.QueryRow(s.q(`
			SELECT target_id FROM item_links
			WHERE source_id = ? AND link_type = 'parent'
		`), current).Scan(&targetID)
		if err != nil || !targetID.Valid {
			break // no parent — no cycle
		}
		current = targetID.String
	}
	return nil
}

// ClearParentLink removes the parent link for an item.
func (s *Store) ClearParentLink(itemID string) error {
	_, err := s.db.Exec(s.q(`DELETE FROM item_links WHERE source_id = ? AND link_type = 'parent'`), itemID)
	if err != nil {
		return fmt.Errorf("clear parent link: %w", err)
	}
	return nil
}

// GetParentForItem returns the parent link for an item, or nil if it has no parent.
// A parent link pointing to a soft-deleted item is treated as no parent — the
// breadcrumb / lineage UI shouldn't show a deleted ancestor. See BUG-734.
func (s *Store) GetParentForItem(itemID string) (*models.ItemLink, error) {
	sStatusExpr := s.dialect.JSONExtractText("s.fields", "status")
	tStatusExpr := s.dialect.JSONExtractText("t.fields", "status")
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT l.id, l.workspace_id, l.source_id, l.target_id, l.link_type, l.created_by, l.created_at,
		       s.title, t.title, s.slug, t.slug, sc.slug, tc.slug, sc.prefix, tc.prefix,
		       s.item_number, t.item_number,
		       %s, %s
		FROM item_links l
		JOIN items s ON s.id = l.source_id AND s.deleted_at IS NULL
		JOIN items t ON t.id = l.target_id AND t.deleted_at IS NULL
		JOIN collections sc ON sc.id = s.collection_id
		JOIN collections tc ON tc.id = t.collection_id
		WHERE l.source_id = ? AND l.link_type IN (%s)
	`, sStatusExpr, tStatusExpr, childLinkTypeSQL())), itemID)
	if err != nil {
		return nil, fmt.Errorf("get parent for item: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, nil
	}

	var link models.ItemLink
	var createdAt string
	var sourcePrefix, targetPrefix string
	var sourceItemNumber, targetItemNumber sql.NullInt64
	var sourceStatus, targetStatus sql.NullString
	if err := rows.Scan(
		&link.ID, &link.WorkspaceID, &link.SourceID, &link.TargetID,
		&link.LinkType, &link.CreatedBy, &createdAt,
		&link.SourceTitle, &link.TargetTitle,
		&link.SourceSlug, &link.TargetSlug,
		&link.SourceCollectionSlug, &link.TargetCollectionSlug,
		&sourcePrefix, &targetPrefix,
		&sourceItemNumber, &targetItemNumber,
		&sourceStatus, &targetStatus,
	); err != nil {
		return nil, fmt.Errorf("scan parent link: %w", err)
	}
	link.CreatedAt = parseTime(createdAt)
	if sourceItemNumber.Valid && sourcePrefix != "" {
		link.SourceRef = fmt.Sprintf("%s-%d", sourcePrefix, sourceItemNumber.Int64)
	}
	if targetItemNumber.Valid && targetPrefix != "" {
		link.TargetRef = fmt.Sprintf("%s-%d", targetPrefix, targetItemNumber.Int64)
	}
	if sourceStatus.Valid {
		link.SourceStatus = sourceStatus.String
	}
	if targetStatus.Valid {
		link.TargetStatus = targetStatus.String
	}
	return &link, nil
}

// GetParentMap returns a map of item ID -> parent item ID for all parent links
// in a workspace. Used for efficient batch lookups (e.g., dashboard, list enrichment).
//
// Links whose source or target item is soft-deleted are excluded so that
// dashboard orphan-detection (handlers_dashboard.go) and similar enrichment
// passes don't treat a task whose parent has been archived as still parented.
// See BUG-734.
func (s *Store) GetParentMap(workspaceID string) (map[string]string, error) {
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT il.source_id, il.target_id FROM item_links il
		JOIN items s ON s.id = il.source_id AND s.deleted_at IS NULL
		JOIN items t ON t.id = il.target_id AND t.deleted_at IS NULL
		WHERE il.workspace_id = ? AND il.link_type IN (%s)
	`, childLinkTypeSQL())), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("get parent map: %w", err)
	}
	defer rows.Close()

	m := make(map[string]string)
	for rows.Next() {
		var sourceID, targetID string
		if err := rows.Scan(&sourceID, &targetID); err != nil {
			return nil, err
		}
		m[sourceID] = targetID
	}
	return m, rows.Err()
}

// --- Child Item Progress ---

// GetItemProgress counts total and done child items linked to a parent via item_links.
// "Done" means the child item's done field (resolved from its collection's
// board_group_by, defaulting to status) matches one of that field's terminal
// options. Children from any collection count toward progress, and each
// child is evaluated against its own collection's done rules.
func (s *Store) GetItemProgress(parentItemID string) (total int, done int, err error) {
	filters := s.childrenDoneFiltersForParent(parentItemID)
	doneExpr, doneArgs := s.buildChildrenDoneExpr(filters, "i")
	args := append(doneArgs, parentItemID)
	err = s.db.QueryRow(s.q(fmt.Sprintf(`
		SELECT COUNT(*),
		       COUNT(CASE WHEN %s THEN 1 END)
		FROM items i
		JOIN item_links il ON il.source_id = i.id AND il.link_type IN (%s) AND il.target_id = ?
		WHERE i.deleted_at IS NULL
	`, doneExpr, childLinkTypeSQL())), args...).Scan(&total, &done)
	if err != nil {
		return 0, 0, fmt.Errorf("get item progress: %w", err)
	}
	return total, done, nil
}

// collectionDoneFilter describes how to evaluate "done" for a single child
// collection: which JSON key to read, and which values count as terminal.
type collectionDoneFilter struct {
	collectionID string
	doneKey      string
	values       []string
}

// childrenDoneFiltersForParent returns a filter per distinct child-item
// collection under the given parent. Each filter carries the child
// collection's resolved done field (honoring board_group_by) and terminal
// values so the caller can build a per-collection OR clause that evaluates
// each child against its own done rules.
//
// Soft-deleted collections are intentionally INCLUDED: progress-counting
// callers count items regardless of their collection's deleted_at, so
// excluding the collection here would leave those items without a
// matching per-collection clause and cause them to always evaluate as
// non-terminal. The collection row still carries valid schema + settings
// until a hard delete cascades, so the done rules remain applicable.
func (s *Store) childrenDoneFiltersForParent(parentItemID string) []collectionDoneFilter {
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT DISTINCT c.id, c.schema, c.settings
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		JOIN item_links il ON il.source_id = i.id AND il.link_type IN (%s) AND il.target_id = ?
		WHERE i.deleted_at IS NULL
	`, childLinkTypeSQL())), parentItemID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanCollectionDoneFilters(rows)
}

// doneFiltersForWorkspace returns a done-filter per collection in the
// workspace. Used by cross-collection queries (e.g. agent-role
// breakdowns) that need to evaluate "is done?" for every item regardless
// of which collection it belongs to.
//
// Includes soft-deleted collections: callers (e.g. GetRoleBreakdown)
// count items in the workspace without filtering by collection
// deleted_at, so excluding soft-deleted collections here would leave
// their items without a matching per-collection clause and cause them
// to always register as non-terminal.
func (s *Store) doneFiltersForWorkspace(workspaceID string) []collectionDoneFilter {
	rows, err := s.db.Query(
		s.q(`SELECT id, schema, settings FROM collections WHERE workspace_id = ?`),
		workspaceID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanCollectionDoneFilters(rows)
}

// childrenDoneFiltersForCollection is the batch version: it gathers one
// filter per distinct child-item collection across all parent→child links
// for parents in a given (workspace, collectionSlug).
//
// Includes soft-deleted child collections for the same reason as
// childrenDoneFiltersForParent — callers count items regardless of their
// collection's deleted_at, and we want items from soft-deleted
// collections to still be evaluated against their own done rules.
func (s *Store) childrenDoneFiltersForCollection(workspaceID, collectionSlug string) []collectionDoneFilter {
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT DISTINCT c.id, c.schema, c.settings
		FROM items t
		JOIN collections c ON c.id = t.collection_id
		JOIN item_links il ON il.source_id = t.id AND il.link_type IN (%s)
		JOIN items p ON p.id = il.target_id AND p.deleted_at IS NULL
		JOIN collections pc ON pc.id = p.collection_id AND pc.slug = ?
		WHERE p.workspace_id = ?
		  AND t.deleted_at IS NULL
	`, childLinkTypeSQL())), collectionSlug, workspaceID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanCollectionDoneFilters(rows)
}

// scanCollectionDoneFilters consumes rows yielding (id, schema, settings)
// and resolves each into a collectionDoneFilter.
//
// When a collection's schema fails to parse we still emit a filter — one
// that falls back to the `status` field and the global default terminal
// list. Silently skipping the collection would leave its items without a
// matching per-collection clause in buildChildrenDoneExpr, so they'd
// always register as non-terminal in progress / role / starred queries
// (a malformed schema on one collection would skew counts on every
// parent-progress computation).
func scanCollectionDoneFilters(rows *sql.Rows) []collectionDoneFilter {
	var filters []collectionDoneFilter
	for rows.Next() {
		var id, schemaJSON string
		var settingsJSON sql.NullString
		if err := rows.Scan(&id, &schemaJSON, &settingsJSON); err != nil {
			continue
		}
		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil {
			// Malformed schema → emit a default-fallback filter so the
			// collection's items still get evaluated against the status
			// column + global default terminals. This matches pre-TASK-604
			// behavior for those items.
			filters = append(filters, collectionDoneFilter{
				collectionID: id,
				doneKey:      "status",
				values:       models.DefaultTerminalStatuses,
			})
			continue
		}
		var settings models.CollectionSettings
		if settingsJSON.Valid && settingsJSON.String != "" {
			_ = json.Unmarshal([]byte(settingsJSON.String), &settings)
		}
		key, values := models.TerminalValuesForDoneField(schema, settings)
		filters = append(filters, collectionDoneFilter{
			collectionID: id,
			doneKey:      key,
			values:       values,
		})
	}
	return filters
}

// buildChildrenDoneExpr compiles a set of per-collection done filters into
// a single SQL boolean expression plus ordered args. `itemAlias` is the
// item-table alias in the outer query (e.g. "i" for GetItemProgress, "t"
// for GetAllItemProgress).
//
// Expression shape:
//
//	((<alias>.collection_id = ? AND LOWER(COALESCE(<field_A>, '')) IN (?,?)) OR
//	 (<alias>.collection_id = ? AND LOWER(COALESCE(<field_B>, '')) IN (?,?)))
//
// The `<field_X>` JSON extract uses scalar text extraction; this works
// because DoneFieldKey in the models package only resolves done fields to
// `select` typed columns (see that function's doc). multi_select-backed
// done fields would store their values as a JSON array and scalar IN
// matching would silently miss them — hence the upstream restriction.
//
// If no filters were constructed (no child collections discovered, or all
// of their schemas failed to parse), falls back to checking <alias>.status
// against the global default terminal list — mirroring the legacy behavior
// so dashboards for untyped collections keep working.
func (s *Store) buildChildrenDoneExpr(filters []collectionDoneFilter, itemAlias string) (string, []any) {
	if len(filters) == 0 {
		statusExpr := s.dialect.JSONExtractText(itemAlias+".fields", "status")
		placeholders, args := models.DefaultTerminalStatusPlaceholders()
		return fmt.Sprintf("LOWER(COALESCE(%s, '')) IN (%s)", statusExpr, placeholders), args
	}
	clauses := make([]string, 0, len(filters))
	args := make([]any, 0, len(filters)*4)
	for _, f := range filters {
		fieldExpr := s.dialect.JSONExtractText(itemAlias+".fields", f.doneKey)
		placeholders := make([]string, len(f.values))
		args = append(args, f.collectionID)
		for i, v := range f.values {
			placeholders[i] = "?"
			args = append(args, strings.ToLower(v))
		}
		clauses = append(clauses, fmt.Sprintf(
			"(%s.collection_id = ? AND LOWER(COALESCE(%s, '')) IN (%s))",
			itemAlias, fieldExpr, strings.Join(placeholders, ","),
		))
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args
}

// ItemProgress holds child item completion counts for a single parent item.
type ItemProgress struct {
	ItemID string `json:"item_id"`
	Total  int    `json:"total"`
	Done   int    `json:"done"`
}

// GetAllItemProgress returns child item completion counts for every non-deleted
// item in the given collection within a workspace.
func (s *Store) GetAllItemProgress(workspaceID, collectionSlug string) ([]ItemProgress, error) {
	filters := s.childrenDoneFiltersForCollection(workspaceID, collectionSlug)
	doneExpr, doneArgs := s.buildChildrenDoneExpr(filters, "t")
	args := append(doneArgs, workspaceID, collectionSlug)
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT p.id,
		       COUNT(t.id),
		       COUNT(CASE WHEN t.id IS NOT NULL AND %s THEN 1 END)
		FROM items p
		JOIN collections pc ON pc.id = p.collection_id
		LEFT JOIN item_links il ON il.link_type IN (%s) AND il.target_id = p.id
		LEFT JOIN items t ON t.id = il.source_id
		                  AND t.deleted_at IS NULL
		WHERE p.workspace_id = ?
		  AND pc.slug = ?
		  AND p.deleted_at IS NULL
		GROUP BY p.id
	`, doneExpr, childLinkTypeSQL())), args...)
	if err != nil {
		return nil, fmt.Errorf("get all item progress: %w", err)
	}
	defer rows.Close()

	var result []ItemProgress
	for rows.Next() {
		var ip ItemProgress
		if err := rows.Scan(&ip.ItemID, &ip.Total, &ip.Done); err != nil {
			return nil, fmt.Errorf("scan item progress: %w", err)
		}
		result = append(result, ip)
	}
	if result == nil {
		result = []ItemProgress{}
	}
	return result, rows.Err()
}

// GetChildItems returns all non-deleted child items linked to the given parent
// via item_links. Returns children from any collection.
func (s *Store) GetChildItems(parentItemID string) ([]models.Item, error) {
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT DISTINCT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.created_at, i.updated_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		JOIN item_links il ON il.source_id = i.id AND il.link_type IN (%s) AND il.target_id = ?
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE i.deleted_at IS NULL
		ORDER BY i.sort_order ASC, i.created_at ASC
	`, childLinkTypeSQL())), parentItemID)
	if err != nil {
		return nil, fmt.Errorf("get child items: %w", err)
	}
	defer rows.Close()

	return scanItems(rows)
}

// PopulateHasChildren sets HasChildren=true on items that have at least one
// child linked via parent link_type. Operates in-place on the slice.
func (s *Store) PopulateHasChildren(items []models.Item) {
	if len(items) == 0 {
		return
	}

	// Build ID list and index
	ids := make([]string, len(items))
	idx := make(map[string]int, len(items))
	for i, item := range items {
		ids[i] = item.ID
		idx[item.ID] = i
	}

	// Batch query: which of these IDs are targets of a parent link?
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT DISTINCT il.target_id FROM item_links il
		JOIN items child ON child.id = il.source_id AND child.deleted_at IS NULL
		WHERE il.link_type IN (%s) AND il.target_id IN (%s)
	`, childLinkTypeSQL(), strings.Join(placeholders, ","))

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return // best-effort; don't fail the whole request
	}
	defer rows.Close()

	for rows.Next() {
		var targetID string
		if err := rows.Scan(&targetID); err != nil {
			continue
		}
		if i, ok := idx[targetID]; ok {
			items[i].HasChildren = true
		}
	}
}

// MoveItem moves an item to a different collection within the same workspace.
// It updates the collection_id and fields JSON. The item_number is preserved
// because numbering is workspace-global — the number stays the same, only the
// collection prefix changes (e.g. IDEA-42 → BUG-42).
func (s *Store) MoveItem(itemID, targetCollectionID, newFieldsJSON string) (*models.Item, error) {
	_, err := s.db.Exec(s.q(`
		UPDATE items
		SET collection_id = ?, fields = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`),
		targetCollectionID, newFieldsJSON, time.Now().UTC().Format(time.RFC3339), itemID)
	if err != nil {
		return nil, fmt.Errorf("move item: %w", err)
	}

	return s.GetItem(itemID)
}

// --- Helpers ---

// validSortField matches safe field names (alphanumeric + underscore, starting with a letter).
var validSortField = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

func buildItemSort(sort string, dialect Dialect) string {
	if sort == "" {
		return " ORDER BY i.pinned DESC, i.updated_at DESC"
	}

	var parts []string
	for _, seg := range strings.Split(sort, ",") {
		seg = strings.TrimSpace(seg)
		tokens := strings.SplitN(seg, ":", 2)
		col := tokens[0]
		dir := "ASC"
		if len(tokens) == 2 && strings.ToUpper(tokens[1]) == "DESC" {
			dir = "DESC"
		}

		switch col {
		case "title":
			parts = append(parts, fmt.Sprintf("i.title %s", dir))
		case "created_at":
			parts = append(parts, fmt.Sprintf("i.created_at %s", dir))
		case "updated_at":
			parts = append(parts, fmt.Sprintf("i.updated_at %s", dir))
		case "sort_order":
			parts = append(parts, fmt.Sprintf("i.sort_order %s", dir))
		default:
			// For field-based sorting, use dialect JSON extract — validate the field name
			// to prevent SQL injection via crafted sort parameters.
			if !validSortField.MatchString(col) {
				continue // skip invalid field names
			}
			parts = append(parts, fmt.Sprintf("%s %s", dialect.JSONExtractText("i.fields", col), dir))
		}
	}

	if len(parts) == 0 {
		return " ORDER BY i.pinned DESC, i.updated_at DESC"
	}
	return " ORDER BY " + strings.Join(parts, ", ")
}

// shouldCreateItemVersion mirrors ShouldCreateVersion but queries item_versions.
func (s *Store) shouldCreateItemVersion(itemID, actor, source string) (bool, error) {
	var createdBy, src, createdAt string
	err := s.db.QueryRow(s.q(`
		SELECT created_by, source, created_at
		FROM item_versions
		WHERE item_id = ?
		ORDER BY created_at DESC
		LIMIT 1
	`), itemID).Scan(&createdBy, &src, &createdAt)
	if err == sql.ErrNoRows {
		return true, nil // No versions yet
	}
	if err != nil {
		return false, err
	}

	// Actor or source changed — always snapshot
	if createdBy != actor || src != source {
		return true, nil
	}

	// Throttle
	lastTime := parseTime(createdAt)
	return time.Since(lastTime) >= VersionThrottleInterval, nil
}

// ListItemVersionsResolved returns versions with full content (diffs resolved).
// Requires the current item content to reconstruct diff-based versions.
func (s *Store) ListItemVersionsResolved(itemID, currentContent string) ([]models.Version, error) {
	versions, err := s.ListItemVersions(itemID)
	if err != nil {
		return nil, err
	}

	// Resolve diffs: walk from newest to oldest, applying reverse patches.
	content := currentContent
	for i := range versions {
		if !versions[i].IsDiff {
			content = versions[i].Content
			continue
		}
		resolved, applyErr := diff.ApplyPatch(content, versions[i].Content)
		if applyErr != nil {
			versions[i].Content = fmt.Sprintf("[patch error: %v]", applyErr)
			versions[i].IsDiff = false
			continue
		}
		versions[i].Content = resolved
		versions[i].IsDiff = false
		content = resolved
	}
	return versions, nil
}

// ListItemVersionsBeforeTime returns versions for an item created before the given time,
// ordered newest-first, limited to `limit` results. Used for cursor-based timeline pagination.
func (s *Store) ListItemVersionsBeforeTime(itemID string, before time.Time, beforeID string, limit int) ([]models.Version, error) {
	ts := before.Format(time.RFC3339)
	rows, err := s.db.Query(s.q(`
		SELECT id, item_id, content, change_summary, created_by, source, is_diff, created_at
		FROM item_versions
		WHERE item_id = ? AND (created_at < ? OR (created_at = ? AND id < ?))
		ORDER BY created_at DESC, id DESC
		LIMIT ?
	`), itemID, ts, ts, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var versions []models.Version
	for rows.Next() {
		var v models.Version
		var createdAt string
		var isDiff bool
		if err := rows.Scan(&v.ID, &v.DocumentID, &v.Content, &v.ChangeSummary, &v.CreatedBy, &v.Source, &isDiff, &createdAt); err != nil {
			return nil, err
		}
		v.IsDiff = isDiff
		v.CreatedAt = parseTime(createdAt)
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

// ListItemVersions returns all versions for an item.
func (s *Store) ListItemVersions(itemID string) ([]models.Version, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id, item_id, content, change_summary, created_by, source, is_diff, created_at
		FROM item_versions
		WHERE item_id = ?
		ORDER BY created_at DESC
	`), itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var versions []models.Version
	for rows.Next() {
		var v models.Version
		var createdAt string
		var isDiff bool
		if err := rows.Scan(&v.ID, &v.DocumentID, &v.Content, &v.ChangeSummary, &v.CreatedBy, &v.Source, &isDiff, &createdAt); err != nil {
			return nil, err
		}
		v.IsDiff = isDiff
		v.CreatedAt = parseTime(createdAt)
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

func scanItems(rows *sql.Rows) ([]models.Item, error) {
	var items []models.Item
	for rows.Next() {
		var item models.Item
		var createdAt, updatedAt string
		var pinned bool
		if err := rows.Scan(
			&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
			&item.Content, &item.Fields, &item.Tags,
			&pinned, &item.SortOrder, &item.ParentID, &item.AssignedUserID, &item.AgentRoleID, &item.RoleSortOrder,
			&item.CreatedBy, &item.LastModifiedBy, &item.Source,
			&item.ItemNumber, &createdAt, &updatedAt,
			&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
			&item.AssignedUserName, &item.AssignedUserEmail,
			&item.AgentRoleName, &item.AgentRoleSlug, &item.AgentRoleIcon,
		); err != nil {
			return nil, err
		}
		item.Pinned = pinned
		item.CreatedAt = parseTime(createdAt)
		item.UpdatedAt = parseTime(updatedAt)
		hydrateItemComputedMetadata(&item)
		items = append(items, item)
	}
	return items, rows.Err()
}

// ItemsModifiedSince returns items in a workspace that were updated after the
// given timestamp. Used for incremental sync on tab resume. Also returns IDs of
// items that were deleted (hard-deleted or archived) since the timestamp.
//
// The updated list includes both active AND recently archived items (those with
// deleted_at > since). This lets the frontend update archived views correctly —
// an item that was just archived needs its full data to appear in archived views,
// not just its ID in the deleted list.
func (s *Store) ItemsModifiedSince(workspaceID string, since time.Time) (updated []models.Item, deletedIDs []string, err error) {
	sinceStr := since.UTC().Format(time.RFC3339)

	// Fetch updated items: active items modified since the timestamp,
	// PLUS items archived since the timestamp (so archived views can update).
	query := s.q(`
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.created_at, i.updated_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE i.workspace_id = ?
		  AND i.updated_at > ?
		  AND (i.deleted_at IS NULL OR i.deleted_at > ?)
		ORDER BY i.updated_at ASC
	`)

	rows, err := s.db.Query(query, workspaceID, sinceStr, sinceStr)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	updated, err = scanItems(rows)
	if err != nil {
		return nil, nil, err
	}

	// Fetch IDs of items deleted since the timestamp.
	delQuery := s.q(`
		SELECT id FROM items
		WHERE workspace_id = ?
		  AND deleted_at IS NOT NULL
		  AND deleted_at > ?
	`)
	delRows, err := s.db.Query(delQuery, workspaceID, sinceStr)
	if err != nil {
		return updated, nil, err
	}
	defer delRows.Close()
	for delRows.Next() {
		var id string
		if err := delRows.Scan(&id); err != nil {
			return updated, nil, err
		}
		deletedIDs = append(deletedIDs, id)
	}

	return updated, deletedIDs, delRows.Err()
}

// ItemCollectionRef is a minimal item reference with collection ID, used for
// filtering deleted items by collection visibility.
type ItemCollectionRef struct {
	ID           string
	CollectionID string
}

// GetDeletedItemsWithCollection returns minimal item info (ID + CollectionID)
// for soft-deleted items, used to filter deleted item IDs by collection visibility.
func (s *Store) GetDeletedItemsWithCollection(workspaceID string, itemIDs []string) ([]ItemCollectionRef, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(itemIDs))
	args := []interface{}{workspaceID}
	for i, id := range itemIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT id, collection_id FROM items
		WHERE workspace_id = ? AND id IN (%s)
	`, strings.Join(placeholders, ","))), args...)
	if err != nil {
		return nil, fmt.Errorf("get deleted items with collection: %w", err)
	}
	defer rows.Close()
	var results []ItemCollectionRef
	for rows.Next() {
		var r ItemCollectionRef
		if err := rows.Scan(&r.ID, &r.CollectionID); err != nil {
			return nil, fmt.Errorf("scan deleted item: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

func hydrateItemComputedMetadata(item *models.Item) {
	if item == nil {
		return
	}
	item.ComputeRef()
	item.CodeContext = models.ExtractItemCodeContext(item.Fields)
	item.Convention = models.ExtractItemConventionMetadata(item.Fields)
	item.ImplementationNotes = models.ExtractItemImplementationNotes(item.Fields)
	item.DecisionLog = models.ExtractItemDecisionLog(item.Fields)
}
