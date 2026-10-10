package server

import (
	"net/http"
	"testing"
)

// BUG-3538: an item under TWO active plans (a parent link to one, an
// implements link to the other) is suggested once, not once per plan. The
// web keys suggested_next by item_slug, and the duplicate threw during render,
// leaving the dashboard on its loading skeleton (cookie-sales, day 90).
func TestBUG3538_SuggestedNextOncePerItemUnderTwoActivePlans(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	a := createItem(t, srv, ws, "plans", map[string]any{"title": "Plan A", "fields": map[string]any{"status": "active"}})
	b := createItem(t, srv, ws, "plans", map[string]any{"title": "Plan B", "fields": map[string]any{"status": "active"}})
	task := createItem(t, srv, ws, "tasks", map[string]any{"title": "Shared task", "fields": map[string]any{
		"status": "in-progress", "priority": "critical", "parent": a.Ref,
	}})
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/"+task.Slug+"/links", map[string]any{
		"target_id": b.ID, "link_type": "implements",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("implements link: %d %s", rr.Code, rr.Body.String())
	}

	d := getDashboard(t, srv, ws)
	if len(d.ActivePlans) != 2 {
		t.Fatalf("precondition: two active plans, got %d", len(d.ActivePlans))
	}
	for _, p := range d.ActivePlans {
		if p.TaskCount != 1 {
			t.Fatalf("precondition: the task is a child of %s too (task_count %d)", p.Slug, p.TaskCount)
		}
	}
	n := 0
	for _, s := range d.SuggestedNext {
		if s.ItemSlug == task.Slug {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the task is suggested %d times, want once: %+v", n, d.SuggestedNext)
	}
}
