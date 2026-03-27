package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/xarmian/pad/internal/diff"
	"github.com/xarmian/pad/internal/models"
)

// ItemSearchResult holds FTS search results for items.
type ItemSearchResult struct {
	Item    models.Item `json:"item"`
	Snippet string      `json:"snippet"`
	Rank    float64     `json:"rank"`
}

func (s *Store) CreateItem(workspaceID, collectionID string, input models.ItemCreate) (*models.Item, error) {
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
	err = tx.QueryRow("SELECT COALESCE(MAX(item_number), 0) + 1 FROM items WHERE collection_id = ?", collectionID).Scan(&nextNum)
	if err != nil {
		return nil, fmt.Errorf("get next item number: %w", err)
	}

	_, err = tx.Exec(`
		INSERT INTO items (id, workspace_id, collection_id, title, slug, content, fields, tags,
		                   pinned, sort_order, parent_id, created_by, last_modified_by, source, item_number, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?)
	`, id, workspaceID, collectionID, input.Title, slug, input.Content, fields, tags,
		boolToInt(input.Pinned), input.ParentID, createdBy, createdBy, source, nextNum, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert item: %w", err)
	}

	// Create initial version if there's content
	if input.Content != "" {
		vid := newID()
		_, err = tx.Exec(`
			INSERT INTO item_versions (id, item_id, content, change_summary, created_by, source, is_diff, created_at)
			VALUES (?, ?, ?, '', ?, ?, 0, ?)
		`, vid, id, input.Content, createdBy, source, ts)
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
	var pinned int

	err := s.db.QueryRow(`
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.created_at, i.updated_at, i.deleted_at,
		       c.slug, c.name, c.icon, c.prefix
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		WHERE i.id = ? AND i.deleted_at IS NULL
	`, id).Scan(
		&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
		&item.Content, &item.Fields, &item.Tags,
		&pinned, &item.SortOrder, &item.ParentID, &item.CreatedBy, &item.LastModifiedBy, &item.Source,
		&item.ItemNumber, &createdAt, &updatedAt, &deletedAt,
		&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item: %w", err)
	}

	item.Pinned = pinned == 1
	item.CreatedAt = parseTime(createdAt)
	item.UpdatedAt = parseTime(updatedAt)
	item.DeletedAt = parseTimePtr(deletedAt)
	return &item, nil
}

func (s *Store) GetItemBySlug(workspaceID, slug string) (*models.Item, error) {
	var id string
	err := s.db.QueryRow(`
		SELECT id FROM items
		WHERE workspace_id = ? AND slug = ? AND deleted_at IS NULL
	`, workspaceID, slug).Scan(&id)
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
	err := s.db.QueryRow(`
		SELECT i.id FROM items i
		JOIN collections c ON c.id = i.collection_id
		WHERE i.workspace_id = ? AND c.prefix = ? AND i.item_number = ? AND i.deleted_at IS NULL
	`, workspaceID, prefix, number).Scan(&id)
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
		var pinned int

		err := s.db.QueryRow(`
			SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
			       i.pinned, i.sort_order, i.parent_id, i.created_by, i.last_modified_by, i.source,
			       i.item_number, i.created_at, i.updated_at, i.deleted_at,
			       c.slug, c.name, c.icon, c.prefix
			FROM items i
			JOIN collections c ON c.id = i.collection_id
			WHERE i.workspace_id = ? AND c.prefix = ? AND i.item_number = ?
		`, workspaceID, prefix, number).Scan(
			&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
			&item.Content, &item.Fields, &item.Tags,
			&pinned, &item.SortOrder, &item.ParentID, &item.CreatedBy, &item.LastModifiedBy, &item.Source,
			&item.ItemNumber, &createdAt, &updatedAt, &deletedAt,
			&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
		)
		if err == nil {
			item.Pinned = pinned == 1
			item.CreatedAt = parseTime(createdAt)
			item.UpdatedAt = parseTime(updatedAt)
			item.DeletedAt = parseTimePtr(deletedAt)
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
func parseItemRef(s string) (string, int, bool) {
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
	var pinned int

	err := s.db.QueryRow(`
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.created_at, i.updated_at, i.deleted_at,
		       c.slug, c.name, c.icon, c.prefix
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		WHERE i.workspace_id = ? AND i.slug = ?
	`, workspaceID, slug).Scan(
		&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
		&item.Content, &item.Fields, &item.Tags,
		&pinned, &item.SortOrder, &item.ParentID, &item.CreatedBy, &item.LastModifiedBy, &item.Source,
		&item.ItemNumber, &createdAt, &updatedAt, &deletedAt,
		&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item by slug (include deleted): %w", err)
	}

	item.Pinned = pinned == 1
	item.CreatedAt = parseTime(createdAt)
	item.UpdatedAt = parseTime(updatedAt)
	item.DeletedAt = parseTimePtr(deletedAt)
	return &item, nil
}

func (s *Store) ListItems(workspaceID string, params models.ItemListParams) ([]models.Item, error) {
	// When search is specified, use FTS
	if params.Search != "" {
		return s.listItemsFTS(workspaceID, params)
	}

	query := `
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.created_at, i.updated_at,
		       c.slug, c.name, c.icon, c.prefix
		FROM items i
		JOIN collections c ON c.id = i.collection_id
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
		query += " AND i.tags LIKE ?"
		args = append(args, "%\""+params.Tag+"\"%")
	}

	if params.ParentID != "" {
		query += " AND i.parent_id = ?"
		args = append(args, params.ParentID)
	}

	// Field filters using json_extract — supports comma-separated values as OR
	for key, value := range params.Fields {
		if strings.Contains(value, ",") {
			values := strings.Split(value, ",")
			placeholders := make([]string, len(values))
			args = append(args, "$."+key)
			for i, v := range values {
				placeholders[i] = "?"
				args = append(args, strings.TrimSpace(v))
			}
			query += " AND json_extract(i.fields, ?) IN (" + strings.Join(placeholders, ",") + ")"
		} else {
			query += " AND json_extract(i.fields, ?) = ?"
			args = append(args, "$."+key, value)
		}
	}

	// Sorting
	query += buildItemSort(params.Sort)

	// Pagination
	if params.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, params.Limit)
		if params.Offset > 0 {
			query += " OFFSET ?"
			args = append(args, params.Offset)
		}
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list items: %w", err)
	}
	defer rows.Close()

	return scanItems(rows)
}

func (s *Store) listItemsFTS(workspaceID string, params models.ItemListParams) ([]models.Item, error) {
	query := `
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.created_at, i.updated_at,
		       c.slug, c.name, c.icon, c.prefix
		FROM items i
		JOIN items_fts fts ON i.rowid = fts.rowid
		JOIN collections c ON c.id = i.collection_id
		WHERE i.workspace_id = ? AND i.deleted_at IS NULL
		AND items_fts MATCH ?
	`
	args := []interface{}{workspaceID, params.Search}

	if params.CollectionSlug != "" {
		query += " AND c.slug = ?"
		args = append(args, params.CollectionSlug)
	}

	query += " ORDER BY rank"

	if params.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, params.Limit)
	}

	rows, err := s.db.Query(query, args...)
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
			isDiff := 0
			patch := diff.CreateReversePatch(existing.Content, *input.Content)
			if diff.IsDiffSmaller(patch, existing.Content) {
				versionContent = patch
				isDiff = 1
			}

			_, err = tx.Exec(`
				INSERT INTO item_versions (id, item_id, content, change_summary, created_by, source, is_diff, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			`, vid, id, versionContent, input.ChangeSummary, createdBy, source, isDiff, ts)
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
		args = append(args, boolToInt(*input.Pinned))
	}
	if input.SortOrder != nil {
		sets = append(sets, "sort_order = ?")
		args = append(args, *input.SortOrder)
	}
	if input.ParentID != nil {
		sets = append(sets, "parent_id = ?")
		args = append(args, *input.ParentID)
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
	_, err = tx.Exec(query, args...)
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
	result, err := s.db.Exec(`
		UPDATE items SET deleted_at = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL
	`, ts, ts, id)
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
	result, err := s.db.Exec(`
		UPDATE items SET deleted_at = NULL, updated_at = ?
		WHERE id = ? AND deleted_at IS NOT NULL
	`, ts, id)
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
	sqlQuery := `
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.created_at, i.updated_at,
		       c.slug, c.name, c.icon, c.prefix,
		       snippet(items_fts, 1, '<mark>', '</mark>', '...', 32) as snippet,
		       rank
		FROM items_fts fts
		JOIN items i ON i.rowid = fts.rowid
		JOIN collections c ON c.id = i.collection_id
		WHERE items_fts MATCH ?
		AND i.deleted_at IS NULL
	`
	args := []interface{}{query}

	if workspaceID != "" {
		sqlQuery += " AND i.workspace_id = ?"
		args = append(args, workspaceID)
	}

	sqlQuery += " ORDER BY rank LIMIT 50"

	rows, err := s.db.Query(sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("search items: %w", err)
	}
	defer rows.Close()

	var results []ItemSearchResult
	for rows.Next() {
		var r ItemSearchResult
		var createdAt, updatedAt string
		var pinned int
		if err := rows.Scan(
			&r.Item.ID, &r.Item.WorkspaceID, &r.Item.CollectionID, &r.Item.Title, &r.Item.Slug,
			&r.Item.Content, &r.Item.Fields, &r.Item.Tags,
			&pinned, &r.Item.SortOrder, &r.Item.ParentID, &r.Item.CreatedBy, &r.Item.LastModifiedBy,
			&r.Item.Source, &r.Item.ItemNumber, &createdAt, &updatedAt,
			&r.Item.CollectionSlug, &r.Item.CollectionName, &r.Item.CollectionIcon, &r.Item.CollectionPrefix,
			&r.Snippet, &r.Rank,
		); err != nil {
			return nil, err
		}
		r.Item.Pinned = pinned == 1
		r.Item.CreatedAt = parseTime(createdAt)
		r.Item.UpdatedAt = parseTime(updatedAt)
		r.Item.Content = "" // Don't include full content in search results
		results = append(results, r)
	}
	return results, rows.Err()
}

// --- Item Links ---

func (s *Store) CreateItemLink(workspaceID string, input models.ItemLinkCreate, sourceID string) (*models.ItemLink, error) {
	id := newID()
	ts := now()

	linkType := input.LinkType
	if linkType == "" {
		linkType = "related"
	}
	createdBy := input.CreatedBy
	if createdBy == "" {
		createdBy = "user"
	}

	_, err := s.db.Exec(`
		INSERT INTO item_links (id, workspace_id, source_id, target_id, link_type, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, id, workspaceID, sourceID, input.TargetID, linkType, createdBy, ts)
	if err != nil {
		return nil, fmt.Errorf("create item link: %w", err)
	}

	return s.getItemLink(id)
}

func (s *Store) getItemLink(id string) (*models.ItemLink, error) {
	var link models.ItemLink
	var createdAt string

	err := s.db.QueryRow(`
		SELECT l.id, l.workspace_id, l.source_id, l.target_id, l.link_type, l.created_by, l.created_at,
		       s.title, t.title
		FROM item_links l
		JOIN items s ON s.id = l.source_id
		JOIN items t ON t.id = l.target_id
		WHERE l.id = ?
	`, id).Scan(
		&link.ID, &link.WorkspaceID, &link.SourceID, &link.TargetID,
		&link.LinkType, &link.CreatedBy, &createdAt,
		&link.SourceTitle, &link.TargetTitle,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get item link: %w", err)
	}
	link.CreatedAt = parseTime(createdAt)
	return &link, nil
}

func (s *Store) GetItemLinks(itemID string) ([]models.ItemLink, error) {
	rows, err := s.db.Query(`
		SELECT l.id, l.workspace_id, l.source_id, l.target_id, l.link_type, l.created_by, l.created_at,
		       s.title, t.title
		FROM item_links l
		JOIN items s ON s.id = l.source_id
		JOIN items t ON t.id = l.target_id
		WHERE l.source_id = ? OR l.target_id = ?
		ORDER BY l.created_at DESC
	`, itemID, itemID)
	if err != nil {
		return nil, fmt.Errorf("get item links: %w", err)
	}
	defer rows.Close()

	var links []models.ItemLink
	for rows.Next() {
		var link models.ItemLink
		var createdAt string
		if err := rows.Scan(
			&link.ID, &link.WorkspaceID, &link.SourceID, &link.TargetID,
			&link.LinkType, &link.CreatedBy, &createdAt,
			&link.SourceTitle, &link.TargetTitle,
		); err != nil {
			return nil, err
		}
		link.CreatedAt = parseTime(createdAt)
		links = append(links, link)
	}
	return links, rows.Err()
}

func (s *Store) DeleteItemLink(id string) error {
	result, err := s.db.Exec("DELETE FROM item_links WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete item link: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// --- Phase Progress ---

// GetPhaseProgress counts total and done tasks linked to a phase via the
// relation field json_extract(fields, '$.phase') = phaseItemID.
func (s *Store) GetPhaseProgress(phaseItemID string) (total int, done int, err error) {
	err = s.db.QueryRow(`
		SELECT COUNT(*),
		       COUNT(CASE WHEN json_extract(i.fields, '$.status') = 'done' THEN 1 END)
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		WHERE c.slug = 'tasks'
		  AND i.deleted_at IS NULL
		  AND json_extract(i.fields, '$.phase') = ?
	`, phaseItemID).Scan(&total, &done)
	if err != nil {
		return 0, 0, fmt.Errorf("get phase progress: %w", err)
	}
	return total, done, nil
}

// PhaseProgress holds task completion counts for a single phase.
type PhaseProgress struct {
	PhaseID string `json:"phase_id"`
	Total   int    `json:"total"`
	Done    int    `json:"done"`
}

// GetAllPhasesProgress returns task completion counts for every non-deleted phase in a workspace.
func (s *Store) GetAllPhasesProgress(workspaceID string) ([]PhaseProgress, error) {
	rows, err := s.db.Query(`
		SELECT p.id,
		       COUNT(t.id),
		       COUNT(CASE WHEN json_extract(t.fields, '$.status') = 'done' THEN 1 END)
		FROM items p
		JOIN collections pc ON pc.id = p.collection_id AND pc.slug = 'phases'
		LEFT JOIN items t ON json_extract(t.fields, '$.phase') = p.id
		                  AND t.deleted_at IS NULL
		LEFT JOIN collections tc ON tc.id = t.collection_id AND tc.slug = 'tasks'
		WHERE p.workspace_id = ?
		  AND p.deleted_at IS NULL
		GROUP BY p.id
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("get all phases progress: %w", err)
	}
	defer rows.Close()

	var result []PhaseProgress
	for rows.Next() {
		var pp PhaseProgress
		if err := rows.Scan(&pp.PhaseID, &pp.Total, &pp.Done); err != nil {
			return nil, fmt.Errorf("scan phase progress: %w", err)
		}
		result = append(result, pp)
	}
	if result == nil {
		result = []PhaseProgress{}
	}
	return result, rows.Err()
}

// GetTasksForPhase returns all non-deleted tasks whose phase relation field
// points to the given phase item ID.
func (s *Store) GetTasksForPhase(phaseItemID string) ([]models.Item, error) {
	rows, err := s.db.Query(`
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.created_at, i.updated_at,
		       c.slug, c.name, c.icon, c.prefix
		FROM items i
		JOIN collections c ON c.id = i.collection_id
		WHERE c.slug = 'tasks'
		  AND i.deleted_at IS NULL
		  AND json_extract(i.fields, '$.phase') = ?
		ORDER BY i.sort_order ASC, i.created_at ASC
	`, phaseItemID)
	if err != nil {
		return nil, fmt.Errorf("get tasks for phase: %w", err)
	}
	defer rows.Close()

	return scanItems(rows)
}

// --- Helpers ---

func buildItemSort(sort string) string {
	if sort == "" {
		return " ORDER BY i.pinned DESC, i.updated_at DESC"
	}

	var parts []string
	for _, s := range strings.Split(sort, ",") {
		s = strings.TrimSpace(s)
		tokens := strings.SplitN(s, ":", 2)
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
			// For field-based sorting, use json_extract
			parts = append(parts, fmt.Sprintf("json_extract(i.fields, '$.%s') %s", col, dir))
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
	err := s.db.QueryRow(`
		SELECT created_by, source, created_at
		FROM item_versions
		WHERE item_id = ?
		ORDER BY created_at DESC
		LIMIT 1
	`, itemID).Scan(&createdBy, &src, &createdAt)
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

// ListItemVersions returns all versions for an item.
func (s *Store) ListItemVersions(itemID string) ([]models.Version, error) {
	rows, err := s.db.Query(`
		SELECT id, item_id, content, change_summary, created_by, source, is_diff, created_at
		FROM item_versions
		WHERE item_id = ?
		ORDER BY created_at DESC
	`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var versions []models.Version
	for rows.Next() {
		var v models.Version
		var createdAt string
		var isDiff int
		if err := rows.Scan(&v.ID, &v.DocumentID, &v.Content, &v.ChangeSummary, &v.CreatedBy, &v.Source, &isDiff, &createdAt); err != nil {
			return nil, err
		}
		v.IsDiff = isDiff == 1
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
		var pinned int
		if err := rows.Scan(
			&item.ID, &item.WorkspaceID, &item.CollectionID, &item.Title, &item.Slug,
			&item.Content, &item.Fields, &item.Tags,
			&pinned, &item.SortOrder, &item.ParentID, &item.CreatedBy, &item.LastModifiedBy, &item.Source,
			&item.ItemNumber, &createdAt, &updatedAt,
			&item.CollectionSlug, &item.CollectionName, &item.CollectionIcon, &item.CollectionPrefix,
		); err != nil {
			return nil, err
		}
		item.Pinned = pinned == 1
		item.CreatedAt = parseTime(createdAt)
		item.UpdatedAt = parseTime(updatedAt)
		items = append(items, item)
	}
	return items, rows.Err()
}
