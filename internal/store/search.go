package store

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// validFieldKey matches safe JSON field keys: alphanumeric, underscores, hyphens only.
var validFieldKey = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

type SearchResult struct {
	Item      models.Item `json:"item"`
	Snippet   string      `json:"snippet"`
	Rank      float64     `json:"rank"`
	MatchedIn string      `json:"matched_in"`
	CommentID string      `json:"comment_id,omitempty"`
	// Workspace says which workspace the item is in, so a search across
	// workspaces can be navigated (TASK-3604). The handler fills it from the
	// same access check that admitted the workspace into the search; it is
	// never derived from the row alone.
	Workspace *SearchResultWorkspace `json:"workspace,omitempty"`
}

// SearchResultWorkspace is the route a result lives under: /{owner}/{slug}.
type SearchResultWorkspace struct {
	Slug          string `json:"slug"`
	OwnerUsername string `json:"owner_username"`
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
	matches, args := s.searchMatches(params, directHitIDs)
	var total int
	if err := s.db.QueryRow(s.q(matches+" SELECT COUNT(*) FROM search_matches"), args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("search count: %w", err)
	}
	facets := s.searchFacetsForMatches(matches, args)

	// Keep the ranks of the two indexes separate. Explicit item sorting still
	// applies within each leg, with the existing item id tie-breaker.
	order := " ORDER BY m.match_leg, CASE WHEN m.match_leg = 0 THEN i.workspace_id ELSE '' END, CASE WHEN m.match_leg = 0 THEN i.id ELSE '' END, " + strings.TrimPrefix(s.searchOrderClause(params), " ORDER BY ")
	page, args := s.searchPage(params, matches, args, order)
	query := page + `
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, ` + contentStateSQL + `, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.seq, i.created_at, i.updated_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, ''), i.deleted_at,
		       CASE m.match_leg WHEN 0 THEN i.title WHEN 1 THEN its.snippet ELSE cms.snippet END,
		       COALESCE(its.title_snippet, ''), COALESCE(its.tags_snippet, ''), m.rank_score, m.comment_id, m.match_leg
		FROM search_page m
		JOIN items i ON i.id = m.id
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		LEFT JOIN item_snippets its ON its.id = m.id
		LEFT JOIN comment_snippets cms ON cms.comment_id = m.comment_id
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
		var leg int
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
		); err != nil {
			return nil, err
		}
		switch {
		case leg == 0:
			r.MatchedIn = "ref"
		case leg == 2:
			r.MatchedIn = "comment"
		case strings.Contains(r.Snippet, "<mark>"):
			r.MatchedIn = "content"
		case strings.Contains(titleSnippet, "<mark>"):
			r.MatchedIn, r.Snippet = "title", titleSnippet
		case strings.Contains(tagsSnippet, "<mark>"):
			r.MatchedIn, r.Snippet = "tags", tagsSnippet
		default:
			r.MatchedIn = "content"
		}
		r.Item.Pinned = pinned
		r.Item.CreatedAt = parseTime(createdAt)
		r.Item.UpdatedAt = parseTime(updatedAt)
		r.Item.DeletedAt = parseTimePtr(deletedAt)
		hydrateItemComputedMetadata(&r.Item)
		r.Item.Content = ""
		// Only a content snippet is body text. Ref, title, tags and comment
		// snippets must not carry the body's staleness marker.
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

// searchMatches builds one row per item before sorting, paging, counting or
// faceting, without computing snippets. Materialization keeps SQLite ranks in
// their MATCH context before the window function picks the best comment.
func (s *Store) searchMatches(params SearchParams, directIDs []string) (string, []interface{}) {
	itemTable, commentTable := "items_fts", "comments_fts"
	itemFrom := "items_fts JOIN items i ON i.rowid = items_fts.rowid"
	commentFrom := "comments_fts JOIN comments cm ON cm.rowid = comments_fts.rowid JOIN items i ON i.id = cm.item_id"
	if s.dialect.Driver() == DriverPostgres {
		itemTable, commentTable = "i", "cm"
		itemFrom = "items i"
		commentFrom = "comments cm JOIN items i ON i.id = cm.item_id"
	}
	itemRank, itemRankArgs := s.dialect.FTSRank(itemTable, "search_vector", params.Query)
	itemMatch, itemMatchArgs := s.dialect.FTSMatch(itemTable, "search_vector", params.Query)
	itemQuery := fmt.Sprintf(`SELECT i.id, %s AS rank_score, '' AS comment_id, 1 AS match_leg
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

	commentRank, rankArgs := s.dialect.FTSRank(commentTable, "search_vector", params.Query)
	commentMatch, matchArgs := s.dialect.FTSMatch(commentTable, "search_vector", params.Query)
	commentQuery := fmt.Sprintf(`SELECT i.id, %s AS rank_score, cm.id AS comment_id, 2 AS match_leg
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
	direction := "ASC"
	if s.dialect.Driver() == DriverPostgres {
		direction = "DESC"
	}
	query := "WITH item_matches AS MATERIALIZED (" + itemQuery + "), comment_candidates AS MATERIALIZED (" + commentQuery + `),
		best_comments AS (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY id ORDER BY rank_score ` + direction + `, comment_id) AS comment_position
			FROM comment_candidates
		), search_matches AS (
			SELECT * FROM item_matches
			UNION ALL
			SELECT id, rank_score, comment_id, match_leg
			FROM best_comments WHERE comment_position = 1`
	if len(directIDs) > 0 {
		query += ` UNION ALL SELECT i.id, -1000, '', 0 FROM items i WHERE i.id IN (` + placeholders(len(directIDs)) + ")"
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
	itemMatch, itemMatchArgs := s.dialect.FTSMatch(itemTable, "search_vector", params.Query)
	query += fmt.Sprintf(`item_snippets AS MATERIALIZED (
		SELECT i.id, %s AS snippet, %s AS title_snippet, %s AS tags_snippet
		FROM %s WHERE %s AND i.id IN (SELECT id FROM search_page WHERE match_leg = 1)
	), `, bodySnippet, titleSnippet, tagsSnippet, itemFrom, itemMatch)
	args = append(args, bodyArgs...)
	args = append(args, titleArgs...)
	args = append(args, tagsArgs...)
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
	return query, args
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
		// SQLite bm25() returns negative values (more negative = more relevant) → ASC.
		// PostgreSQL ts_rank() returns positive values (higher = more relevant) → DESC.
		if s.dialect.Driver() == DriverPostgres {
			return " ORDER BY rank_score DESC, i.id"
		}
		return " ORDER BY rank_score, i.id"
	}
}

// searchFacets counts the full, unpaginated FTS result set.
func (s *Store) searchFacets(params SearchParams) *SearchFacets {
	matches, args := s.searchMatches(params, nil)
	return s.searchFacetsForMatches(matches, args)
}

func (s *Store) searchFacetsForMatches(matches string, args []interface{}) *SearchFacets {
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
	defer statusRows.Close()
	for statusRows.Next() {
		var status string
		var count int
		if err := statusRows.Scan(&status, &count); err != nil {
			return nil
		}
		if status != "" {
			facets.Statuses[status] = count
		}
	}
	if err := statusRows.Err(); err != nil {
		return nil
	}
	return facets
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
