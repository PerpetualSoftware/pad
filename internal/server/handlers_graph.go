package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// GraphNode is one item in the workspace graph
// (GET /workspaces/{ws}/graph — PLAN-1730 / TASK-1731). Nodes are
// keyed by ref (TASK-5) rather than UUID: refs are what the web UI's
// force-graph uses for display, deep links, and edge endpoints.
type GraphNode struct {
	Ref        string    `json:"ref"`
	Title      string    `json:"title"`
	Collection string    `json:"collection"`
	Status     string    `json:"status,omitempty"`
	IsTerminal bool      `json:"is_terminal"`
	ChildCount int       `json:"child_count"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// GraphEdge is one typed relationship between two graph nodes. Source
// and target are refs. Type is one of 'parent' | 'blocks' |
// 'implements' | 'related' | 'wiki-link'. For 'parent', source is the
// child and target is the parent (matching item_links semantics); for
// 'blocks', source blocks target.
type GraphEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Type   string `json:"type"`
}

// GraphResponse is the payload of GET /workspaces/{ws}/graph — the
// whole workspace as {nodes, edges} in one call, feeding the 3D graph
// view (PLAN-1730).
type GraphResponse struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// handleGetWorkspaceGraph serves GET /api/v1/workspaces/{ws}/graph.
//
// Query params:
//   - include_terminal=true — include items in terminal status
//     (done/fixed/archived/...). Default false: the graph view opens
//     showing active work only; large workspaces would otherwise be
//     hairball soup.
//
// Visibility follows the dashboard model exactly: collection-level
// visibility via visibleCollectionIDs, item-level guest grants via
// guestResourceFilter, both pushed into ListItems. Edges are then
// filtered to endpoints present in the visible node set, so a guest
// can't infer the existence of items they can't see from dangling
// edge endpoints.
func (s *Server) handleGetWorkspaceGraph(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	includeTerminal := r.URL.Query().Get("include_terminal") == "true"

	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	fullCollIDs, grantedItemIDs, err := s.guestResourceFilter(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	collIDs := visibleIDs
	var itemIDs []string
	if len(grantedItemIDs) > 0 {
		collIDs = fullCollIDs
		itemIDs = grantedItemIDs
	}

	items, err := s.store.ListItems(workspaceID, models.ItemListParams{CollectionIDs: collIDs, ItemIDs: itemIDs})
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Done-context map from ALL workspace collections (not the
	// visibility-filtered set) so item-level grants from hidden
	// collections still evaluate against their own done rules — same
	// reasoning as buildDashboardResponse.
	allCollections, err := s.store.ListCollectionsMinimal(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	ctxMap := buildDoneContextMap(allCollections)

	links, err := s.store.ListWorkspaceGraphLinks(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	resp := GraphResponse{Nodes: []GraphNode{}, Edges: []GraphEdge{}}

	// Build the visible node set first; edges are filtered against it.
	refByID := make(map[string]string, len(items))
	nodeIdx := make(map[string]int, len(items)) // item ID → index in resp.Nodes
	for _, item := range items {
		if item.Ref == "" {
			continue
		}
		terminal := isItemDone(item.Fields, item.CollectionID, ctxMap)
		if terminal && !includeTerminal {
			continue
		}
		var fields map[string]any
		_ = json.Unmarshal([]byte(item.Fields), &fields)
		status, _ := fields["status"].(string)
		refByID[item.ID] = item.Ref
		nodeIdx[item.ID] = len(resp.Nodes)
		resp.Nodes = append(resp.Nodes, GraphNode{
			Ref:        item.Ref,
			Title:      item.Title,
			Collection: item.CollectionSlug,
			Status:     status,
			IsTerminal: terminal,
			UpdatedAt:  item.UpdatedAt,
		})
	}

	for _, l := range links {
		srcRef, srcOK := refByID[l.SourceID]
		tgtRef, tgtOK := refByID[l.TargetID]
		if !srcOK || !tgtOK {
			continue
		}
		resp.Edges = append(resp.Edges, GraphEdge{Source: srcRef, Target: tgtRef, Type: l.Type})
		// Child counts derive from the edge list: 'parent' and
		// 'implements' both render as children in the existing UI
		// surfaces (see wiki_links_test.go's lineage note).
		if l.Type == "parent" || l.Type == "implements" {
			resp.Nodes[nodeIdx[l.TargetID]].ChildCount++
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
