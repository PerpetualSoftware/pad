package server

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

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

	// When no specific workspace is given, scope search to the user's
	// workspaces so results never leak across workspace boundaries.
	if params.Workspace == "" {
		user := currentUser(r)
		if user != nil {
			workspaces, err := s.store.GetUserWorkspaces(user.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal_error", "Failed to resolve user workspaces")
				return
			}
			// OAuth consent scoping (BUG-2102): /search is workspace-global
			// when no workspace is given — it fans out over EVERY membership.
			// RequireWorkspaceAccess never runs here, so a consent-scoped
			// token would otherwise get item titles + content across
			// workspaces it wasn't granted. Restrict the fan-out to the
			// allow-list. No-op for PAT / web session (nil allow-list).
			workspaces = filterWorkspacesByTokenAllowlist(r.Context(), workspaces)
			for _, ws := range workspaces {
				params.WorkspaceIDs = append(params.WorkspaceIDs, ws.ID)
			}
			// If user has no workspaces, return empty results
			if len(params.WorkspaceIDs) == 0 {
				writeJSON(w, http.StatusOK, &store.SearchResponse{
					Results: []store.SearchResult{},
					Limit:   params.Limit,
					Offset:  params.Offset,
				})
				return
			}
			// Apply per-workspace collection visibility filtering.
			// Collect all visible collection IDs across user's workspaces.
			// For guest workspaces, also collect item-level grants.
			allVisibleCollIDs := []string{} // non-nil empty = "no access" by default
			var allVisibleItemIDs []string
			needsCollFilter := false
			for _, ws := range workspaces {
				if ws.IsGuest {
					// Guest workspace: use item-level filtering
					fullCollIDs, grantedItemIDs, grantErr := s.store.GuestVisibleResources(ws.ID, user.ID)
					if grantErr != nil {
						params.WorkspaceIDs = removeString(params.WorkspaceIDs, ws.ID)
						continue
					}
					needsCollFilter = true
					allVisibleCollIDs = append(allVisibleCollIDs, fullCollIDs...)
					allVisibleItemIDs = append(allVisibleItemIDs, grantedItemIDs...)
					continue
				}
				visIDs, err := s.store.VisibleCollectionIDs(ws.ID, user.ID)
				if err != nil {
					params.WorkspaceIDs = removeString(params.WorkspaceIDs, ws.ID)
					continue
				}
				if visIDs != nil {
					needsCollFilter = true
					// For restricted members with item grants, separate full-access
					// collections from item-granted collections
					_, itemGrants, _ := s.store.GuestVisibleResources(ws.ID, user.ID)
					if len(itemGrants) > 0 {
						memberColls, _ := s.store.GetMemberCollectionAccess(ws.ID, user.ID)
						sysColls, _ := s.store.ListSystemCollectionIDs(ws.ID)
						collGrants, _, _ := s.store.GuestVisibleResources(ws.ID, user.ID)
						fullSet := make(map[string]bool)
						for _, id := range memberColls {
							fullSet[id] = true
						}
						for _, id := range sysColls {
							fullSet[id] = true
						}
						for _, id := range collGrants {
							fullSet[id] = true
						}
						for id := range fullSet {
							allVisibleCollIDs = append(allVisibleCollIDs, id)
						}
						allVisibleItemIDs = append(allVisibleItemIDs, itemGrants...)
					} else {
						allVisibleCollIDs = append(allVisibleCollIDs, visIDs...)
					}
				} else {
					// "all" access — include all collections from this workspace
					colls, _ := s.store.ListCollections(ws.ID)
					for _, c := range colls {
						allVisibleCollIDs = append(allVisibleCollIDs, c.ID)
					}
				}
			}
			if needsCollFilter {
				params.CollectionIDs = allVisibleCollIDs
			}
			if len(allVisibleItemIDs) > 0 {
				params.ItemIDs = allVisibleItemIDs
			}
		}
		// If no user (fresh install, no auth), allow unscoped search
	}

	// Apply collection visibility filter when searching a specific workspace
	if params.Workspace != "" {
		ws, _ := s.store.GetWorkspaceBySlug(params.Workspace)
		if ws != nil {
			// OAuth consent scoping (BUG-2102): a token consented to specific
			// workspaces must not read another one's item content by naming it
			// here, even a co-membership. Return empty (not 403) so the token
			// can't confirm the workspace exists. No-op for PAT / web session.
			if !tokenAllowedWorkspaceMatches(r.Context(), ws.Slug) {
				writeJSON(w, http.StatusOK, &store.SearchResponse{
					Results: []store.SearchResult{},
					Limit:   params.Limit,
					Offset:  params.Offset,
				})
				return
			}
			user := currentUser(r)
			visibleIDs, visErr := s.visibleCollectionIDs(r, ws.ID)
			if visErr != nil {
				writeInternalError(w, visErr)
				return
			}
			params.CollectionIDs = visibleIDs

			// For users with item grants (guests or restricted members),
			// apply item-level filtering so item grants don't leak entire
			// collections in search results.
			// Note: /search is not behind RequireWorkspaceAccess, so we
			// can't use guestResourceFilter (needs workspaceRole). We check
			// membership and collection access directly.
			if user != nil {
				needsItemFilter := false
				member, _ := s.store.GetWorkspaceMember(ws.ID, user.ID)
				if member == nil {
					// Guest (non-member)
					needsItemFilter = true
				} else if member.CollectionAccess == "specific" {
					// Restricted member — check if they have item grants
					_, itemGrants, _ := s.store.GuestVisibleResources(ws.ID, user.ID)
					needsItemFilter = len(itemGrants) > 0
				}

				if needsItemFilter {
					grantCollIDs, grantedItemIDs, grantErr := s.store.GuestVisibleResources(ws.ID, user.ID)
					if grantErr != nil {
						writeInternalError(w, grantErr)
						return
					}
					// For restricted members, merge member collections into full access set
					fullCollIDs := grantCollIDs
					if member != nil {
						memberColls, _ := s.store.GetMemberCollectionAccess(ws.ID, user.ID)
						sysColls, _ := s.store.ListSystemCollectionIDs(ws.ID)
						fullSet := make(map[string]bool)
						for _, id := range grantCollIDs {
							fullSet[id] = true
						}
						for _, id := range memberColls {
							fullSet[id] = true
						}
						for _, id := range sysColls {
							fullSet[id] = true
						}
						fullCollIDs = make([]string, 0, len(fullSet))
						for id := range fullSet {
							fullCollIDs = append(fullCollIDs, id)
						}
					}
					params.CollectionIDs = fullCollIDs
					params.ItemIDs = grantedItemIDs
				}
			}
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

func removeString(ss []string, s string) []string {
	result := ss[:0]
	for _, v := range ss {
		if v != s {
			result = append(result, v)
		}
	}
	return result
}
