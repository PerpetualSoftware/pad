package store

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// validFieldKey matches safe JSON field keys: alphanumeric, underscores, hyphens only.
var validFieldKey = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

type SearchResult struct {
	Item    models.Item `json:"item"`
	Snippet string      `json:"snippet"`
	// Rank is an opaque relevance score; higher is better on both dialects.
	Rank      float64 `json:"rank"`
	MatchedIn string  `json:"matched_in"`
	CommentID string  `json:"comment_id,omitempty"`
	// EntryID is set on a `note` or `decision` match (TASK-4491): the stored
	// id of the matching implementation note or decision-log entry, the same
	// value `item show` and the append echo carry. Not its index, which
	// shifts when an earlier entry is removed.
	EntryID string `json:"entry_id,omitempty"`
	// Workspace says which workspace the item is in, so a search across
	// workspaces can be navigated (TASK-3604). The handler fills it from the
	// same access check that admitted the workspace into the search; it is
	// never derived from the row alone.
	Workspace *SearchResultWorkspace `json:"workspace,omitempty"`
}

// SearchResultWorkspace is the route a result lives under: /{owner}/{slug}.
type SearchResultWorkspace struct {
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	OwnerUsername string `json:"owner_username"`
}

type SearchFacets struct {
	Collections     map[string]int `json:"collections"`          // collection slug → count
	Statuses        map[string]int `json:"statuses"`             // status value → count
	WorkspaceCounts map[string]int `json:"-"`                    // workspace ID → count (unscoped searches only)
	Workspaces      map[string]int `json:"workspaces,omitempty"` // workspace slug → count (filled by handler)
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
	Query        string
	Workspace    string   // workspace slug, optional — scopes to single workspace
	WorkspaceIDs []string // workspace IDs to scope results to (used when no specific workspace is given)
	// The permission filter (BUG-3331). An item is returned only if its
	// collection is in CollectionIDs or the item itself is in ItemIDs. Both
	// empty matches NOTHING, whether nil or not, unless Unrestricted is set:
	// "the caller can see every item in scope" must be said explicitly, so a
	// zero-visibility caller can never be mistaken for an unfiltered one.
	// Unrestricted with non-empty IDs still applies the IDs.
	CollectionIDs []string
	ItemIDs       []string
	Unrestricted  bool

	// Content filters (applied on top of permission filters)
	Collection string // collection slug — scope search to a single collection
	// CollectionFilterIDs, when non-nil, replaces Collection: the filter
	// resolved to concrete collections, one per in-scope workspace
	// (BUG-2659). One slug cannot express it, because the same input can
	// name `task` in one workspace and resolve to `tasks` in another.
	// Non-nil and empty matches nothing.
	CollectionFilterIDs []string
	FieldFilters        map[string]string // field key → value filters (e.g. {"status": "open", "priority": "high"})
	// IncludeArchived widens every query path (ref, number, FTS, count,
	// facets) to soft-deleted items as well as live ones (TASK-2864). An
	// archived result carries its deleted_at. It widens nothing else: the
	// permission filter, the soft-deleted-collection filter and the
	// workspace scope apply as before.
	IncludeArchived bool

	// Pagination
	Limit  int // max results per page (default 50, max 200)
	Offset int // skip this many results

	// Sorting: "relevance" (default), "created_at", "updated_at", "title"
	Sort  string // sort field
	Order string // "asc" or "desc" (default depends on sort field)

	// Now overrides the reference timestamp for recency scoring (zero means
	// time.Now()). Recency cutoffs are quantised to UTC midnight, so two pages
	// fetched either side of a UTC midnight may disagree; this is a known,
	// accepted limit.
	Now time.Time
}

func (s *Store) searchStatusFilters(params SearchParams) []collectionDoneFilter {
	if params.CollectionFilterIDs != nil && len(params.CollectionFilterIDs) == 0 {
		return nil
	}
	var query string
	var args []interface{}
	switch {
	case params.Workspace != "":
		query = `SELECT id, schema, settings FROM collections WHERE deleted_at IS NULL AND workspace_id IN (SELECT w.id FROM workspaces w JOIN workspace_slug_claims c ON c.workspace_id = w.id WHERE c.slug = ? AND w.deleted_at IS NULL)`
		args = append(args, params.Workspace)
	case len(params.WorkspaceIDs) > 0:
		query = `SELECT id, schema, settings FROM collections WHERE deleted_at IS NULL AND workspace_id IN (` + placeholders(len(params.WorkspaceIDs)) + `)`
		for _, id := range params.WorkspaceIDs {
			args = append(args, id)
		}
	default:
		return nil
	}
	if params.CollectionFilterIDs != nil {
		query += ` AND id IN (` + placeholders(len(params.CollectionFilterIDs)) + `)`
		for _, id := range params.CollectionFilterIDs {
			args = append(args, id)
		}
	}
	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanCollectionDoneFilters(rows)
}

func (s *Store) searchScoreExpr(params SearchParams, filters []collectionDoneFilter, now time.Time) (string, []interface{}) {
	if now.IsZero() {
		now = time.Now()
	}
	trimmed := strings.TrimSpace(params.Query)
	var factors []string
	var args []interface{}

	if s.dialect.Driver() == DriverPostgres {
		// The array is ordered {D,C,B,A}; the trigger sets title A, content B,
		// tags C, so this is title 1.0, tags 0.5, content 0.4, matching SQLite's
		// title 2.5, tags 1.25, content 1.0. Do not reorder.
		tsquery, baseArgs := postgresFTSQuery(params.Query)
		factors = append(factors, fmt.Sprintf("ts_rank('{0.1,0.5,0.4,1.0}'::float4[], i.search_vector, %s)", tsquery))
		args = append(args, baseArgs...)
	} else {
		factors = append(factors, "(-bm25(items_fts, 2.5, 1.0, 1.25))")
	}

	likePattern := "%" + escapeLikePattern(trimmed) + "%"
	factors = append(factors, `(CASE WHEN LOWER(TRIM(i.title)) = LOWER(?) THEN 3.0 WHEN LOWER(i.title) LIKE LOWER(?) ESCAPE '\' THEN 1.8 ELSE 1.0 END)`)
	args = append(args, trimmed, likePattern)

	if len(strings.Fields(trimmed)) >= 2 {
		factors = append(factors, `(CASE WHEN LOWER(i.content) LIKE LOWER(?) ESCAPE '\' THEN 1.3 ELSE 1.0 END)`)
		args = append(args, likePattern)
	}

	if len(filters) == 0 {
		factors = append(factors, "1.0")
	} else {
		abandonedExpr, abandonedArgs, terminalExpr, terminalArgs := s.buildSearchStatusExprs(filters, "i")
		factors = append(factors, fmt.Sprintf("(CASE WHEN %s THEN 0.35 WHEN %s THEN 0.6 ELSE 1.0 END)", abandonedExpr, terminalExpr))
		args = append(args, abandonedArgs...)
		args = append(args, terminalArgs...)
	}

	if params.IncludeArchived {
		factors = append(factors, "(CASE WHEN i.deleted_at IS NOT NULL THEN 0.5 ELSE 1.0 END)")
	}

	utc := now.UTC()
	midnight := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	cutoff7 := midnight.AddDate(0, 0, -7).Format(time.RFC3339)
	cutoff30 := midnight.AddDate(0, 0, -30).Format(time.RFC3339)
	cutoff180 := midnight.AddDate(0, 0, -180).Format(time.RFC3339)
	factors = append(factors, "(CASE WHEN i.updated_at >= ? THEN 1.25 WHEN i.updated_at >= ? THEN 1.12 WHEN i.updated_at >= ? THEN 1.0 ELSE 0.9 END)")
	args = append(args, cutoff7, cutoff30, cutoff180)

	return "(" + strings.Join(factors, " * ") + ")", args
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

	// No visible collections and no granted items, and not explicitly
	// unrestricted: nothing is visible (BUG-3331).
	if !params.Unrestricted && len(params.CollectionIDs) == 0 && len(params.ItemIDs) == 0 {
		return &SearchResponse{Results: []SearchResult{}, Limit: params.Limit, Offset: params.Offset}, nil
	}

	// Whitespace-only queries collapse to empty after FTS5 sanitization and
	// would error on `MATCH ''`. Treat them as no-result rather than letting
	// SQLite raise a syntax error. See BUG-818.
	if strings.TrimSpace(params.Query) == "" {
		return &SearchResponse{Results: []SearchResult{}, Limit: params.Limit, Offset: params.Offset}, nil
	}

	// Refs and bare numbers resolve before full-text matches.
	directHitIDs := []string{}
	var directWhere string
	var directArgs []interface{}
	if prefix, number, ok := parseItemRef(strings.TrimSpace(params.Query)); ok {
		directWhere = "c.prefix = ? AND i.item_number = ?"
		directArgs = []interface{}{prefix, number}
	} else if number, ok := parseItemNumber(strings.TrimSpace(params.Query)); ok {
		directWhere = "i.item_number = ?"
		directArgs = []interface{}{number}
	}
	if directWhere != "" {
		query := "SELECT i.id FROM items i JOIN collections c ON c.id = i.collection_id WHERE " + directWhere
		query, directArgs = s.appendSearchFilters(query, directArgs, params)
		rows, err := s.db.Query(s.q(query), directArgs...)
		if err != nil {
			return nil, fmt.Errorf("search direct: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			directHitIDs = append(directHitIDs, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	countMatches, countArgs := s.searchMatchIDs(params, directHitIDs)
	var total int
	if err := s.db.QueryRow(s.q(countMatches+" SELECT COUNT(*) FROM search_matches"), countArgs...).Scan(&total); err != nil {
		return nil, fmt.Errorf("search count: %w", err)
	}
	facets := s.searchFacetsForMatches(params, countMatches, countArgs)

	refNow := params.Now
	if refNow.IsZero() {
		refNow = time.Now()
	}
	statusFilters := s.searchStatusFilters(params)
	matches, args := s.searchMatches(params, directHitIDs, statusFilters, refNow)

	// Keep the ranks of the two indexes separate. Direct hits (leg 0) order by
	// updated_at DESC, id; explicit item sorting applies within each FTS leg,
	// with the existing item id tie-breaker.
	order := " ORDER BY m.match_leg, CASE WHEN m.match_leg = 0 THEN i.updated_at ELSE '' END DESC, CASE WHEN m.match_leg = 0 THEN i.id ELSE '' END, " + strings.TrimPrefix(s.searchOrderClause(params), " ORDER BY ")
	page, args := s.searchPage(params, matches, args, order)
	query := page + `
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, ` + contentStateSQL + `, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.seq, i.created_at, i.updated_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, ''), i.deleted_at,
		       CASE m.match_leg WHEN 0 THEN i.title WHEN 1 THEN its.snippet ELSE COALESCE(cms.snippet, '') END,
		       COALESCE(its.title_snippet, ''), COALESCE(its.tags_snippet, ''), m.rank_score, m.comment_id, m.match_leg,
		       COALESCE(es.kind, ''), COALESCE(es.entry_id, ''), COALESCE(es.head_snippet, ''), COALESCE(es.body_snippet, ''),
		       COALESCE(its.content_matched, 0), COALESCE(its.title_matched, 0), COALESCE(its.tags_matched, 0), COALESCE(es.head_matched, 0)
		FROM search_page m
		JOIN items i ON i.id = m.id
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		LEFT JOIN item_snippets its ON its.id = m.id
		LEFT JOIN comment_snippets cms ON cms.comment_id = m.comment_id
		LEFT JOIN entry_snippets es ON es.entry_row = m.entry_row AND es.item_id = m.id
	`
	query += order
	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()
	results := []SearchResult{}
	for rows.Next() {
		var r SearchResult
		var createdAt, updatedAt string
		var deletedAt *string
		var pinned bool
		var titleSnippet, tagsSnippet string
		var entryKind, entryID, headSnippet, bodySnippet string
		var leg int
		var contentMatched, titleMatched, tagsMatched, headMatched int
		if err := rows.Scan(
			&r.Item.ID, &r.Item.WorkspaceID, &r.Item.CollectionID, &r.Item.Title, &r.Item.Slug,
			&r.Item.Content, &r.Item.ContentState, &r.Item.Fields, &r.Item.Tags,
			&pinned, &r.Item.SortOrder, &r.Item.ParentID, &r.Item.AssignedUserID, &r.Item.AgentRoleID, &r.Item.RoleSortOrder,
			&r.Item.CreatedBy, &r.Item.LastModifiedBy,
			&r.Item.Source, &r.Item.ItemNumber, &r.Item.Seq, &createdAt, &updatedAt,
			&r.Item.CollectionSlug, &r.Item.CollectionName, &r.Item.CollectionIcon, &r.Item.CollectionPrefix,
			&r.Item.AssignedUserName, &r.Item.AssignedUserEmail,
			&r.Item.AgentRoleName, &r.Item.AgentRoleSlug, &r.Item.AgentRoleIcon, &deletedAt,
			&r.Snippet, &titleSnippet, &tagsSnippet, &r.Rank, &r.CommentID, &leg,
			&entryKind, &entryID, &headSnippet, &bodySnippet,
			&contentMatched, &titleMatched, &tagsMatched, &headMatched,
		); err != nil {
			return nil, err
		}
		switch {
		case leg == 0:
			r.MatchedIn = "ref"
		case leg == 2 && r.CommentID != "":
			r.MatchedIn = "comment"
		case leg == 2:
			// A note or decision (TASK-4491). The head (summary or
			// decision) is preferred when it carries the match.
			r.MatchedIn, r.EntryID = "note", entryID
			if entryKind == "decision" {
				r.MatchedIn = "decision"
			}
			r.Snippet = bodySnippet
			if headMatched != 0 {
				r.Snippet = headSnippet
			}
		case contentMatched != 0:
			r.MatchedIn = "content"
		case titleMatched != 0:
			r.MatchedIn, r.Snippet = "title", titleSnippet
		case tagsMatched != 0:
			r.MatchedIn, r.Snippet = "tags", plainTagsSnippet(tagsSnippet)
		default:
			r.MatchedIn = "content"
		}
		r.Item.Pinned = pinned
		r.Item.CreatedAt = parseTime(createdAt)
		r.Item.UpdatedAt = parseTime(updatedAt)
		r.Item.DeletedAt = parseTimePtr(deletedAt)
		hydrateItemComputedMetadata(&r.Item)
		r.Item.Content = ""
		// Only a content snippet is body text. Ref, title, tags, comment, note
		// and decision snippets must not carry the body's staleness marker.
		if r.MatchedIn != "content" {
			r.Item.ContentState = ""
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &SearchResponse{Results: results, Total: total, Limit: params.Limit, Offset: params.Offset, Facets: facets}, nil
}

// searchMatchIDs builds the distinct matching item IDs for count and facet
// queries without evaluating ranking scores.
func (s *Store) searchMatchIDs(params SearchParams, directIDs []string) (string, []interface{}) {
	itemTable, commentTable := "items_fts", "comments_fts"
	itemFrom := "items_fts JOIN items i ON i.rowid = items_fts.rowid"
	commentFrom := "comments_fts JOIN comments cm ON cm.rowid = comments_fts.rowid JOIN items i ON i.id = cm.item_id"
	entryTable := "item_entries_fts"
	entryFrom := "item_entries_fts JOIN item_entries e ON e.id = item_entries_fts.rowid JOIN items i ON i.id = e.item_id"
	if s.dialect.Driver() == DriverPostgres {
		itemTable, commentTable, entryTable = "i", "cm", "e"
		itemFrom = "items i"
		commentFrom = "comments cm JOIN items i ON i.id = cm.item_id"
		entryFrom = "item_entries e JOIN items i ON i.id = e.item_id"
	}

	itemMatch, args := s.dialect.FTSMatch(itemTable, "search_vector", params.Query)
	itemQuery := fmt.Sprintf(`SELECT i.id FROM %s JOIN collections c ON c.id = i.collection_id WHERE %s`, itemFrom, itemMatch)
	itemQuery, args = s.appendSearchFilters(itemQuery, args, params)
	if len(directIDs) > 0 {
		itemQuery += " AND i.id NOT IN (" + placeholders(len(directIDs)) + ")"
		for _, id := range directIDs {
			args = append(args, id)
		}
	}

	commentMatch, commentArgs := s.dialect.FTSMatch(commentTable, "search_vector", params.Query)
	commentQuery := fmt.Sprintf(`SELECT i.id FROM %s JOIN collections c ON c.id = i.collection_id WHERE %s AND cm.deleted_at IS NULL`, commentFrom, commentMatch)
	commentQuery, commentArgs = s.appendSearchFilters(commentQuery, commentArgs, params)
	commentQuery += " AND i.id NOT IN (SELECT id FROM item_matches)"
	if len(directIDs) > 0 {
		commentQuery += " AND i.id NOT IN (" + placeholders(len(directIDs)) + ")"
		for _, id := range directIDs {
			commentArgs = append(commentArgs, id)
		}
	}
	args = append(args, commentArgs...)

	entryMatch, entryArgs := s.dialect.FTSMatch(entryTable, "search_vector", params.Query)
	entryQuery := fmt.Sprintf(`SELECT i.id FROM %s JOIN collections c ON c.id = i.collection_id WHERE %s`, entryFrom, entryMatch)
	entryQuery, entryArgs = s.appendSearchFilters(entryQuery, entryArgs, params)
	entryQuery += " AND i.id NOT IN (SELECT id FROM item_matches)"
	if len(directIDs) > 0 {
		entryQuery += " AND i.id NOT IN (" + placeholders(len(directIDs)) + ")"
		for _, id := range directIDs {
			entryArgs = append(entryArgs, id)
		}
	}
	args = append(args, entryArgs...)

	query := "WITH item_matches AS MATERIALIZED (" + itemQuery + "), side_matches AS (" + commentQuery + " UNION " + entryQuery + `),
		search_matches AS (
			SELECT id FROM item_matches
			UNION ALL
			SELECT id FROM side_matches`
	if len(directIDs) > 0 {
		query += ` UNION ALL SELECT i.id FROM items i WHERE i.id IN (` + placeholders(len(directIDs)) + ")"
		for _, id := range directIDs {
			args = append(args, id)
		}
	}
	return query + ") ", args
}

// searchMatches builds one row per item before sorting and paging, without
// computing snippets. Materialization keeps SQLite ranks in their MATCH
// context before the window function picks the best side match: a comment, or
// a note or decision (TASK-4491), which share one band (leg 2). entry_row is
// the item_entries row of an entry match, 0 otherwise. Rank scores are opaque
// values where higher is better on both dialects.
func (s *Store) searchMatches(params SearchParams, directIDs []string, filters []collectionDoneFilter, now time.Time) (string, []interface{}) {
	itemTable, commentTable := "items_fts", "comments_fts"
	itemFrom := "items_fts JOIN items i ON i.rowid = items_fts.rowid"
	commentFrom := "comments_fts JOIN comments cm ON cm.rowid = comments_fts.rowid JOIN items i ON i.id = cm.item_id"
	if s.dialect.Driver() == DriverPostgres {
		itemTable, commentTable = "i", "cm"
		itemFrom = "items i"
		commentFrom = "comments cm JOIN items i ON i.id = cm.item_id"
	}
	itemRank, itemRankArgs := s.searchScoreExpr(params, filters, now)
	itemMatch, itemMatchArgs := s.dialect.FTSMatch(itemTable, "search_vector", params.Query)
	itemQuery := fmt.Sprintf(`SELECT i.id, %s AS rank_score, '' AS comment_id, 0 AS entry_row, 1 AS match_leg
		FROM %s JOIN collections c ON c.id = i.collection_id WHERE %s`,
		itemRank, itemFrom, itemMatch)
	args := append(itemRankArgs, itemMatchArgs...)
	itemQuery, args = s.appendSearchFilters(itemQuery, args, params)
	if len(directIDs) > 0 {
		itemQuery += " AND i.id NOT IN (" + placeholders(len(directIDs)) + ")"
		for _, id := range directIDs {
			args = append(args, id)
		}
	}

	var commentRank string
	var rankArgs []interface{}
	if s.dialect.Driver() == DriverPostgres {
		commentRank, rankArgs = s.dialect.FTSRank(commentTable, "search_vector", params.Query)
	} else {
		commentRank = "(-rank)"
	}
	commentMatch, matchArgs := s.dialect.FTSMatch(commentTable, "search_vector", params.Query)
	commentQuery := fmt.Sprintf(`SELECT i.id, %s AS rank_score, cm.id AS comment_id, 0 AS entry_row, 2 AS match_leg
		FROM %s JOIN collections c ON c.id = i.collection_id
		WHERE %s AND cm.deleted_at IS NULL`, commentRank, commentFrom, commentMatch)
	commentArgs := append(rankArgs, matchArgs...)
	commentQuery, commentArgs = s.appendSearchFilters(commentQuery, commentArgs, params)
	commentQuery += " AND i.id NOT IN (SELECT id FROM item_matches)"
	if len(directIDs) > 0 {
		commentQuery += " AND i.id NOT IN (" + placeholders(len(directIDs)) + ")"
		for _, id := range directIDs {
			commentArgs = append(commentArgs, id)
		}
	}
	args = append(args, commentArgs...)

	entryTable := "item_entries_fts"
	entryFrom := "item_entries_fts JOIN item_entries e ON e.id = item_entries_fts.rowid JOIN items i ON i.id = e.item_id"
	if s.dialect.Driver() == DriverPostgres {
		entryTable = "e"
		entryFrom = "item_entries e JOIN items i ON i.id = e.item_id"
	}
	var entryRank string
	var entryRankArgs []interface{}
	if s.dialect.Driver() == DriverPostgres {
		entryRank, entryRankArgs = s.dialect.FTSRank(entryTable, "search_vector", params.Query)
	} else {
		entryRank = "(-rank)"
	}
	entryMatch, entryMatchArgs := s.dialect.FTSMatch(entryTable, "search_vector", params.Query)
	entryQuery := fmt.Sprintf(`SELECT i.id, %s AS rank_score, '' AS comment_id, e.id AS entry_row, 2 AS match_leg
		FROM %s JOIN collections c ON c.id = i.collection_id
		WHERE %s`, entryRank, entryFrom, entryMatch)
	entryArgs := append(entryRankArgs, entryMatchArgs...)
	entryQuery, entryArgs = s.appendSearchFilters(entryQuery, entryArgs, params)
	entryQuery += " AND i.id NOT IN (SELECT id FROM item_matches)"
	if len(directIDs) > 0 {
		entryQuery += " AND i.id NOT IN (" + placeholders(len(directIDs)) + ")"
		for _, id := range directIDs {
			entryArgs = append(entryArgs, id)
		}
	}
	args = append(args, entryArgs...)

	// A rank tie goes to the comment, then the lowest comment id or entry row.
	query := "WITH item_matches AS MATERIALIZED (" + itemQuery + "), side_candidates AS MATERIALIZED (" + commentQuery + " UNION ALL " + entryQuery + `),
		best_sides AS (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY id ORDER BY rank_score DESC, CASE WHEN comment_id <> '' THEN 0 ELSE 1 END, comment_id, entry_row) AS side_position
			FROM side_candidates
		), search_matches AS (
			SELECT * FROM item_matches
			UNION ALL
			SELECT id, rank_score, comment_id, entry_row, match_leg
			FROM best_sides WHERE side_position = 1`
	if len(directIDs) > 0 {
		// Direct ref/number hits receive a fixed high opaque rank (1e6); higher is better.
		query += ` UNION ALL SELECT i.id, 1000000, '', 0, 0 FROM items i WHERE i.id IN (` + placeholders(len(directIDs)) + ")"
		for _, id := range directIDs {
			args = append(args, id)
		}
	}
	return query + ") ", args
}

// searchPage fixes the ordered page before evaluating any snippets. Each
// snippet CTE is restricted to the selected leg and ids, so at most Limit
// rows need highlighting. SQLite snippets stay inside their FTS MATCH context.
func (s *Store) searchPage(params SearchParams, matches string, args []interface{}, order string) (string, []interface{}) {
	query := matches + `, search_page AS MATERIALIZED (
		SELECT m.* FROM search_matches m JOIN items i ON i.id = m.id` + order +
		fmt.Sprintf(" LIMIT %d OFFSET %d), ", params.Limit, params.Offset)
	itemTable, commentTable := "items_fts", "comments_fts"
	itemFrom := "items_fts JOIN items i ON i.rowid = items_fts.rowid"
	commentFrom := "comments_fts JOIN comments cm ON cm.rowid = comments_fts.rowid"
	tagsColumn := "i.tags"
	if s.dialect.Driver() == DriverPostgres {
		itemTable, commentTable = "i", "cm"
		itemFrom, commentFrom = "items i", "comments cm"
		tagsColumn = "i.tags::text"
	}
	bodySnippet, bodyArgs := s.dialect.FTSSnippet(itemTable, 1, "i.content", params.Query)
	titleSnippet, titleArgs := s.dialect.FTSSnippet(itemTable, 0, "i.title", params.Query)
	tagsSnippet, tagsArgs := s.dialect.FTSSnippet(itemTable, 2, tagsColumn, params.Query)
	// Which column matched comes from these flags, not from "<mark>" in a
	// snippet: stored text can contain that string (TASK-4514).
	bodyMatched, bodyMatchedArgs := s.dialect.FTSColumnMatched(itemTable, 1, "i.content", params.Query)
	titleMatched, titleMatchedArgs := s.dialect.FTSColumnMatched(itemTable, 0, "i.title", params.Query)
	tagsMatched, tagsMatchedArgs := s.dialect.FTSColumnMatched(itemTable, 2, tagsColumn, params.Query)
	itemMatch, itemMatchArgs := s.dialect.FTSMatch(itemTable, "search_vector", params.Query)
	query += fmt.Sprintf(`item_snippets AS MATERIALIZED (
		SELECT i.id, %s AS snippet, %s AS title_snippet, %s AS tags_snippet,
		       %s AS content_matched, %s AS title_matched, %s AS tags_matched
		FROM %s WHERE %s AND i.id IN (SELECT id FROM search_page WHERE match_leg = 1)
	), `, bodySnippet, titleSnippet, tagsSnippet, bodyMatched, titleMatched, tagsMatched, itemFrom, itemMatch)
	args = append(args, bodyArgs...)
	args = append(args, titleArgs...)
	args = append(args, tagsArgs...)
	args = append(args, bodyMatchedArgs...)
	args = append(args, titleMatchedArgs...)
	args = append(args, tagsMatchedArgs...)
	args = append(args, itemMatchArgs...)

	commentSnippet, snippetArgs := s.dialect.FTSSnippet(commentTable, 0, "cm.body", params.Query)
	commentMatch, commentMatchArgs := s.dialect.FTSMatch(commentTable, "search_vector", params.Query)
	query += fmt.Sprintf(`comment_snippets AS MATERIALIZED (
		SELECT cm.id AS comment_id, %s AS snippet FROM %s
		WHERE %s AND cm.deleted_at IS NULL
		AND cm.id IN (SELECT comment_id FROM search_page WHERE match_leg = 2)
	) `, commentSnippet, commentFrom, commentMatch)
	args = append(args, snippetArgs...)
	args = append(args, commentMatchArgs...)

	// Note and decision snippets (TASK-4491): head is column 0, body column 1.
	// The Postgres head flag reads the head as its search vector indexes it
	// (pgmigrations/144: the first 65536 characters).
	entryTable, entryFrom := "item_entries_fts", "item_entries_fts JOIN item_entries e ON e.id = item_entries_fts.rowid"
	headSource := "e.head"
	if s.dialect.Driver() == DriverPostgres {
		entryTable, entryFrom = "e", "item_entries e"
		headSource = "left(e.head, 65536)"
	}
	headSnippet, headArgs := s.dialect.FTSSnippet(entryTable, 0, "e.head", params.Query)
	entryBodySnippet, entryBodyArgs := s.dialect.FTSSnippet(entryTable, 1, "e.body", params.Query)
	headMatched, headMatchedArgs := s.dialect.FTSColumnMatched(entryTable, 0, headSource, params.Query)
	entryMatch, entryMatchArgs := s.dialect.FTSMatch(entryTable, "search_vector", params.Query)
	query += fmt.Sprintf(`, entry_snippets AS MATERIALIZED (
		SELECT e.id AS entry_row, e.item_id, e.kind, e.entry_id, %s AS head_snippet, %s AS body_snippet, %s AS head_matched
		FROM %s WHERE %s
		AND e.id IN (SELECT entry_row FROM search_page WHERE match_leg = 2 AND entry_row <> 0)
	) `, headSnippet, entryBodySnippet, headMatched, entryFrom, entryMatch)
	args = append(args, headArgs...)
	args = append(args, entryBodyArgs...)
	args = append(args, headMatchedArgs...)
	args = append(args, entryMatchArgs...)
	return query, args
}

// appendSearchFilters adds workspace, collection, and field filter clauses to a query.
// Used to keep count and results queries in sync.
func (s *Store) appendSearchFilters(query string, args []interface{}, params SearchParams) (string, []interface{}) {
	if params.Workspace != "" {
		query += ` AND i.workspace_id = (SELECT w.id FROM workspaces w JOIN workspace_slug_claims c ON c.workspace_id = w.id WHERE c.slug = ? AND w.deleted_at IS NULL)`
		args = append(args, params.Workspace)
	} else if len(params.WorkspaceIDs) > 0 {
		query += ` AND i.workspace_id IN (` + placeholders(len(params.WorkspaceIDs)) + `)`
		for _, id := range params.WorkspaceIDs {
			args = append(args, id)
		}
	}

	query, args = appendSearchPermissionFilter(query, args, params)
	query = appendLiveItemFilter(query, params)
	query = appendLiveCollectionFilter(query)

	query, args = appendSearchCollectionFilter(query, args, params)

	for key, value := range params.FieldFilters {
		if !validFieldKey.MatchString(key) {
			continue
		}
		cond, condArgs := s.dialect.JSONFieldEquals("i.fields", key, value) // BUG-3221
		query += ` AND ` + cond
		args = append(args, condArgs...)
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
		// rank_score is higher-is-better on both SQLite and PostgreSQL.
		return " ORDER BY rank_score DESC, i.id"
	}
}

// searchFacets counts the full, unpaginated FTS result set without computing scores.
func (s *Store) searchFacets(params SearchParams) *SearchFacets {
	matches, args := s.searchMatchIDs(params, nil)
	return s.searchFacetsForMatches(params, matches, args)
}

func (s *Store) searchFacetsForMatches(params SearchParams, matches string, args []interface{}) *SearchFacets {
	statusExtract := s.dialect.JSONFieldText("i.fields", "status")
	baseQuery := ` FROM search_matches m JOIN items i ON i.id = m.id JOIN collections c ON c.id = i.collection_id `
	facets := &SearchFacets{Collections: make(map[string]int), Statuses: make(map[string]int)}
	collRows, err := s.db.Query(s.q(matches+"SELECT c.slug, COUNT(*)"+baseQuery+"GROUP BY c.slug"), args...)
	if err != nil {
		return nil
	}
	for collRows.Next() {
		var slug string
		var count int
		if err := collRows.Scan(&slug, &count); err != nil {
			collRows.Close()
			return nil
		}
		facets.Collections[slug] = count
	}
	if err := collRows.Err(); err != nil {
		collRows.Close()
		return nil
	}
	collRows.Close()
	statusQuery := fmt.Sprintf("SELECT %s, COUNT(*) %s WHERE %s IS NOT NULL GROUP BY %s", statusExtract, baseQuery, statusExtract, statusExtract)
	statusRows, err := s.db.Query(s.q(matches+statusQuery), args...)
	if err != nil {
		return nil
	}
	for statusRows.Next() {
		var status string
		var count int
		if err := statusRows.Scan(&status, &count); err != nil {
			statusRows.Close()
			return nil
		}
		if status != "" {
			facets.Statuses[status] = count
		}
	}
	if err := statusRows.Err(); err != nil {
		statusRows.Close()
		return nil
	}
	statusRows.Close()
	if params.Workspace == "" {
		// A failed workspace count omits only that facet; the collection and
		// status facets above stand.
		facets.WorkspaceCounts = s.searchWorkspaceCounts(matches+"SELECT i.workspace_id, COUNT(*)"+baseQuery+"GROUP BY i.workspace_id", args)
	}
	return facets
}

// searchWorkspaceCounts maps workspace ID to its count of matches, nil on any
// error. Zero counts are never present.
func (s *Store) searchWorkspaceCounts(query string, args []interface{}) map[string]int {
	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var wsID string
		var count int
		if err := rows.Scan(&wsID, &count); err != nil {
			return nil
		}
		if wsID != "" && count > 0 {
			counts[wsID] = count
		}
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	return counts
}

// sanitizeFTSQuery wraps each token in double quotes so FTS5 treats
// special characters (like hyphens) as literals rather than operators.
// The final token gains a prefix marker when its final part is long enough.
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
	if _, part := splitFTSPrefix(q); part != "" {
		tokens[len(tokens)-1] += "*"
	}
	return strings.Join(tokens, " ")
}

// maxFTSPrefixBytes is the smallest operand, in bytes, that a Postgres
// tsquery refuses.
const maxFTSPrefixBytes = 2047

// splitFTSPrefix separates the final alphanumeric part from the whole-word
// head. An empty part disables prefix matching. Only letters and decimal
// digits can enter the prefix tsquery; punctuation in the head goes to
// plainto_tsquery. Other number runes (superscripts, fractions) split the
// part, because the Postgres parser drops them and would otherwise stem "m²"
// into a one-letter prefix.
func splitFTSPrefix(q string) (head, part string) {
	words := strings.Fields(q)
	if len(words) == 0 {
		return "", ""
	}
	parts := strings.FieldsFunc(words[len(words)-1], func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(parts) == 0 {
		return "", ""
	}
	part = parts[len(parts)-1]
	// Postgres rejects a tsquery operand of maxFTSPrefixBytes or more with
	// an error, and never indexes a word that long, so such a prefix could
	// match nothing. Both engines skip it to keep their behaviour the same.
	if utf8.RuneCountInString(part) < 2 || len(part) >= maxFTSPrefixBytes {
		return "", ""
	}
	head = strings.Join(append(words[:len(words)-1], parts[:len(parts)-1]...), " ")
	return head, part
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

// appendSearchCollectionFilter adds the collection content filter to a search
// query. A resolved CollectionFilterIDs set (BUG-2659) wins; a bare Collection
// slug is the literal match callers inside the store still use.
// appendSearchPermissionFilter adds the permission predicate every search
// query path shares: the ref lookup, the number lookup, the FTS query, and the
// count and facet queries (BUG-3331). An item matches if its collection is
// visible or it is granted individually. With neither set and Unrestricted
// unset it adds a predicate that matches nothing, so a zero-visibility caller
// can never read as unfiltered, even on a path that skipped Search's early
// return.
func appendSearchPermissionFilter(query string, args []interface{}, params SearchParams) (string, []interface{}) {
	colls, items := params.CollectionIDs, params.ItemIDs
	switch {
	case len(colls) > 0 && len(items) > 0:
		query += ` AND (i.collection_id IN (` + placeholders(len(colls)) + `) OR i.id IN (` + placeholders(len(items)) + `))`
	case len(colls) > 0:
		query += ` AND i.collection_id IN (` + placeholders(len(colls)) + `)`
	case len(items) > 0:
		query += ` AND i.id IN (` + placeholders(len(items)) + `)`
	case params.Unrestricted:
		return query, args
	default:
		return query + ` AND 1 = 0`, args
	}
	for _, id := range colls {
		args = append(args, id)
	}
	for _, id := range items {
		args = append(args, id)
	}
	return query, args
}

// appendLiveItemFilter leaves out soft-deleted items unless the caller asked
// for them (TASK-2864). Called beside appendLiveCollectionFilter on every
// query path, so each one applies the same archived rule.
func appendLiveItemFilter(query string, params SearchParams) string {
	if params.IncludeArchived {
		return query
	}
	return query + ` AND i.deleted_at IS NULL`
}

// appendLiveCollectionFilter leaves out the items of a soft-deleted collection
// (BUG-3428), for every caller alike. Every search query joins collections as
// c and calls it beside appendSearchPermissionFilter, so the results, the
// total and the facets agree.
func appendLiveCollectionFilter(query string) string {
	return query + ` AND c.deleted_at IS NULL`
}

func appendSearchCollectionFilter(query string, args []interface{}, params SearchParams) (string, []interface{}) {
	if params.CollectionFilterIDs != nil {
		if len(params.CollectionFilterIDs) == 0 {
			return query + ` AND 1 = 0`, args
		}
		query += ` AND i.collection_id IN (` + placeholders(len(params.CollectionFilterIDs)) + `)`
		for _, id := range params.CollectionFilterIDs {
			args = append(args, id)
		}
		return query, args
	}
	if params.Collection != "" {
		query += ` AND c.slug = ?`
		args = append(args, params.Collection)
	}
	return query, args
}

// plainTagsSnippet renders a tags snippet as a plain list, `<mark>x</mark>, y`,
// rather than the stored JSON array text it was cut from (`["<mark>x</mark>","y"]`
// on SQLite, `["<mark>x</mark>", "y"]` on Postgres). An FTS ellipsis at either
// end of a cut snippet is kept, and the JSON escapes \" and \\ inside a tag are
// undone. The stored tags are not touched.
func plainTagsSnippet(snippet string) string {
	s, lead, trail := snippet, "", ""
	if strings.HasPrefix(s, "...") {
		lead, s = "...", s[3:]
	}
	if strings.HasSuffix(s, "...") {
		trail, s = "...", s[:len(s)-3]
	}
	// A bracket is the array's only at an end the snippet did not cut.
	if lead == "" {
		s = strings.TrimPrefix(s, "[")
	}
	if trail == "" {
		s = strings.TrimSuffix(s, "]")
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			if i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
				i++
				b.WriteByte(s[i])
			} else {
				b.WriteByte(c)
			}
		case '"':
			// An unescaped quote either closes a tag before the next one or
			// opens or closes the list; only the separator is shown.
			j := i + 1
			for j < len(s) && s[j] == ' ' {
				j++
			}
			if j < len(s) && s[j] == ',' {
				j++
				for j < len(s) && s[j] == ' ' {
					j++
				}
				if j < len(s) && s[j] == '"' {
					b.WriteString(", ")
					i = j
				}
			}
		default:
			b.WriteByte(c)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return lead + b.String() + trail
}
