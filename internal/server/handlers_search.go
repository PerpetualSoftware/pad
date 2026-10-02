package server

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// safeFieldKey matches safe JSON field keys: starts with a letter, alphanumeric/underscore/hyphen.
var safeFieldKey = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Query parameter 'q' is required")
		return
	}

	params := store.SearchParams{
		Query:      query,
		Workspace:  r.URL.Query().Get("workspace"),
		Collection: r.URL.Query().Get("collection"),
	}

	// Parse field filters: status, priority as top-level params,
	// plus generic field.* params (e.g. field.category=backend).
	fieldFilters := make(map[string]string)
	if v := r.URL.Query().Get("status"); v != "" {
		fieldFilters["status"] = v
	}
	if v := r.URL.Query().Get("priority"); v != "" {
		fieldFilters["priority"] = v
	}
	for key, values := range r.URL.Query() {
		if strings.HasPrefix(key, "field.") && len(values) > 0 && values[0] != "" {
			fieldKey := strings.TrimPrefix(key, "field.")
			if !safeFieldKey.MatchString(fieldKey) {
				continue // skip keys with unsafe characters
			}
			fieldFilters[fieldKey] = values[0]
		}
	}
	if len(fieldFilters) > 0 {
		params.FieldFilters = fieldFilters
	}

	// Parse pagination params
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Offset = n
		}
	}

	// Parse sort params
	params.Sort = r.URL.Query().Get("sort")
	params.Order = r.URL.Query().Get("order")

	// Normalize defaults early so early-return paths have correct values.
	params.Normalize()

	empty := func() {
		writeJSON(w, http.StatusOK, &store.SearchResponse{
			Results: []store.SearchResult{},
			Limit:   params.Limit,
			Offset:  params.Offset,
		})
	}

	// /search is not behind RequireWorkspaceAccess, so it decides access
	// itself, through searchAccessFor, which reaches the same answer that
	// middleware would for each workspace (BUG-3331). A workspace the caller
	// cannot read contributes nothing; the answer is an empty result, never a
	// 403, so a slug's existence is not confirmed (the BUG-2102 shape).
	if params.Workspace != "" {
		ws, err := s.store.GetWorkspaceBySlug(params.Workspace)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if ws == nil {
			empty()
			return
		}
		// OAuth consent scoping (BUG-2102), checked here as well as in
		// searchAccessFor so the enforcement is visible at the handler.
		if !tokenAllowedWorkspaceMatches(r.Context(), ws.Slug) {
			empty()
			return
		}
		access, err := s.searchAccessFor(r, ws)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if access.none() {
			empty()
			return
		}
		params.Unrestricted = access.unrestricted
		params.CollectionIDs = access.collectionIDs
		params.ItemIDs = access.itemIDs
	} else {
		workspaces, err := s.searchFanOutWorkspaces(r)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		allUnrestricted := true
		for _, ws := range workspaces {
			access, err := s.searchAccessFor(r, ws)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			if access.none() {
				continue
			}
			params.WorkspaceIDs = append(params.WorkspaceIDs, ws.ID)
			if access.unrestricted {
				// Collection IDs are unique across workspaces, so a full-access
				// workspace joins the union as all of its collections.
				colls, err := s.store.ListCollections(ws.ID)
				if err != nil {
					writeInternalError(w, err)
					return
				}
				for _, c := range colls {
					params.CollectionIDs = append(params.CollectionIDs, c.ID)
				}
				continue
			}
			allUnrestricted = false
			params.CollectionIDs = append(params.CollectionIDs, access.collectionIDs...)
			params.ItemIDs = append(params.ItemIDs, access.itemIDs...)
		}
		if len(params.WorkspaceIDs) == 0 {
			empty()
			return
		}
		if allUnrestricted {
			// Every workspace in scope is fully visible: scope by workspace
			// alone, as the named-workspace path does for full access.
			params.Unrestricted = true
			params.CollectionIDs = nil
			params.ItemIDs = nil
		}
	}

	// BUG-2659: resolve the collection filter the way the item handlers
	// resolve a collection (exact match first, then the singular/alias
	// fallback, an archived name still claiming itself), once per workspace
	// in scope. A literal `c.slug = ?` answered a shorthand with nothing, so
	// clients normalised before sending, which let `tasks` shadow a real
	// collection named `task`.
	if params.Collection != "" {
		ids, rerr := s.resolveSearchCollectionFilter(params)
		if rerr != nil {
			writeInternalError(w, rerr)
			return
		}
		if len(ids) == 0 {
			writeJSON(w, http.StatusOK, &store.SearchResponse{
				Results: []store.SearchResult{},
				Limit:   params.Limit,
				Offset:  params.Offset,
			})
			return
		}
		params.CollectionFilterIDs = ids
		params.Collection = ""
	}

	resp, err := s.store.Search(params)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// resolveSearchCollectionFilter resolves params.Collection in every workspace
// the search is scoped to, and returns the matched collection IDs (at most one
// per workspace). The scope mirrors the store's: a named workspace, else the
// caller's scoped workspace IDs, else (no user: the fresh-install window)
// every workspace. Resolution does not consult visibility; the permission
// filter the store applies alongside it still does, so resolving to a
// collection the caller cannot see yields no rows rather than another
// collection's.
func (s *Server) resolveSearchCollectionFilter(params store.SearchParams) ([]string, error) {
	var wsIDs []string
	switch {
	case params.Workspace != "":
		ws, err := s.store.GetWorkspaceBySlug(params.Workspace)
		if err != nil {
			return nil, err
		}
		if ws != nil {
			wsIDs = []string{ws.ID}
		}
	case len(params.WorkspaceIDs) > 0:
		wsIDs = params.WorkspaceIDs
	default:
		all, err := s.store.ListWorkspaces()
		if err != nil {
			return nil, err
		}
		for _, ws := range all {
			wsIDs = append(wsIDs, ws.ID)
		}
	}
	ids := []string{}
	for _, wsID := range wsIDs {
		coll, err := s.resolveItemCollectionSlug(wsID, params.Collection)
		if err != nil {
			return nil, err
		}
		if coll != nil {
			ids = append(ids, coll.ID)
		}
	}
	return ids, nil
}

// searchAccess is what one caller may read in one workspace through /search
// (BUG-3331): everything, nothing, or the union of whole collections and
// individually granted items.
type searchAccess struct {
	unrestricted  bool
	collectionIDs []string
	itemIDs       []string
}

func (a searchAccess) none() bool {
	return !a.unrestricted && len(a.collectionIDs) == 0 && len(a.itemIDs) == 0
}

// searchAccessFor decides what the caller may read in ws, reaching the same
// answer RequireWorkspaceAccess and the item routes' visibility filters reach,
// because /search is mounted outside that middleware and must not be a wider
// door than the item routes:
//
//   - The fresh-install window (no users at all) reads everything.
//   - A request with no user is a legacy workspace-scoped token: its own
//     workspace in full, nothing else.
//   - A token whose consent allow-list excludes ws reads nothing (BUG-2102).
//   - A platform admin on a cookie session reads everything; on a bearer, the
//     admin is an ordinary user and only membership counts (BUG-1616), not
//     even a guest grant.
//   - A member with "all" collection access reads everything; a restricted
//     member reads their collections, the system collections, and anything
//     granted to them.
//   - A non-member reads exactly what is granted to them, which with no
//     grants is nothing.
func (s *Server) searchAccessFor(r *http.Request, ws *models.Workspace) (searchAccess, error) {
	user := currentUser(r)
	if user == nil {
		count, err := s.store.UserCount()
		if err != nil {
			return searchAccess{}, err
		}
		if count == 0 {
			return searchAccess{unrestricted: true}, nil
		}
		if tokenWsID := tokenWorkspaceID(r); tokenWsID != "" && tokenWsID == ws.ID {
			return searchAccess{unrestricted: true}, nil
		}
		return searchAccess{}, nil
	}
	if !tokenAllowedWorkspaceMatches(r.Context(), ws.Slug) {
		return searchAccess{}, nil
	}
	bearer := isBearerAuth(r)
	if user.Role == "admin" && !bearer {
		return searchAccess{unrestricted: true}, nil
	}
	member, err := s.store.GetWorkspaceMember(ws.ID, user.ID)
	if err != nil {
		return searchAccess{}, err
	}
	if member == nil {
		if user.Role == "admin" && bearer {
			return searchAccess{}, nil
		}
		colls, items, err := s.store.GuestVisibleResources(ws.ID, user.ID)
		if err != nil {
			return searchAccess{}, err
		}
		return searchAccess{collectionIDs: colls, itemIDs: items}, nil
	}
	if member.CollectionAccess == "all" || member.CollectionAccess == "" {
		return searchAccess{unrestricted: true}, nil
	}
	grantColls, itemGrants, err := s.store.GuestVisibleResources(ws.ID, user.ID)
	if err != nil {
		return searchAccess{}, err
	}
	memberColls, err := s.store.GetMemberCollectionAccess(ws.ID, user.ID)
	if err != nil {
		return searchAccess{}, err
	}
	sysColls, err := s.store.ListSystemCollectionIDs(ws.ID)
	if err != nil {
		return searchAccess{}, err
	}
	seen := map[string]bool{}
	var colls []string
	for _, group := range [][]string{memberColls, sysColls, grantColls} {
		for _, id := range group {
			if !seen[id] {
				seen[id] = true
				colls = append(colls, id)
			}
		}
	}
	return searchAccess{collectionIDs: colls, itemIDs: itemGrants}, nil
}

// searchFanOutWorkspaces lists the workspaces an unscoped search covers: the
// caller's memberships and guest workspaces, narrowed to a token's consent
// allow-list (BUG-2102); for a legacy workspace token with no user, only its
// own workspace; in the fresh-install window, every workspace.
func (s *Server) searchFanOutWorkspaces(r *http.Request) ([]*models.Workspace, error) {
	user := currentUser(r)
	if user == nil {
		count, err := s.store.UserCount()
		if err != nil {
			return nil, err
		}
		if count == 0 {
			all, err := s.store.ListWorkspaces()
			if err != nil {
				return nil, err
			}
			out := make([]*models.Workspace, 0, len(all))
			for i := range all {
				out = append(out, &all[i])
			}
			return out, nil
		}
		tokenWsID := tokenWorkspaceID(r)
		if tokenWsID == "" {
			return nil, nil
		}
		ws, err := s.store.GetWorkspaceByID(tokenWsID)
		if err != nil || ws == nil {
			return nil, err
		}
		return []*models.Workspace{ws}, nil
	}
	memberships, err := s.store.GetUserWorkspaces(user.ID)
	if err != nil {
		return nil, err
	}
	memberships = filterWorkspacesByTokenAllowlist(r.Context(), memberships)
	out := make([]*models.Workspace, 0, len(memberships))
	for i := range memberships {
		out = append(out, &memberships[i])
	}
	return out, nil
}
