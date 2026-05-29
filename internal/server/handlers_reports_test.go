package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Exercises the full report HTTP path as the workspace owner (admin/all-access
// → visibleCollectionIDs returns nil, so the report is unscoped). Store-level
// tests cover the visibility-scoping mechanics; this locks the route, the
// nil-visibility branch, and the JSON shape.
func TestReportEndpoint_OwnerGetsFullReport(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "A"})
	createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "B"})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/report?window=week", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("report: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp store.ReportData
	parseJSON(t, rr, &resp)

	if resp.Window != "week" || resp.Granularity != "day" {
		t.Fatalf("expected week/day, got %s/%s", resp.Window, resp.Granularity)
	}
	if resp.Totals.Created != 2 {
		t.Fatalf("expected 2 created, got %d", resp.Totals.Created)
	}

	// Arrays must serialize as [] not null.
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &rawMap); err != nil {
		t.Fatalf("parse raw: %v", err)
	}
	for _, key := range []string{"buckets", "collections", "completed_by_collection", "status_distribution"} {
		if string(rawMap[key]) == "null" {
			t.Errorf("expected %s to be [], got null", key)
		}
	}
}

// Pins the bearer-aware visibility gate (BUG-1616/1617 pattern): a platform
// admin who is only a *restricted* workspace member gets the full report over
// a cookie session (web UI admin affordance) but a SCOPED report over bearer
// auth (PAT/CLI/OAuth) — never leaking the hidden collection's aggregates.
func TestReportEndpoint_AdminBearerIsScoped(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "RepBearer", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, admin.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	visible, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Visible", Slug: "visible", Prefix: "VIS",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("create visible: %v", err)
	}
	hidden, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Hidden", Slug: "hidden", Prefix: "HID",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("create hidden: %v", err)
	}
	if _, err := srv.store.CreateItem(ws.ID, visible.ID, models.ItemCreate{Title: "v", Fields: `{"status":"open"}`}); err != nil {
		t.Fatalf("create visible item: %v", err)
	}
	if _, err := srv.store.CreateItem(ws.ID, hidden.ID, models.ItemCreate{Title: "h", Fields: `{"status":"open"}`}); err != nil {
		t.Fatalf("create hidden item: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, admin.ID, "specific", []string{visible.ID}); err != nil {
		t.Fatalf("set access: %v", err)
	}
	_ = hidden

	// Cookie admin → bypass fires, unrestricted (scopeToVisible=false).
	cookieScope, _, err := srv.reportVisibleCollections(
		mkAdminCookieReq(t, "/api/v1/workspaces/"+ws.Slug+"/report", admin), ws.ID)
	if err != nil {
		t.Fatalf("cookie scope: %v", err)
	}
	if cookieScope {
		t.Errorf("cookie admin should be unrestricted (scopeToVisible=false)")
	}

	// Bearer admin → bypass suppressed, scoped to the granted collection only.
	bearerScope, bearerIDs, err := srv.reportVisibleCollections(
		mkAdminBearerReq(t, "/api/v1/workspaces/"+ws.Slug+"/report", admin), ws.ID)
	if err != nil {
		t.Fatalf("bearer scope: %v", err)
	}
	if !bearerScope {
		t.Fatalf("bearer admin must be scoped, got scopeToVisible=false")
	}
	if len(bearerIDs) != 1 || bearerIDs[0] != visible.ID {
		t.Fatalf("bearer admin should be scoped to the 'visible' collection, got %v", bearerIDs)
	}

	// End-to-end through GetReport: bearer scope sees only the visible item.
	rep, err := srv.store.GetReport(ws.ID, store.ReportOptions{
		Window: "week", ScopeToVisible: bearerScope, VisibleCollectionIDs: bearerIDs,
	})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if rep.Totals.Created != 1 || len(rep.Collections) != 1 || rep.Collections[0] != "visible" {
		t.Fatalf("bearer-scoped report leaked hidden collection: %+v", rep)
	}
}

func TestReportEndpoint_DefaultsToWeek(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/report", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("report: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp store.ReportData
	parseJSON(t, rr, &resp)
	if resp.Window != "week" {
		t.Fatalf("expected default window=week, got %q", resp.Window)
	}
}
