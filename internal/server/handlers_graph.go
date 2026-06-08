package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Focus-mode bounds for the per-item neighborhood view (PLAN-1780 /
// TASK-1781). defaultFocusDepth is the BFS hop count when ?depth is
// omitted; maxFocusDepth caps it so a deep chain can't expand into the
// whole workspace. maxFocusNodes caps the neighborhood size — when the
// BFS would exceed it, expansion stops (closest nodes first) and the
// response is flagged truncated so the client can offer expand-on-click.
const (
	defaultFocusDepth = 2
	maxFocusDepth     = 5
)

// maxFocusNodes caps the focus-mode neighborhood size — when the BFS
// would exceed it, expansion stops (closest nodes first) and the
// response is flagged truncated so the client can offer expand-on-click.
// A var, not a const, so tests can lower it without standing up hundreds
// of items (which would trip the per-IP create rate limiter).
var maxFocusNodes = 200

// GraphNode is one item in the workspace graph
// (GET /workspaces/{ws}/graph — PLAN-1730 / TASK-1731). Nodes are
// keyed by ref (TASK-5) rather than UUID: refs are what the web UI's
// force-graph uses for display, deep links, and edge endpoints.
type GraphNode struct {
	// ID is the item UUID. The graph view needs it to correlate SSE
	// item events (which carry item_id, not ref) with rendered nodes
	// for the live glow/pulse layer (TASK-1736).
	ID         string    `json:"id"`
	Ref        string    `json:"ref"`
	Title      string    `json:"title"`
	Collection string    `json:"collection"`
	Status     string    `json:"status,omitempty"`
	IsTerminal bool      `json:"is_terminal"`
	ChildCount int       `json:"child_count"`
	UpdatedAt  time.Time `json:"updated_at"`
	// Role is the assigned agent-role slug, when set. Feeds the graph
	// view's role filter (TASK-1735).
	Role string `json:"role,omitempty"`
}

// GraphEdge is one typed relationship between two graph nodes. Source
// and target are refs. Type is one of 'parent' | 'blocks' |
// 'implements' | 'related' | 'split-from' | 'supersedes' |
// 'wiki-link' (the canonical item_links vocabulary, hyphenated — see
// store.graphEdgeType — plus synthesized wiki-link edges from the
// PLAN-1593 reverse index). For 'parent', source is the child and
// target is the parent (matching item_links semantics); for 'blocks',
// source blocks target.
type GraphEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Type   string `json:"type"`
}

// GraphResponse is the payload of GET /workspaces/{ws}/graph — the
// whole workspace as {nodes, edges} in one call, feeding the 3D graph
// view (PLAN-1730) and the per-item neighborhood view (PLAN-1780).
type GraphResponse struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
	// Truncated is set in focus mode when the neighborhood hit
	// maxFocusNodes and BFS expansion stopped early — the client should
	// offer expand-on-click rather than treat the graph as complete.
	// Omitted (false) for the unbounded whole-workspace view so its
	// payload shape is unchanged.
	Truncated bool `json:"truncated,omitempty"`
}

// handleGetWorkspaceGraph serves GET /api/v1/workspaces/{ws}/graph.
//
// Query params:
//   - include_terminal=true — include items in terminal status
//     (done/fixed/archived/...). Default false: the graph view opens
//     showing active work only; large workspaces would otherwise be
//     hairball soup. The focused item itself is always included even
//     when terminal — you asked to look at it.
//   - focus=REF — render only the neighborhood around this item
//     (PLAN-1780). BFS-traverses typed edges out from the ref,
//     undirected, returning nodes reachable within `depth` hops plus the
//     edges among them. An unknown/invisible ref returns 404.
//   - depth=N — focus-mode hop count (default 2, clamped to
//     [1, maxFocusDepth]). Ignored without focus.
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
	focus := r.URL.Query().Get("focus")
	depth := defaultFocusDepth
	if d := r.URL.Query().Get("depth"); d != "" {
		if n, err := strconv.Atoi(d); err == nil {
			depth = n
		}
	}
	if depth < 1 {
		depth = 1
	}
	if depth > maxFocusDepth {
		depth = maxFocusDepth
	}

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

	// Per-item terminal/status data for every visible item, keyed by ID.
	// Computed once; both the whole-workspace path and focus BFS read it.
	type itemMeta struct {
		terminal bool
		status   string
	}
	metaByID := make(map[string]itemMeta, len(items))
	focusID := ""
	for _, item := range items {
		if item.Ref == "" {
			continue
		}
		terminal := isItemDone(item.Fields, item.CollectionID, ctxMap)
		var fields map[string]any
		_ = json.Unmarshal([]byte(item.Fields), &fields)
		status, _ := fields["status"].(string)
		metaByID[item.ID] = itemMeta{terminal: terminal, status: status}
		if focus != "" && item.Ref == focus {
			focusID = item.ID
		}
	}

	// Decide which item IDs are in the rendered node set.
	included := make(map[string]bool, len(items))
	if focus == "" {
		// Whole-workspace view: every visible item, terminal filtered.
		for id, m := range metaByID {
			if m.terminal && !includeTerminal {
				continue
			}
			included[id] = true
		}
	} else {
		// Focus view: BFS the neighborhood around focusID. An unknown or
		// invisible ref is a 404 — the caller can't tell those apart, by
		// design (no leaking existence of items the user can't see).
		if focusID == "" {
			writeError(w, http.StatusNotFound, "not_found", "focus item not found")
			return
		}
		// Undirected adjacency among visible items only, so the BFS can't
		// hop through (or to) items outside the visible set.
		adj := make(map[string][]string)
		for _, l := range links {
			_, srcOK := metaByID[l.SourceID]
			_, tgtOK := metaByID[l.TargetID]
			if !srcOK || !tgtOK {
				continue
			}
			adj[l.SourceID] = append(adj[l.SourceID], l.TargetID)
			adj[l.TargetID] = append(adj[l.TargetID], l.SourceID)
		}
		// The focus node is always included, even if terminal — you asked
		// to view it. Neighbors honor the terminal filter.
		included[focusID] = true
		frontier := []string{focusID}
		for hop := 0; hop < depth && len(frontier) > 0 && !resp.Truncated; hop++ {
			var next []string
			for _, id := range frontier {
				for _, nb := range adj[id] {
					if included[nb] {
						continue
					}
					if metaByID[nb].terminal && !includeTerminal {
						continue
					}
					if len(included) >= maxFocusNodes {
						resp.Truncated = true
						break
					}
					included[nb] = true
					next = append(next, nb)
				}
				if resp.Truncated {
					break
				}
			}
			frontier = next
		}
	}

	// Build nodes in items order (stable) for the included set; edges are
	// filtered against it so guests never see dangling endpoints.
	refByID := make(map[string]string, len(included))
	nodeIdx := make(map[string]int, len(included)) // item ID → index in resp.Nodes
	for _, item := range items {
		if item.Ref == "" || !included[item.ID] {
			continue
		}
		m := metaByID[item.ID]
		refByID[item.ID] = item.Ref
		nodeIdx[item.ID] = len(resp.Nodes)
		resp.Nodes = append(resp.Nodes, GraphNode{
			ID:         item.ID,
			Ref:        item.Ref,
			Title:      item.Title,
			Collection: item.CollectionSlug,
			Status:     m.status,
			IsTerminal: m.terminal,
			UpdatedAt:  item.UpdatedAt,
			Role:       item.AgentRoleSlug,
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
