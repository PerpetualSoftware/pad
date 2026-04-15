package store

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/xarmian/pad/internal/models"
)

// validFieldKey matches safe JSON field keys: alphanumeric, underscores, hyphens only.
var validFieldKey = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

type SearchResult struct {
	Item    models.Item `json:"item"`
	Snippet string      `json:"snippet"`
	Rank    float64     `json:"rank"`
}

type SearchResponse struct {
	Results []SearchResult `json:"results"`
	Total   int            `json:"total"`
	Limit   int            `json:"limit"`
	Offset  int            `json:"offset"`
}

// placeholders returns a comma-separated string of SQL placeholders: "?, ?, ?"
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	s := strings.Repeat("?, ", n)
	return s[:len(s)-2] // trim trailing ", "
}

type SearchParams struct {
	Query         string
	Workspace     string   // workspace slug, optional — scopes to single workspace
	WorkspaceIDs  []string // workspace IDs to scope results to (used when no specific workspace is given)
	CollectionIDs []string // permission filter: restrict to these collection IDs (nil = no filter)
	ItemIDs       []string // permission filter: additionally allow these specific item IDs (for item-level grants)

	// Content filters (applied on top of permission filters)
	Collection   string            // collection slug — scope search to a single collection
	FieldFilters map[string]string // field key → value filters (e.g. {"status": "open", "priority": "high"})

	// Pagination
	Limit  int // max results per page (default 50, max 200)
	Offset int // skip this many results

	// Sorting: "relevance" (default), "created_at", "updated_at", "title"
	Sort  string // sort field
	Order string // "asc" or "desc" (default depends on sort field)
}

// Normalize sets pagination and sorting defaults on SearchParams.
func (p *SearchParams) Normalize() {
	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.Limit > 200 {
		p.Limit = 200
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	if p.Sort == "" {
		p.Sort = "relevance"
	}
	if p.Order == "" {
		switch p.Sort {
		case "title":
			p.Order = "asc"
		default:
			p.Order = "desc"
		}
	}
	if p.Order != "asc" && p.Order != "desc" {
		p.Order = "desc"
	}
}

func (s *Store) Search(params SearchParams) (*SearchResponse, error) {
	params.Normalize()

	// Non-nil empty CollectionIDs means "no visible collections" — return
	// empty results immediately, unless ItemIDs are also provided (item-level
	// grants may still allow access to specific items even without full
	// collection access).
	if params.CollectionIDs != nil && len(params.CollectionIDs) == 0 && len(params.ItemIDs) == 0 {
		return &SearchResponse{Results: []SearchResult{}, Limit: params.Limit, Offset: params.Offset}, nil
	}

	var results []SearchResult

	// Check if the query looks like an item ref (e.g. "TASK-5", "BUG-8")
	// and do a direct lookup first so refs are always findable.
	if prefix, number, ok := parseItemRef(strings.TrimSpace(params.Query)); ok {
		refQuery := `
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
			WHERE c.prefix = ? AND i.item_number = ? AND i.deleted_at IS NULL
		`
		refArgs := []interface{}{prefix, number}

		if params.Workspace != "" {
			refQuery += ` AND i.workspace_id = (SELECT id FROM workspaces WHERE slug = ? AND deleted_at IS NULL)`
			refArgs = append(refArgs, params.Workspace)
		} else if len(params.WorkspaceIDs) > 0 {
			refQuery += ` AND i.workspace_id IN (` + placeholders(len(params.WorkspaceIDs)) + `)`
			for _, id := range params.WorkspaceIDs {
				refArgs = append(refArgs, id)
			}
		}

		if len(params.CollectionIDs) > 0 && len(params.ItemIDs) > 0 {
			refQuery += ` AND (i.collection_id IN (` + placeholders(len(params.CollectionIDs)) + `) OR i.id IN (` + placeholders(len(params.ItemIDs)) + `))`
			for _, id := range params.CollectionIDs {
				refArgs = append(refArgs, id)
			}
			for _, id := range params.ItemIDs {
				refArgs = append(refArgs, id)
			}
		} else if len(params.CollectionIDs) > 0 {
			refQuery += ` AND i.collection_id IN (` + placeholders(len(params.CollectionIDs)) + `)`
			for _, id := range params.CollectionIDs {
				refArgs = append(refArgs, id)
			}
		} else if len(params.ItemIDs) > 0 {
			refQuery += ` AND i.id IN (` + placeholders(len(params.ItemIDs)) + `)`
			for _, id := range params.ItemIDs {
				refArgs = append(refArgs, id)
			}
		}

		// Apply content filters to ref lookup too
		if params.Collection != "" {
			refQuery += ` AND c.slug = ?`
			refArgs = append(refArgs, params.Collection)
		}
		for key, value := range params.FieldFilters {
			if !validFieldKey.MatchString(key) {
				continue // skip unsafe keys
			}
			refQuery += ` AND ` + s.dialect.JSONExtractText("i.fields", key) + ` = ?`
			refArgs = append(refArgs, value)
		}

		refRows, err := s.db.Query(s.q(refQuery), refArgs...)
		if err == nil {
			defer refRows.Close()
			for refRows.Next() {
				var r SearchResult
				var createdAt, updatedAt string
				var pinned bool
				if err := refRows.Scan(
					&r.Item.ID, &r.Item.WorkspaceID, &r.Item.CollectionID, &r.Item.Title, &r.Item.Slug,
					&r.Item.Content, &r.Item.Fields, &r.Item.Tags,
					&pinned, &r.Item.SortOrder, &r.Item.ParentID, &r.Item.AssignedUserID, &r.Item.AgentRoleID, &r.Item.RoleSortOrder,
					&r.Item.CreatedBy, &r.Item.LastModifiedBy,
					&r.Item.Source, &r.Item.ItemNumber, &createdAt, &updatedAt,
					&r.Item.CollectionSlug, &r.Item.CollectionName, &r.Item.CollectionIcon, &r.Item.CollectionPrefix,
					&r.Item.AssignedUserName, &r.Item.AssignedUserEmail,
					&r.Item.AgentRoleName, &r.Item.AgentRoleSlug, &r.Item.AgentRoleIcon,
				); err != nil {
					continue
				}
				r.Item.Pinned = pinned
				r.Item.CreatedAt = parseTime(createdAt)
				r.Item.UpdatedAt = parseTime(updatedAt)
				hydrateItemComputedMetadata(&r.Item)
				r.Item.Content = ""
				r.Snippet = r.Item.Title
				r.Rank = -1000 // Best possible rank so it sorts first
				results = append(results, r)
			}
		}
		// If no ref matches, fall through to FTS below
	}

	// Build the FTS query — the approach differs between SQLite (FTS5 virtual table)
	// and PostgreSQL (tsvector column on the items table).
	var query string
	var args []interface{}

	if s.dialect.Driver() == DriverPostgres {
		// PostgreSQL: search_vector is a column on the items table (aliased as "i"); no JOIN needed.
		ftsSnippet := s.dialect.FTSSnippet("i", 1, "i.content")
		ftsRank := s.dialect.FTSRank("i", "search_vector")
		ftsMatch := s.dialect.FTSMatch("i", "search_vector")

		// FTSSnippet, FTSRank, and FTSMatch each consume a "?" placeholder
		// for the query parameter (plainto_tsquery('english', ?)).
		query = fmt.Sprintf(`
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
		searchQuery := params.Query
		args = []interface{}{searchQuery, searchQuery, searchQuery}
	} else {
		// SQLite: uses FTS5 virtual table with JOIN on rowid.
		ftsSnippet := s.dialect.FTSSnippet("items_fts", 1, "i.content")
		ftsRank := s.dialect.FTSRank("items_fts", "search_vector")
		ftsMatch := s.dialect.FTSMatch("items_fts", "search_vector")

		query = fmt.Sprintf(`
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
		args = []interface{}{sanitizeFTSQuery(params.Query)}
	}

	if params.Workspace != "" {
		query += `
			AND i.workspace_id = (
				SELECT id FROM workspaces WHERE slug = ? AND deleted_at IS NULL
			)
		`
		args = append(args, params.Workspace)
	} else if len(params.WorkspaceIDs) > 0 {
		query += ` AND i.workspace_id IN (` + placeholders(len(params.WorkspaceIDs)) + `)`
		for _, id := range params.WorkspaceIDs {
			args = append(args, id)
		}
	}

	if len(params.CollectionIDs) > 0 && len(params.ItemIDs) > 0 {
		query += ` AND (i.collection_id IN (` + placeholders(len(params.CollectionIDs)) + `) OR i.id IN (` + placeholders(len(params.ItemIDs)) + `))`
		for _, id := range params.CollectionIDs {
			args = append(args, id)
		}
		for _, id := range params.ItemIDs {
			args = append(args, id)
		}
	} else if len(params.CollectionIDs) > 0 {
		query += ` AND i.collection_id IN (` + placeholders(len(params.CollectionIDs)) + `)`
		for _, id := range params.CollectionIDs {
			args = append(args, id)
		}
	} else if len(params.ItemIDs) > 0 {
		query += ` AND i.id IN (` + placeholders(len(params.ItemIDs)) + `)`
		for _, id := range params.ItemIDs {
			args = append(args, id)
		}
	}

	// Collection slug filter — scope to a single collection by slug.
	if params.Collection != "" {
		query += ` AND c.slug = ?`
		args = append(args, params.Collection)
	}

	// Field filters — filter by structured field values in the JSON fields column.
	for key, value := range params.FieldFilters {
		if !validFieldKey.MatchString(key) {
			continue // skip unsafe keys
		}
		query += ` AND ` + s.dialect.JSONExtractText("i.fields", key) + ` = ?`
		args = append(args, value)
	}

	// --- Count query (total matching results before pagination) ---
	// Build the count query by replacing the SELECT columns with COUNT(*).
	// We reuse the same WHERE/JOIN clauses.
	var countQuery string
	countArgs := make([]interface{}, len(args))
	copy(countArgs, args)

	if s.dialect.Driver() == DriverPostgres {
		ftsMatch := s.dialect.FTSMatch("i", "search_vector")
		countQuery = fmt.Sprintf(`
			SELECT COUNT(*)
			FROM items i
			JOIN collections c ON c.id = i.collection_id
			WHERE %s AND i.deleted_at IS NULL
		`, ftsMatch)
		countArgs = []interface{}{params.Query}
	} else {
		ftsMatch := s.dialect.FTSMatch("items_fts", "search_vector")
		countQuery = fmt.Sprintf(`
			SELECT COUNT(*)
			FROM items_fts fts
			JOIN items i ON i.rowid = fts.rowid
			JOIN collections c ON c.id = i.collection_id
			WHERE %s AND i.deleted_at IS NULL
		`, ftsMatch)
		countArgs = []interface{}{sanitizeFTSQuery(params.Query)}
	}

	// Replay the same filters for the count query
	countQuery, countArgs = s.appendSearchFilters(countQuery, countArgs, params)

	var total int
	countErr := s.db.QueryRow(s.q(countQuery), countArgs...).Scan(&total)
	if countErr != nil {
		// Count failure is non-fatal — fall back to reporting result count
		total = -1
	}

	// --- Sorting ---
	orderClause := s.searchOrderClause(params)
	query += orderClause

	// --- Pagination ---
	query += fmt.Sprintf(" LIMIT %d OFFSET %d", params.Limit, params.Offset)

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		// If we already have a ref match, return that instead of failing
		// (FTS5 may reject queries like "TASK-5" due to special syntax)
		if len(results) > 0 {
			if total < 0 {
				total = len(results)
			}
			return &SearchResponse{Results: results, Total: total, Limit: params.Limit, Offset: params.Offset}, nil
		}
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	// Track IDs we already have from the ref lookup to avoid duplicates
	seen := make(map[string]bool)
	for _, r := range results {
		seen[r.Item.ID] = true
	}

	for rows.Next() {
		var r SearchResult
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
		// Skip if already included from ref lookup
		if seen[r.Item.ID] {
			continue
		}
		r.Item.Pinned = pinned
		r.Item.CreatedAt = parseTime(createdAt)
		r.Item.UpdatedAt = parseTime(updatedAt)
		hydrateItemComputedMetadata(&r.Item)
		// Don't include full content in search results
		r.Item.Content = ""
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if total < 0 {
		total = len(results)
	}

	// The count query only covers FTS hits, but direct ref lookups (e.g.
	// searching "TASK-5") can return matches not in the FTS index. Ensure
	// total is never less than actual results returned.
	if len(seen) > 0 && total < len(results) {
		total = len(results)
	}

	return &SearchResponse{Results: results, Total: total, Limit: params.Limit, Offset: params.Offset}, nil
}

// appendSearchFilters adds workspace, collection, and field filter clauses to a query.
// Used to keep count and results queries in sync.
func (s *Store) appendSearchFilters(query string, args []interface{}, params SearchParams) (string, []interface{}) {
	if params.Workspace != "" {
		query += ` AND i.workspace_id = (SELECT id FROM workspaces WHERE slug = ? AND deleted_at IS NULL)`
		args = append(args, params.Workspace)
	} else if len(params.WorkspaceIDs) > 0 {
		query += ` AND i.workspace_id IN (` + placeholders(len(params.WorkspaceIDs)) + `)`
		for _, id := range params.WorkspaceIDs {
			args = append(args, id)
		}
	}

	if len(params.CollectionIDs) > 0 && len(params.ItemIDs) > 0 {
		query += ` AND (i.collection_id IN (` + placeholders(len(params.CollectionIDs)) + `) OR i.id IN (` + placeholders(len(params.ItemIDs)) + `))`
		for _, id := range params.CollectionIDs {
			args = append(args, id)
		}
		for _, id := range params.ItemIDs {
			args = append(args, id)
		}
	} else if len(params.CollectionIDs) > 0 {
		query += ` AND i.collection_id IN (` + placeholders(len(params.CollectionIDs)) + `)`
		for _, id := range params.CollectionIDs {
			args = append(args, id)
		}
	} else if len(params.ItemIDs) > 0 {
		query += ` AND i.id IN (` + placeholders(len(params.ItemIDs)) + `)`
		for _, id := range params.ItemIDs {
			args = append(args, id)
		}
	}

	if params.Collection != "" {
		query += ` AND c.slug = ?`
		args = append(args, params.Collection)
	}

	for key, value := range params.FieldFilters {
		if !validFieldKey.MatchString(key) {
			continue
		}
		query += ` AND ` + s.dialect.JSONExtractText("i.fields", key) + ` = ?`
		args = append(args, value)
	}

	return query, args
}

// searchOrderClause returns the ORDER BY clause for search results.
func (s *Store) searchOrderClause(params SearchParams) string {
	switch params.Sort {
	case "created_at":
		return fmt.Sprintf(" ORDER BY i.created_at %s", params.Order)
	case "updated_at":
		return fmt.Sprintf(" ORDER BY i.updated_at %s", params.Order)
	case "title":
		return fmt.Sprintf(" ORDER BY i.title %s", params.Order)
	default: // "relevance"
		// SQLite bm25() returns negative values (more negative = more relevant) → ASC.
		// PostgreSQL ts_rank() returns positive values (higher = more relevant) → DESC.
		if s.dialect.Driver() == DriverPostgres {
			return " ORDER BY rank_score DESC"
		}
		return " ORDER BY rank_score"
	}
}

// sanitizeFTSQuery wraps each token in double quotes so FTS5 treats
// special characters (like hyphens) as literals rather than operators.
// Only used for SQLite FTS5 queries.
func sanitizeFTSQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return q
	}
	tokens := strings.Fields(q)
	for i, t := range tokens {
		// Remove any existing double quotes, then wrap in quotes
		t = strings.ReplaceAll(t, `"`, ``)
		tokens[i] = `"` + t + `"`
	}
	return strings.Join(tokens, " ")
}
