package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// helper: fetch the workspace graph and return the parsed response
func getGraph(t *testing.T, srv *Server, wsSlug, query string) GraphResponse {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+wsSlug+"/graph"+query, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("graph: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp GraphResponse
	parseJSON(t, rr, &resp)
	return resp
}

func graphNode(resp GraphResponse, ref string) *GraphNode {
	for i := range resp.Nodes {
		if resp.Nodes[i].Ref == ref {
			return &resp.Nodes[i]
		}
	}
	return nil
}

func hasGraphEdge(resp GraphResponse, source, target, typ string) bool {
	for _, e := range resp.Edges {
		if e.Source == source && e.Target == target && e.Type == typ {
			return true
		}
	}
	return false
}

func TestGraphEmpty(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	resp := getGraph(t, srv, slug, "")
	if len(resp.Nodes) != 0 {
		t.Errorf("expected 0 nodes, got %d", len(resp.Nodes))
	}
	if len(resp.Edges) != 0 {
		t.Errorf("expected 0 edges, got %d", len(resp.Edges))
	}

	// Arrays must serialize as [], not null.
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/graph", nil)
	body := rr.Body.String()
	if body == "" || body[0] != '{' {
		t.Fatalf("unexpected body: %s", body)
	}
	for _, frag := range []string{`"nodes":[]`, `"edges":[]`} {
		if !strings.Contains(body, frag) {
			t.Errorf("expected body to contain %s, got %s", frag, body)
		}
	}
}

func TestGraphNodesAndTypedEdges(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	plan := createItem(t, srv, slug, "plans", map[string]interface{}{
		"title": "Graph plan", "fields": map[string]interface{}{"status": "active"},
	})
	task := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Graph child task", "fields": map[string]interface{}{"status": "open"},
	})
	blocked := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Blocked task", "fields": map[string]interface{}{"status": "open"},
	})

	// task is a child of plan
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+task.Slug+"/links", map[string]interface{}{
		"target_id": plan.ID, "link_type": "parent",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create parent link: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	// task blocks blocked
	createBlocksLink(t, srv, slug, task.Slug, blocked.ID)
	// blocked split from plan — stored as split_from, must surface
	// hyphenated per the advertised edge vocabulary.
	rr2 := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+blocked.Slug+"/links", map[string]interface{}{
		"target_id": plan.ID, "link_type": "split_from",
	})
	if rr2.Code != http.StatusCreated {
		t.Fatalf("create split_from link: expected 201, got %d: %s", rr2.Code, rr2.Body.String())
	}

	resp := getGraph(t, srv, slug, "")
	if len(resp.Nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d: %+v", len(resp.Nodes), resp.Nodes)
	}

	pn := graphNode(resp, plan.Ref)
	if pn == nil {
		t.Fatalf("plan node %s missing", plan.Ref)
	}
	if pn.Collection != "plans" {
		t.Errorf("expected plan node collection=plans, got %s", pn.Collection)
	}
	if pn.Status != "active" {
		t.Errorf("expected plan node status=active, got %s", pn.Status)
	}
	if pn.IsTerminal {
		t.Errorf("expected plan node is_terminal=false")
	}
	if pn.ChildCount != 1 {
		t.Errorf("expected plan child_count=1, got %d", pn.ChildCount)
	}
	tn := graphNode(resp, task.Ref)
	if tn == nil || tn.ChildCount != 0 {
		t.Errorf("expected task node with child_count=0, got %+v", tn)
	}

	if !hasGraphEdge(resp, task.Ref, plan.Ref, "parent") {
		t.Errorf("expected parent edge %s -> %s, edges: %+v", task.Ref, plan.Ref, resp.Edges)
	}
	if !hasGraphEdge(resp, task.Ref, blocked.Ref, "blocks") {
		t.Errorf("expected blocks edge %s -> %s, edges: %+v", task.Ref, blocked.Ref, resp.Edges)
	}
	if !hasGraphEdge(resp, blocked.Ref, plan.Ref, "split-from") {
		t.Errorf("expected split-from edge %s -> %s, edges: %+v", blocked.Ref, plan.Ref, resp.Edges)
	}
}

func TestGraphWikiLinkEdges(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	target := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Wiki target", "fields": map[string]interface{}{"status": "open"},
	})
	// Mention the target by ref twice — must still produce ONE edge.
	source := createItem(t, srv, slug, "docs", map[string]interface{}{
		"title":   "Wiki source",
		"content": "See [[" + target.Ref + "]] and again [[" + target.Ref + "]].",
	})
	// A stored wiki_link item_links row for the same pair normalizes to
	// the same 'wiki-link' type and must dedupe with the parsed mention.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+source.Slug+"/links", map[string]interface{}{
		"target_id": target.ID, "link_type": "wiki_link",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create wiki_link link: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := getGraph(t, srv, slug, "")
	count := 0
	for _, e := range resp.Edges {
		if e.Source == source.Ref && e.Target == target.Ref && e.Type == "wiki-link" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 wiki-link edge %s -> %s, got %d (edges: %+v)",
			source.Ref, target.Ref, count, resp.Edges)
	}
}

func TestGraphTerminalFiltering(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	open := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Open task", "fields": map[string]interface{}{"status": "open"},
	})
	done := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Done task", "fields": map[string]interface{}{"status": "done"},
	})
	// Edge between them — must disappear with the terminal node.
	createBlocksLink(t, srv, slug, done.Slug, open.ID)

	resp := getGraph(t, srv, slug, "")
	if graphNode(resp, open.Ref) == nil {
		t.Errorf("expected open task %s in default graph", open.Ref)
	}
	if graphNode(resp, done.Ref) != nil {
		t.Errorf("expected done task %s filtered from default graph", done.Ref)
	}
	if len(resp.Edges) != 0 {
		t.Errorf("expected edges to terminal nodes filtered, got %+v", resp.Edges)
	}

	full := getGraph(t, srv, slug, "?include_terminal=true")
	dn := graphNode(full, done.Ref)
	if dn == nil {
		t.Fatalf("expected done task %s with include_terminal=true", done.Ref)
	}
	if !dn.IsTerminal {
		t.Errorf("expected done task is_terminal=true")
	}
	if !hasGraphEdge(full, done.Ref, open.Ref, "blocks") {
		t.Errorf("expected blocks edge restored with include_terminal=true, edges: %+v", full.Edges)
	}
}

func TestGraphExcludesDeletedItems(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	keep := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Kept task", "fields": map[string]interface{}{"status": "open"},
	})
	gone := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Deleted task", "fields": map[string]interface{}{"status": "open"},
	})
	createBlocksLink(t, srv, slug, keep.Slug, gone.ID)

	rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/items/"+gone.Slug, nil)
	if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("delete item: expected 200/204, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := getGraph(t, srv, slug, "")
	if graphNode(resp, gone.Ref) != nil {
		t.Errorf("expected deleted item %s excluded", gone.Ref)
	}
	if len(resp.Edges) != 0 {
		t.Errorf("expected edges to deleted items excluded, got %+v", resp.Edges)
	}
}

// chainTasks creates n open tasks linked in a blocks chain
// t0 -> t1 -> ... -> t(n-1) and returns them in order.
func chainTasks(t *testing.T, srv *Server, slug string, n int) []models.Item {
	t.Helper()
	tasks := make([]models.Item, n)
	for i := 0; i < n; i++ {
		tasks[i] = createItem(t, srv, slug, "tasks", map[string]interface{}{
			"title": "Chain task", "fields": map[string]interface{}{"status": "open"},
		})
	}
	for i := 0; i+1 < n; i++ {
		createBlocksLink(t, srv, slug, tasks[i].Slug, tasks[i+1].ID)
	}
	return tasks
}

func TestGraphFocusDepth(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	// t0 - t1 - t2 - t3 - t4 (undirected neighborhood for BFS).
	tk := chainTasks(t, srv, slug, 5)

	// depth=1 around t0 → t0, t1 only.
	d1 := getGraph(t, srv, slug, "?focus="+tk[0].Ref+"&depth=1")
	if len(d1.Nodes) != 2 {
		t.Fatalf("depth=1: expected 2 nodes, got %d: %+v", len(d1.Nodes), d1.Nodes)
	}
	if graphNode(d1, tk[0].Ref) == nil || graphNode(d1, tk[1].Ref) == nil {
		t.Errorf("depth=1: expected t0 and t1 present")
	}
	if graphNode(d1, tk[2].Ref) != nil {
		t.Errorf("depth=1: expected t2 excluded")
	}

	// depth=2 around t0 → t0, t1, t2.
	d2 := getGraph(t, srv, slug, "?focus="+tk[0].Ref+"&depth=2")
	if len(d2.Nodes) != 3 {
		t.Fatalf("depth=2: expected 3 nodes, got %d: %+v", len(d2.Nodes), d2.Nodes)
	}
	if graphNode(d2, tk[3].Ref) != nil {
		t.Errorf("depth=2: expected t3 excluded")
	}

	// default depth (no param) is 2.
	def := getGraph(t, srv, slug, "?focus="+tk[0].Ref)
	if len(def.Nodes) != 3 {
		t.Errorf("default depth: expected 3 nodes, got %d", len(def.Nodes))
	}

	// focus in the middle reaches both directions.
	mid := getGraph(t, srv, slug, "?focus="+tk[2].Ref+"&depth=1")
	if len(mid.Nodes) != 3 {
		t.Fatalf("mid focus: expected 3 nodes (t1,t2,t3), got %d: %+v", len(mid.Nodes), mid.Nodes)
	}
	for _, ref := range []string{tk[1].Ref, tk[2].Ref, tk[3].Ref} {
		if graphNode(mid, ref) == nil {
			t.Errorf("mid focus: expected %s present", ref)
		}
	}

	// depth is clamped — a huge depth still resolves the whole 5-chain,
	// not an error.
	clamped := getGraph(t, srv, slug, "?focus="+tk[0].Ref+"&depth=999")
	if len(clamped.Nodes) != 5 {
		t.Errorf("clamped depth: expected all 5 nodes, got %d", len(clamped.Nodes))
	}
}

func TestGraphFocusTerminal(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// The focused item is always included, even when terminal.
	done := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Done focus", "fields": map[string]interface{}{"status": "done"},
	})
	active := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Active neighbor", "fields": map[string]interface{}{"status": "open"},
	})
	createBlocksLink(t, srv, slug, done.Slug, active.ID)

	resp := getGraph(t, srv, slug, "?focus="+done.Ref)
	dn := graphNode(resp, done.Ref)
	if dn == nil {
		t.Fatalf("expected terminal focus node %s included", done.Ref)
	}
	if !dn.IsTerminal {
		t.Errorf("expected focus node is_terminal=true")
	}
	if graphNode(resp, active.Ref) == nil {
		t.Errorf("expected active neighbor %s included", active.Ref)
	}

	// A terminal neighbor is hidden by default, restored with the flag.
	focus := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Active focus", "fields": map[string]interface{}{"status": "open"},
	})
	doneNb := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Done neighbor", "fields": map[string]interface{}{"status": "done"},
	})
	createBlocksLink(t, srv, slug, focus.Slug, doneNb.ID)

	def := getGraph(t, srv, slug, "?focus="+focus.Ref)
	if graphNode(def, doneNb.Ref) != nil {
		t.Errorf("expected terminal neighbor %s hidden by default", doneNb.Ref)
	}
	full := getGraph(t, srv, slug, "?focus="+focus.Ref+"&include_terminal=true")
	if graphNode(full, doneNb.Ref) == nil {
		t.Errorf("expected terminal neighbor %s restored with include_terminal=true", doneNb.Ref)
	}
}

func TestGraphFocusUnknownRef(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Some task", "fields": map[string]interface{}{"status": "open"},
	})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/graph?focus=TASK-9999", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown focus ref, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGraphFocusCrossCollectionEdges(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	plan := createItem(t, srv, slug, "plans", map[string]interface{}{
		"title": "Focus plan", "fields": map[string]interface{}{"status": "active"},
	})
	task := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Focus task", "fields": map[string]interface{}{"status": "open"},
	})
	bug := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Related bug", "fields": map[string]interface{}{"status": "open"},
	})
	// task is a child of plan; task blocks bug.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+task.Slug+"/links", map[string]interface{}{
		"target_id": plan.ID, "link_type": "parent",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create parent link: %d %s", rr.Code, rr.Body.String())
	}
	createBlocksLink(t, srv, slug, task.Slug, bug.ID)

	// Focus on the task at depth 1 pulls in both the parent plan and the
	// blocked bug, across collections, with typed edges preserved.
	resp := getGraph(t, srv, slug, "?focus="+task.Ref+"&depth=1")
	if len(resp.Nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d: %+v", len(resp.Nodes), resp.Nodes)
	}
	if !hasGraphEdge(resp, task.Ref, plan.Ref, "parent") {
		t.Errorf("expected parent edge %s -> %s", task.Ref, plan.Ref)
	}
	if !hasGraphEdge(resp, task.Ref, bug.Ref, "blocks") {
		t.Errorf("expected blocks edge %s -> %s", task.Ref, bug.Ref)
	}
	if resp.Truncated {
		t.Errorf("did not expect truncation for a 3-node neighborhood")
	}
}

func TestGraphFocusTruncation(t *testing.T) {
	// Lower the cap so we don't have to create 200 items (which would
	// trip the per-IP create rate limiter). Restore after.
	orig := maxFocusNodes
	maxFocusNodes = 5
	defer func() { maxFocusNodes = orig }()

	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// A star larger than maxFocusNodes around a single focus node.
	hub := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Hub", "fields": map[string]interface{}{"status": "open"},
	})
	for i := 0; i < maxFocusNodes+5; i++ {
		leaf := createItem(t, srv, slug, "tasks", map[string]interface{}{
			"title": "Leaf", "fields": map[string]interface{}{"status": "open"},
		})
		createBlocksLink(t, srv, slug, hub.Slug, leaf.ID)
	}

	resp := getGraph(t, srv, slug, "?focus="+hub.Ref+"&depth=1")
	if !resp.Truncated {
		t.Errorf("expected truncated=true for an oversized neighborhood")
	}
	if len(resp.Nodes) != maxFocusNodes {
		t.Errorf("expected node count capped at %d, got %d", maxFocusNodes, len(resp.Nodes))
	}
}
