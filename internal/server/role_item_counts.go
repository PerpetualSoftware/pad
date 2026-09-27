package server

import (
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// visibleRoleItemCounts counts, per agent role, the items the caller may see.
// restricted is false when visibility is unrestricted (visibleCollectionIDs
// answered nil), and the caller keeps the store's workspace-wide count.
//
// It is the one rule for a role's item_count on every door that serves one:
// GET /agent-roles and each lane of GET /roles/board (BUG-3257, where the
// board served the workspace-wide count to a guest and to a collection-
// restricted member). The role list itself is not filtered: roles are
// workspace metadata any reader may see, and only the count describes items.
func (s *Server) visibleRoleItemCounts(r *http.Request, workspaceID string) (counts map[string]int, restricted bool, err error) {
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		return nil, false, err
	}
	if visibleIDs == nil {
		return nil, false, nil
	}
	// For users with item-level grants, use item-level filtering.
	collIDs := visibleIDs
	var itemIDs []string
	fullCollIDs, grantedItemIDs, err := s.guestResourceFilter(r, workspaceID)
	if err != nil {
		return nil, false, err
	}
	if len(grantedItemIDs) > 0 {
		collIDs = fullCollIDs
		itemIDs = grantedItemIDs
	}
	items, err := s.store.ListItems(workspaceID, models.ItemListParams{CollectionIDs: collIDs, ItemIDs: itemIDs})
	if err != nil {
		return nil, false, err
	}
	counts = make(map[string]int)
	for _, item := range items {
		if item.AgentRoleID != nil && *item.AgentRoleID != "" {
			counts[*item.AgentRoleID]++
		}
	}
	return counts, true, nil
}
