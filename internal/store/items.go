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

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Assign the next item_number within this collection
	var nextNum int
	err = tx.QueryRow(s.q("SELECT COALESCE(MAX(item_number), 0) + 1 FROM items WHERE collection_id = ?"), collectionID).Scan(&nextNum)
	if err != nil {
		return nil, fmt.Errorf("get next item number: %w", err)
	}

	_, err = tx.Exec(s.q(`
		INSERT INTO items (id, workspace_id, collection_id, title, slug, content, fields, tags,
		                   pinned, sort_order, parent_id, assigned_user_id, agent_role_id, role_sort_order,
		                   created_by, last_modified_by, source, item_number, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?)
	`), id, workspaceID, collectionID, input.Title, slug, input.Content, fields, tags,
		s.dialect.BoolToInt(input.Pinned), input.ParentID, input.AssignedUserID, input.AgentRoleID,
		createdBy, createdBy, source, nextNum, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert item: %w", err)
	}

	// Create initial version if there's content
	if input.Content != "" {
		vid := newID()
		_, err = tx.Exec(s.q(`
			INSERT INTO item_versions (id, item_id, content, change_summary, created_by, source, is_diff, created_at)
			VALUES (?, ?, ?, '', ?, ?, ?, ?)
		`), vid, id, input.Content, createdBy, source, s.dialect.BoolToInt(false), ts)
		if err != nil {
			return nil, fmt.Errorf("create initial version: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetItem(id)
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
func (s *Store) GetItemByRef(workspaceID, prefix string, number int) (*models.Item, error) {
	var id string
	err := s.db.QueryRow(s.q(`
		SELECT i.id FROM items i
		JOIN collections c ON c.id = i.collection_id
		WHERE i.workspace_id = ? AND c.prefix = ? AND i.item_number = ? AND i.deleted_at IS NULL
	`), workspaceID, prefix, number).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item by ref: %w", err)
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

	// Parent link filter via item_links
	if params.ParentLinkID != "" {
		query += " AND EXISTS (SELECT 1 FROM item_links il WHERE il.source_id = i.id AND il.link_type = 'parent' AND il.target_id = ?)"
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

	// SQLite bm25(): more negative = more relevant → ASC (default).
	// PostgreSQL ts_rank(): higher = more relevant → DESC.
	if s.dialect.Driver() == DriverPostgres {
		query += " ORDER BY " + ftsRank + " DESC"
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

func (s *Store) GetItemLinks(itemID string) ([]models.ItemLink, error) {
	srcStatusExpr := s.dialect.JSONExtractText("s.fields", "status")
	tgtStatusExpr := s.dialect.JSONExtractText("t.fields", "status")
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT l.id, l.workspace_id, l.source_id, l.target_id, l.link_type, l.created_by, l.created_at,
		       s.title, t.title, s.slug, t.slug, sc.slug, tc.slug, sc.prefix, tc.prefix,
		       s.item_number, t.item_number,
		       %s, %s
		FROM item_links l
		JOIN items s ON s.id = l.source_id
		JOIN items t ON t.id = l.target_id
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

	// Return the full link with enriched fields
	links, err := s.GetItemLinks(itemID)
	if err != nil {
		return nil, err
	}
	for _, link := range links {
		if link.ID == id {
			return &link, nil
		}
	}
	return nil, fmt.Errorf("parent link created but not found")
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
func (s *Store) GetParentForItem(itemID string) (*models.ItemLink, error) {
	sStatusExpr := s.dialect.JSONExtractText("s.fields", "status")
	tStatusExpr := s.dialect.JSONExtractText("t.fields", "status")
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT l.id, l.workspace_id, l.source_id, l.target_id, l.link_type, l.created_by, l.created_at,
		       s.title, t.title, s.slug, t.slug, sc.slug, tc.slug, sc.prefix, tc.prefix,
		       s.item_number, t.item_number,
		       %s, %s
		FROM item_links l
		JOIN items s ON s.id = l.source_id
		JOIN items t ON t.id = l.target_id
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
func (s *Store) GetParentMap(workspaceID string) (map[string]string, error) {
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT source_id, target_id FROM item_links
		WHERE workspace_id = ? AND link_type IN (%s)
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
// "Done" means any terminal status as defined by the child items' collection schemas.
// Children from any collection count toward progress.
func (s *Store) GetItemProgress(parentItemID string) (total int, done int, err error) {
	termPlaceholders, termArgs := s.getChildTerminalPlaceholders(parentItemID)
	args := append(termArgs, parentItemID)
	statusExpr := s.dialect.JSONExtractText("i.fields", "status")
	err = s.db.QueryRow(s.q(fmt.Sprintf(`
		SELECT COUNT(*),
		       COUNT(CASE WHEN LOWER(%s) IN (%s) THEN 1 END)
		FROM items i
		JOIN item_links il ON il.source_id = i.id AND il.link_type IN (%s) AND il.target_id = ?
		WHERE i.deleted_at IS NULL
	`, statusExpr, termPlaceholders, childLinkTypeSQL())), args...).Scan(&total, &done)
	if err != nil {
		return 0, 0, fmt.Errorf("get item progress: %w", err)
	}
	return total, done, nil
}

// getChildTerminalPlaceholders returns SQL placeholders and args for the terminal
// statuses of the collections that a parent item's children belong to.
// It queries the actual child items' collection schemas rather than hardcoding 'tasks'.
func (s *Store) getChildTerminalPlaceholders(parentItemID string) (string, []any) {
	// Find distinct collection IDs of child items
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT DISTINCT c.schema
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		JOIN item_links il ON il.source_id = i.id AND il.link_type IN (%s) AND il.target_id = ?
		WHERE i.deleted_at IS NULL AND c.deleted_at IS NULL
	`, childLinkTypeSQL())), parentItemID)
	if err != nil {
		return models.DefaultTerminalStatusPlaceholders()
	}
	defer rows.Close()

	// Collect terminal statuses from all child collections
	terminalSet := make(map[string]bool)
	found := false
	for rows.Next() {
		var schemaJSON string
		if err := rows.Scan(&schemaJSON); err != nil {
			continue
		}
		found = true
		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil {
			continue
		}
		for _, ts := range models.TerminalStatusesFromSchema(schema) {
			terminalSet[strings.ToLower(ts)] = true
		}
	}

	if !found || len(terminalSet) == 0 {
		return models.DefaultTerminalStatusPlaceholders()
	}

	placeholders := make([]string, 0, len(terminalSet))
	args := make([]any, 0, len(terminalSet))
	for ts := range terminalSet {
		placeholders = append(placeholders, "?")
		args = append(args, ts)
	}
	return strings.Join(placeholders, ","), args
}

// getCollectionChildTerminalPlaceholders returns SQL placeholders and args for the
// terminal statuses of all child collections linked to parents in the given collection.
// This is the batch version of getChildTerminalPlaceholders — instead of querying for
// a single parent, it gathers terminal statuses across all parent→child links in a
// workspace/collection pair.
func (s *Store) getCollectionChildTerminalPlaceholders(workspaceID, collectionSlug string) (string, []any) {
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT DISTINCT c.schema
		FROM items t
		JOIN collections c ON c.id = t.collection_id
		JOIN item_links il ON il.source_id = t.id AND il.link_type IN (%s)
		JOIN items p ON p.id = il.target_id AND p.deleted_at IS NULL
		JOIN collections pc ON pc.id = p.collection_id AND pc.slug = ?
		WHERE p.workspace_id = ?
		  AND t.deleted_at IS NULL
		  AND c.deleted_at IS NULL
	`, childLinkTypeSQL())), collectionSlug, workspaceID)
	if err != nil {
		return models.DefaultTerminalStatusPlaceholders()
	}
	defer rows.Close()

	terminalSet := make(map[string]bool)
	found := false
	for rows.Next() {
		var schemaJSON string
		if err := rows.Scan(&schemaJSON); err != nil {
			continue
		}
		found = true
		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil {
			continue
		}
		for _, ts := range models.TerminalStatusesFromSchema(schema) {
			terminalSet[strings.ToLower(ts)] = true
		}
	}

	if !found || len(terminalSet) == 0 {
		return models.DefaultTerminalStatusPlaceholders()
	}

	placeholders := make([]string, 0, len(terminalSet))
	args := make([]any, 0, len(terminalSet))
	for ts := range terminalSet {
		placeholders = append(placeholders, "?")
		args = append(args, ts)
	}
	return strings.Join(placeholders, ","), args
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
	termPlaceholders, termArgs := s.getCollectionChildTerminalPlaceholders(workspaceID, collectionSlug)
	args := append(termArgs, workspaceID, collectionSlug)
	tStatusExpr := s.dialect.JSONExtractText("t.fields", "status")
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT p.id,
		       COUNT(t.id),
		       COUNT(CASE WHEN LOWER(%s) IN (%s) THEN 1 END)
		FROM items p
		JOIN collections pc ON pc.id = p.collection_id
		LEFT JOIN item_links il ON il.link_type IN (%s) AND il.target_id = p.id
		LEFT JOIN items t ON t.id = il.source_id
		                  AND t.deleted_at IS NULL
		WHERE p.workspace_id = ?
		  AND pc.slug = ?
		  AND p.deleted_at IS NULL
		GROUP BY p.id
	`, tStatusExpr, termPlaceholders, childLinkTypeSQL())), args...)
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
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
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
// It updates the collection_id, assigns a new item_number in the target collection,
// and updates the fields JSON.
func (s *Store) MoveItem(itemID, targetCollectionID, newFieldsJSON string) (*models.Item, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Get next item_number in the target collection
	var nextNumber int
	err = tx.QueryRow(s.q(`SELECT COALESCE(MAX(item_number), 0) + 1 FROM items WHERE collection_id = ?`), targetCollectionID).Scan(&nextNumber)
	if err != nil {
		return nil, fmt.Errorf("get next item number: %w", err)
	}

	// Update the item
	_, err = tx.Exec(s.q(`
		UPDATE items
		SET collection_id = ?, fields = ?, item_number = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`),
		targetCollectionID, newFieldsJSON, nextNumber, time.Now().UTC().Format(time.RFC3339), itemID)
	if err != nil {
		return nil, fmt.Errorf("move item: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
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
