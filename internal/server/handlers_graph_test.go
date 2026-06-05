package server

import (
	"net/http"
	"strings"
	"testing"
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
