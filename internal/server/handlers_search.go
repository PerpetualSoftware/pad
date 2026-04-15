package server

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/xarmian/pad/internal/store"
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
			for _, ws := range workspaces {
				params.WorkspaceIDs = append(params.WorkspaceIDs, ws.ID)
			}
			// If user has no workspaces, return empty results
			if len(params.WorkspaceIDs) == 0 {
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"results": []store.SearchResult{},
					"total":   0,
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

	results, err := s.store.Search(params)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if results == nil {
		results = []store.SearchResult{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results": results,
		"total":   len(results),
	})
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
