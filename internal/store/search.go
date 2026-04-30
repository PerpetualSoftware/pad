package store

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// validFieldKey matches safe JSON field keys: alphanumeric, underscores, hyphens only.
var validFieldKey = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

type SearchResult struct {
	Item    models.Item `json:"item"`
	Snippet string      `json:"snippet"`
	Rank    float64     `json:"rank"`
}

type SearchFacets struct {
	Collections map[string]int `json:"collections"` // collection slug → count
	Statuses    map[string]int `json:"statuses"`    // status value → count
}

type SearchResponse struct {
	Results []SearchResult `json:"results"`
	Total   int            `json:"total"`
	Limit   int            `json:"limit"`
	Offset  int            `json:"offset"`
	Facets  *SearchFacets  `json:"facets,omitempty"`
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

	// Whitespace-only queries collapse to empty after FTS5 sanitization and
	// would error on `MATCH ''`. Treat them as no-result rather than letting
	// SQLite raise a syntax error. See BUG-818.
	if strings.TrimSpace(params.Query) == "" {
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

		// Stable order for cross-workspace global searches (where the same
		// PREFIX-N can match in multiple workspaces) so pagination is
		// deterministic across pages.
		refQuery += ` ORDER BY i.workspace_id, i.id`

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

	// Bare numeric query (e.g. "843") — resolve to the workspace-global
	// item with that item_number. Mirrors the parseItemRef block above
	// but without a collection prefix filter, since item_number is unique
	// per workspace (idx_items_workspace_number). At most one item matches.
	// Lets the search palette double as a quick "go to item N" jump. See BUG-910.
	if number, ok := parseItemNumber(strings.TrimSpace(params.Query)); ok {
		numQuery := `
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
			WHERE i.item_number = ? AND i.deleted_at IS NULL
		`
		numArgs := []interface{}{number}

		if params.Workspace != "" {
			numQuery += ` AND i.workspace_id = (SELECT id FROM workspaces WHERE slug = ? AND deleted_at IS NULL)`
			numArgs = append(numArgs, params.Workspace)
		} else if len(params.WorkspaceIDs) > 0 {
			numQuery += ` AND i.workspace_id IN (` + placeholders(len(params.WorkspaceIDs)) + `)`
			for _, id := range params.WorkspaceIDs {
				numArgs = append(numArgs, id)
			}
		}

		if len(params.CollectionIDs) > 0 && len(params.ItemIDs) > 0 {
			numQuery += ` AND (i.collection_id IN (` + placeholders(len(params.CollectionIDs)) + `) OR i.id IN (` + placeholders(len(params.ItemIDs)) + `))`
			for _, id := range params.CollectionIDs {
				numArgs = append(numArgs, id)
			}
			for _, id := range params.ItemIDs {
				numArgs = append(numArgs, id)
			}
		} else if len(params.CollectionIDs) > 0 {
			numQuery += ` AND i.collection_id IN (` + placeholders(len(params.CollectionIDs)) + `)`
			for _, id := range params.CollectionIDs {
				numArgs = append(numArgs, id)
			}
		} else if len(params.ItemIDs) > 0 {
			numQuery += ` AND i.id IN (` + placeholders(len(params.ItemIDs)) + `)`
			for _, id := range params.ItemIDs {
				numArgs = append(numArgs, id)
			}
		}

		// Apply content filters to numeric lookup too
		if params.Collection != "" {
			numQuery += ` AND c.slug = ?`
			numArgs = append(numArgs, params.Collection)
		}
		for key, value := range params.FieldFilters {
			if !validFieldKey.MatchString(key) {
				continue
			}
			numQuery += ` AND ` + s.dialect.JSONExtractText("i.fields", key) + ` = ?`
			numArgs = append(numArgs, value)
		}

		// Avoid duplicating an item already added by the parseItemRef path
		// (can't happen for purely numeric queries, but cheap to be safe).
		alreadySeen := make(map[string]bool, len(results))
		for _, r := range results {
			alreadySeen[r.Item.ID] = true
		}

		// Stable cross-workspace order for global searches (item_number is
		// only unique per workspace, so q="1" can match one row per workspace).
		numQuery += ` ORDER BY i.workspace_id, i.id`

		numRows, err := s.db.Query(s.q(numQuery), numArgs...)
		if err == nil {
			defer numRows.Close()
			for numRows.Next() {
				var r SearchResult
				var createdAt, updatedAt string
				var pinned bool
				if err := numRows.Scan(
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
				if alreadySeen[r.Item.ID] {
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
		// FTS still runs below — bare numeric tokens may also match titles/content
	}

	// Snapshot direct hits (ref + numeric) before FTS so we can:
	//   1) exclude their IDs in the FTS WHERE clause to prevent them from
	//      consuming pagination slots that would otherwise be deduped after
	//      LIMIT/OFFSET (which would shrink page 0 below `limit` and skew
	//      later pages); and
	//   2) add their count back to the FTS total so SearchResponse.Total
	//      reflects the true result set size.
	// See Codex review for BUG-864/910 round 1.
	refHits := results
	refCount := len(refHits)
	refIDs := make(map[string]bool, refCount)
	directHitIDs := make([]string, 0, refCount)
	for _, r := range refHits {
		refIDs[r.Item.ID] = true
		directHitIDs = append(directHitIDs, r.Item.ID)
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

		// PG FTSSnippet, FTSRank, and FTSMatch each consume TWO "?" args
		// (raw query + hyphen-sanitized query) for the OR-combined
		// plainto_tsquery — see dialect.go and BUG-842.
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
		sanitized := sanitizePGFTSQuery(searchQuery)
		args = []interface{}{
			searchQuery, sanitized, // FTSSnippet
			searchQuery, sanitized, // FTSRank
			searchQuery, sanitized, // FTSMatch
		}
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

	// Exclude direct hits (ref + numeric) from FTS so duplicates don't consume
	// pagination slots. See snapshot above and Codex review for BUG-864/910 r1.
	if len(directHitIDs) > 0 {
		query += ` AND i.id NOT IN (` + placeholders(len(directHitIDs)) + `)`
		for _, id := range directHitIDs {
			args = append(args, id)
		}
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
		// PG FTSMatch consumes TWO args (raw + sanitized) — BUG-842.
		countArgs = []interface{}{params.Query, sanitizePGFTSQuery(params.Query)}
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

	// Mirror the FTS direct-hit exclusion so the count matches the SELECT.
	if len(directHitIDs) > 0 {
		countQuery += ` AND i.id NOT IN (` + placeholders(len(directHitIDs)) + `)`
		for _, id := range directHitIDs {
			countArgs = append(countArgs, id)
		}
	}

	var total int
	countErr := s.db.QueryRow(s.q(countQuery), countArgs...).Scan(&total)
	if countErr != nil {
		// Count failure is non-fatal — fall back to reporting result count
		total = -1
	} else {
		// FTS count excluded direct hits via NOT IN — add them back so
		// SearchResponse.Total reflects the full result set the caller sees.
		total += refCount
	}

	// --- Faceted counts (full result set, not paginated) ---
	facets := s.searchFacets(params)

	// Note: facets are computed from FTS results only. Direct ref hits
	// (e.g. searching "TASK-42") may not appear in FTS and thus may not
	// be reflected in facet counts. This is acceptable — ref searches
	// typically return 1 exact match, and overcounting would be worse
	// since we can't cheaply detect FTS overlap.

	// --- Sorting (with deterministic tie-breaker for stable pagination) ---
	query += s.searchOrderClause(params)

	// --- Pagination ---
	// Direct hits (ref + numeric) sort first conceptually (rank=-1000); FTS
	// fills in afterwards. Both halves must be paginated together to honour
	// (offset, limit). In global searches (no `workspace`), the same
	// PREFIX-N or bare number can match in multiple workspaces, so refCount
	// can exceed Limit — we must slice direct hits, not just append them.
	// See Codex review for BUG-864/910 round 2.

	if total < 0 {
		total = 0
	}

	// Slice direct hits according to (offset, limit). After this block,
	// `results` holds the direct-hit page slice, `directConsumed` is how
	// many direct-hit slots that consumed, and ftsLimit/ftsOffset are the
	// FTS-side adjustments to fill the remaining slots / skip past direct
	// hits we already showed.
	directStart := params.Offset
	if directStart > refCount {
		directStart = refCount
	}
	directEnd := params.Offset + params.Limit
	if directEnd > refCount {
		directEnd = refCount
	}
	results = results[directStart:directEnd]
	directConsumed := directEnd - directStart

	ftsLimit := params.Limit - directConsumed
	if ftsLimit < 0 {
		ftsLimit = 0
	}
	ftsOffset := params.Offset - refCount
	if ftsOffset < 0 {
		ftsOffset = 0
	}
	query += fmt.Sprintf(" LIMIT %d OFFSET %d", ftsLimit, ftsOffset)

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		// If we already have ref matches, return those instead of failing
		// (FTS5 may reject queries like "TASK-5" due to special syntax)
		if len(results) > 0 {
			return &SearchResponse{Results: results, Total: total, Limit: params.Limit, Offset: params.Offset, Facets: facets}, nil
		}
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

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
		// Skip items already included from ref lookup
		if refIDs[r.Item.ID] {
			continue
		}
		r.Item.Pinned = pinned
		r.Item.CreatedAt = parseTime(createdAt)
		r.Item.UpdatedAt = parseTime(updatedAt)
		hydrateItemComputedMetadata(&r.Item)
		r.Item.Content = ""
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Ensure total is never less than actual results. The FTS count query
	// may miss direct ref hits that aren't in the FTS index, so the real
	// total can exceed the count. This is a simple, safe floor.
	if total < len(results) {
		total = len(results)
	}

	// Normalize nil → empty slice so JSON always serializes `results` as
	// `[]` not `null`. Frontend consumers (CommandPalette) read .length
	// without a null check.
	if results == nil {
		results = []SearchResult{}
	}

	return &SearchResponse{Results: results, Total: total, Limit: params.Limit, Offset: params.Offset, Facets: facets}, nil
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
// Includes i.id as a tie-breaker for deterministic pagination.
func (s *Store) searchOrderClause(params SearchParams) string {
	switch params.Sort {
	case "created_at":
		return fmt.Sprintf(" ORDER BY i.created_at %s, i.id", params.Order)
	case "updated_at":
		return fmt.Sprintf(" ORDER BY i.updated_at %s, i.id", params.Order)
	case "title":
		return fmt.Sprintf(" ORDER BY i.title %s, i.id", params.Order)
	default: // "relevance"
		// SQLite bm25() returns negative values (more negative = more relevant) → ASC.
		// PostgreSQL ts_rank() returns positive values (higher = more relevant) → DESC.
		if s.dialect.Driver() == DriverPostgres {
			return " ORDER BY rank_score DESC, i.id"
		}
		return " ORDER BY rank_score, i.id"
	}
}

// searchFacets runs two GROUP BY queries against the full (unpaginated) search
// result set to produce collection and status breakdowns. Non-fatal — returns
// nil if either query fails.
func (s *Store) searchFacets(params SearchParams) *SearchFacets {
	statusExtract := s.dialect.JSONExtractText("i.fields", "status")

	var baseQuery string
	var baseArgs []interface{}

	if s.dialect.Driver() == DriverPostgres {
		ftsMatch := s.dialect.FTSMatch("i", "search_vector")
		baseQuery = fmt.Sprintf(`
			FROM items i
			JOIN collections c ON c.id = i.collection_id
			WHERE %s AND i.deleted_at IS NULL
		`, ftsMatch)
		// PG FTSMatch consumes TWO args (raw + sanitized) — BUG-842.
		baseArgs = []interface{}{params.Query, sanitizePGFTSQuery(params.Query)}
	} else {
		ftsMatch := s.dialect.FTSMatch("items_fts", "search_vector")
		baseQuery = fmt.Sprintf(`
			FROM items_fts fts
			JOIN items i ON i.rowid = fts.rowid
			JOIN collections c ON c.id = i.collection_id
			WHERE %s AND i.deleted_at IS NULL
		`, ftsMatch)
		baseArgs = []interface{}{sanitizeFTSQuery(params.Query)}
	}

	baseQuery, baseArgs = s.appendSearchFilters(baseQuery, baseArgs, params)

	facets := &SearchFacets{
		Collections: make(map[string]int),
		Statuses:    make(map[string]int),
	}

	// Collection facets
	collQuery := "SELECT c.slug, COUNT(*) " + baseQuery + " GROUP BY c.slug"
	collRows, err := s.db.Query(s.q(collQuery), baseArgs...)
	if err == nil {
		defer collRows.Close()
		for collRows.Next() {
			var slug string
			var count int
			if collRows.Scan(&slug, &count) == nil {
				facets.Collections[slug] = count
			}
		}
	}

	// Status facets
	statusQuery := fmt.Sprintf("SELECT %s, COUNT(*) %s AND %s IS NOT NULL GROUP BY %s",
		statusExtract, baseQuery, statusExtract, statusExtract)
	// Need fresh copy of args since baseArgs may be consumed
	statusArgs := make([]interface{}, len(baseArgs))
	copy(statusArgs, baseArgs)
	statusRows, err := s.db.Query(s.q(statusQuery), statusArgs...)
	if err == nil {
		defer statusRows.Close()
		for statusRows.Next() {
			var status string
			var count int
			if statusRows.Scan(&status, &count) == nil && status != "" {
				facets.Statuses[status] = count
			}
		}
	}

	return facets
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

// sanitizePGFTSQuery returns a copy of q with hyphens replaced by spaces.
// Used as the second leg of the OR-combined PG FTS query: postgres'
// english parser indexes a hyphenated word as both an asciihword token
// and its parts (`task-five-distinctive` → `task-five-distinct, task,
// five, distinct`), but plainto_tsquery applied to a partial hyphenated
// query produces the stemmed asciihword for that partial input
// (`task-fiv`), which is NOT in the vector — so the bare match returns
// 0 rows. ORing in `plainto_tsquery(<hyphens-as-spaces>)` makes the
// part lexemes (`task & five`) participate in the match, fixing the
// regression for `task-five` while leaving cases like `BUG-842` (which
// the english parser indexes as `bug, -842` and DOES match the bare
// query) untouched. See BUG-842 and the FTSMatch dialect method.
func sanitizePGFTSQuery(q string) string {
	return strings.ReplaceAll(q, "-", " ")
}
