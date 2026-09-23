package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2781: the activity feeds page by a (created_at, id) keyset cursor.

func seedWorkspaceActivities(t *testing.T, srv *Server, slug, userID string, n int) {
	t.Helper()
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("workspace %s: %v", slug, err)
	}
	for range n {
		// Workspace-level rows (no document), so the visibility filter keeps
		// them. Written back-to-back, so most share one second: the walk
		// below crosses timestamp ties, which is the case the id half of the
		// cursor exists for.
		if _, err := srv.store.CreateActivity(models.Activity{WorkspaceID: ws.ID, Action: "settings_changed", Actor: "user", Source: "web", UserID: userID}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func TestWorkspaceActivityKeysetWalksEveryRowOnce(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	seedWorkspaceActivities(t, srv, slug, "", 7)

	base := "/api/v1/workspaces/" + slug + "/activity?action=settings_changed"
	rr := doRequest(srv, "GET", base+"&limit=100", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("full list: %d %s", rr.Code, rr.Body.String())
	}
	var all []models.Activity
	parseJSON(t, rr, &all)
	if len(all) != 7 {
		t.Fatalf("precondition: want 7 seeded rows, got %d", len(all))
	}

	var walked []string
	path := base + "&limit=2"
	for range 10 {
		rr := doRequest(srv, "GET", path, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("page: %d %s", rr.Code, rr.Body.String())
		}
		var page []models.Activity
		parseJSON(t, rr, &page)
		if len(page) == 0 {
			break
		}
		for _, a := range page {
			walked = append(walked, a.ID)
		}
		last := page[len(page)-1]
		// The cursor is taken from the response's own JSON, exactly as a
		// client does, so the serialized created_at must parse back.
		raw, _ := json.Marshal(last.CreatedAt)
		var ts string
		_ = json.Unmarshal(raw, &ts)
		path = base + "&limit=2&before=" + url.QueryEscape(ts) + "&before_id=" + url.QueryEscape(last.ID)
	}
	if len(walked) != len(all) {
		t.Fatalf("walked %d rows, want %d: %v", len(walked), len(all), walked)
	}
	for i := range all {
		if walked[i] != all[i].ID {
			t.Fatalf("row %d: walked %s, full list has %s", i, walked[i], all[i].ID)
		}
	}
}

func TestActivityCursorIsRefusedWhenMalformed(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	base := "/api/v1/workspaces/" + slug + "/activity?"
	cases := map[string]string{
		"before without before_id": "before=2026-01-01T00:00:00Z",
		"before_id without before": "before_id=abc",
		"cursor with offset":       "before=2026-01-01T00:00:00Z&before_id=abc&offset=20",
		"unparseable before":       "before=yesterday&before_id=abc",
	}
	for name, q := range cases {
		rr := doRequest(srv, "GET", base+q, nil)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400: %s", name, rr.Code, rr.Body.String())
		}
	}
	// Positive control: offset alone still works (the deprecation window),
	// and so does a well-formed cursor.
	for _, q := range []string{"offset=20", "before=2026-01-01T00:00:00Z&before_id=abc"} {
		if rr := doRequest(srv, "GET", base+q, nil); rr.Code != http.StatusOK {
			t.Errorf("%s: got %d, want 200: %s", q, rr.Code, rr.Body.String())
		}
	}

	// The item activity door shares the parser (codex round 1): a cursor it
	// ignored would answer every next page with the first page again.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{"title": "cursor door"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)
	itemBase := "/api/v1/workspaces/" + slug + "/items/" + item.Slug + "/activity?"
	if rr := doRequest(srv, "GET", itemBase+"before_id=abc", nil); rr.Code != http.StatusBadRequest {
		t.Errorf("item door, half cursor: got %d, want 400", rr.Code)
	}
	if rr := doRequest(srv, "GET", itemBase+"before=2000-01-01T00:00:00Z&before_id=0", nil); rr.Code != http.StatusOK {
		t.Errorf("item door, cursor: got %d, want 200", rr.Code)
	} else {
		var page []models.Activity
		parseJSON(t, rr, &page)
		if len(page) != 0 {
			t.Errorf("item door: a cursor before every row must return nothing, got %d rows (was it ignored?)", len(page))
		}
	}
}

func TestAdminUserActivityReturnsAKeysetCursor(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	admin, err := srv.store.GetUserByEmail("admin@test.com")
	if err != nil || admin == nil {
		t.Fatalf("admin: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Admin feed"})
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	slug := ws.Slug
	seedWorkspaceActivities(t, srv, slug, admin.ID, 3)

	type envelope struct {
		Events       []models.Activity `json:"events"`
		NextBefore   *string           `json:"next_before"`
		NextBeforeID *string           `json:"next_before_id"`
	}
	get := func(q string) envelope {
		t.Helper()
		rr := doRequestWithCookie(srv, "GET", "/api/v1/admin/users/"+admin.ID+"/activity?action=settings_changed&"+q, nil, adminToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", q, rr.Code, rr.Body.String())
		}
		var e envelope
		parseJSON(t, rr, &e)
		return e
	}

	first := get("limit=2")
	if len(first.Events) != 2 || first.NextBefore == nil || first.NextBeforeID == nil {
		t.Fatalf("first page: want 2 events and a cursor, got %d events, cursor %v/%v", len(first.Events), first.NextBefore, first.NextBeforeID)
	}
	if *first.NextBeforeID != first.Events[1].ID {
		t.Fatalf("next_before_id %s is not the last row %s", *first.NextBeforeID, first.Events[1].ID)
	}
	second := get("limit=2&before=" + url.QueryEscape(*first.NextBefore) + "&before_id=" + url.QueryEscape(*first.NextBeforeID))
	if len(second.Events) != 1 || second.NextBefore != nil || second.NextBeforeID != nil {
		t.Fatalf("second page: want 1 event and no cursor, got %d events, cursor %v/%v", len(second.Events), second.NextBefore, second.NextBeforeID)
	}
	for _, a := range first.Events {
		if a.ID == second.Events[0].ID {
			t.Fatal("the second page repeated a row from the first")
		}
	}

	// The audit-log door shares the parser: a half cursor is refused there too.
	if rr := doRequestWithCookie(srv, "GET", "/api/v1/audit-log?before_id=abc", nil, adminToken); rr.Code != http.StatusBadRequest {
		t.Fatalf("audit-log half cursor: got %d, want 400", rr.Code)
	}
}
