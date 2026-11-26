package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4026: GET /api/v1/workspaces/{ws}/items/{slug}/backlinks sets
// X-Pad-Has-More ("true"/"false" on every 200) and X-Pad-Next-Offset (only
// when true), matching the item list contract.

func TestBUG4026_BacklinksPagingHeaders_SameWorkspace(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "bl-same@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "SameWS", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("create ws: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, admin.ID, "owner"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	tasks, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	target, err := srv.store.CreateItem(ws.ID, tasks.ID, models.ItemCreate{
		Title: "Target", CreatedBy: "user", Source: "test",
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	targetRef := "TASK-" + strconv.Itoa(*target.ItemNumber)
	for i := 0; i < 5; i++ {
		if _, err := srv.store.CreateItem(ws.ID, tasks.ID, models.ItemCreate{
			Title:     fmt.Sprintf("Source %d", i),
			Content:   fmt.Sprintf("Links to [[%s]]", targetRef),
			CreatedBy: "user", Source: "test",
		}); err != nil {
			t.Fatalf("create source %d: %v", i, err)
		}
	}
	sessTok, err := srv.store.CreateSession(admin.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	baseURL := "/api/v1/workspaces/" + ws.Slug + "/items/" + target.Slug + "/backlinks"

	get := func(q string) ([]models.Backlink, string, string) {
		t.Helper()
		rr := doRequestWithCookie(srv, "GET", baseURL+q, nil, sessTok)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d body=%s", q, rr.Code, rr.Body.String())
		}
		var rows []models.Backlink
		if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode %s: %v", q, err)
		}
		return rows, rr.Header().Get(itemListHasMoreHeader), rr.Header().Get(itemListNextOffsetHeader)
	}

	// Mid-list: 2 of 5 -> has-more true, next-offset 2.
	rows, hasMore, nextOff := get("?limit=2&offset=0")
	if len(rows) != 2 || hasMore != "true" || nextOff != "2" {
		t.Fatalf("page 1: len=%d hasMore=%q nextOff=%q; want 2, true, 2", len(rows), hasMore, nextOff)
	}

	// Still mid-list: 2 more -> has-more true, next-offset 4.
	rows, hasMore, nextOff = get("?limit=2&offset=2")
	if len(rows) != 2 || hasMore != "true" || nextOff != "4" {
		t.Fatalf("page 2: len=%d hasMore=%q nextOff=%q; want 2, true, 4", len(rows), hasMore, nextOff)
	}

	// Last partial page: 1 row -> has-more false, no next-offset.
	rows, hasMore, nextOff = get("?limit=2&offset=4")
	if len(rows) != 1 || hasMore != "false" || nextOff != "" {
		t.Fatalf("page 3: len=%d hasMore=%q nextOff=%q; want 1, false, empty", len(rows), hasMore, nextOff)
	}

	// Exact end of all rows -> has-more false, no next-offset.
	rows, hasMore, nextOff = get("?limit=5&offset=0")
	if len(rows) != 5 || hasMore != "false" || nextOff != "" {
		t.Fatalf("exact all: len=%d hasMore=%q nextOff=%q; want 5, false, empty", len(rows), hasMore, nextOff)
	}

	// Past end -> empty array, has-more false.
	rows, hasMore, nextOff = get("?limit=5&offset=10")
	if len(rows) != 0 || hasMore != "false" || nextOff != "" {
		t.Fatalf("past end: len=%d hasMore=%q nextOff=%q; want 0, false, empty", len(rows), hasMore, nextOff)
	}
}

func TestBUG4026_BacklinksPagingHeaders_SameAndCrossWorkspaceBoundary(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "bl-cross@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	wsA, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "CrossA", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("create wsA: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(wsA.ID, admin.ID, "owner"); err != nil {
		t.Fatalf("add wsA member: %v", err)
	}
	wsB, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "CrossB", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("create wsB: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(wsB.ID, admin.ID, "owner"); err != nil {
		t.Fatalf("add wsB member: %v", err)
	}

	tasksA, err := srv.store.CreateCollection(wsA.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TASK"})
	if err != nil {
		t.Fatalf("create tasksA: %v", err)
	}
	notesB, err := srv.store.CreateCollection(wsB.ID, models.CollectionCreate{Name: "Notes", Slug: "notes", Prefix: "NOTE"})
	if err != nil {
		t.Fatalf("create notesB: %v", err)
	}

	target, err := srv.store.CreateItem(wsA.ID, tasksA.ID, models.ItemCreate{
		Title: "Target", CreatedBy: "user", Source: "test",
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	targetRef := "TASK-" + strconv.Itoa(*target.ItemNumber)

	// 3 same-workspace backlinks.
	for i := 0; i < 3; i++ {
		if _, err := srv.store.CreateItem(wsA.ID, tasksA.ID, models.ItemCreate{
			Title:     fmt.Sprintf("Same %d", i),
			Content:   fmt.Sprintf("Refs [[%s]]", targetRef),
			CreatedBy: "user", Source: "test",
		}); err != nil {
			t.Fatalf("create same %d: %v", i, err)
		}
	}
	// 2 cross-workspace backlinks.
	for i := 0; i < 2; i++ {
		if _, err := srv.store.CreateItem(wsB.ID, notesB.ID, models.ItemCreate{
			Title:     fmt.Sprintf("Cross %d", i),
			Content:   fmt.Sprintf("Refs [[%s::%s]]", wsA.Slug, targetRef),
			CreatedBy: "user", Source: "test",
		}); err != nil {
			t.Fatalf("create cross %d: %v", i, err)
		}
	}

	sessTok, err := srv.store.CreateSession(admin.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	baseURL := "/api/v1/workspaces/" + wsA.Slug + "/items/" + target.Slug + "/backlinks"

	get := func(q string) ([]models.Backlink, string, string) {
		t.Helper()
		rr := doRequestWithCookie(srv, "GET", baseURL+q, nil, sessTok)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d body=%s", q, rr.Code, rr.Body.String())
		}
		var rows []models.Backlink
		if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode %s: %v", q, err)
		}
		return rows, rr.Header().Get(itemListHasMoreHeader), rr.Header().Get(itemListNextOffsetHeader)
	}

	// Ends exactly at the end of same-ws (3), with cross-ws rows present -> true.
	rows, hasMore, nextOff := get("?limit=3&offset=0")
	if len(rows) != 3 || hasMore != "true" || nextOff != "3" {
		t.Fatalf("same-ws boundary: len=%d hasMore=%q nextOff=%q; want 3, true, 3", len(rows), hasMore, nextOff)
	}

	// Next page gets the 2 cross-ws rows, ending exactly at end of all rows -> false.
	rows, hasMore, nextOff = get("?limit=2&offset=3")
	if len(rows) != 2 || hasMore != "false" || nextOff != "" {
		t.Fatalf("exact cross-ws end: len=%d hasMore=%q nextOff=%q; want 2, false, empty", len(rows), hasMore, nextOff)
	}

	// Single page spanning both tiers and ending exactly at all 5 rows -> false.
	rows, hasMore, nextOff = get("?limit=5&offset=0")
	if len(rows) != 5 || hasMore != "false" || nextOff != "" {
		t.Fatalf("exact union end: len=%d hasMore=%q nextOff=%q; want 5, false, empty", len(rows), hasMore, nextOff)
	}
}

func TestBUG4026_BacklinksPagingHeaders_CrossWorkspaceLimit300NotTruncated(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "bl-300@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	wsA, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Cross300A", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("create wsA: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(wsA.ID, admin.ID, "owner"); err != nil {
		t.Fatalf("add wsA member: %v", err)
	}
	wsB, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Cross300B", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("create wsB: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(wsB.ID, admin.ID, "owner"); err != nil {
		t.Fatalf("add wsB member: %v", err)
	}

	tasksA, err := srv.store.CreateCollection(wsA.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TASK"})
	if err != nil {
		t.Fatalf("create tasksA: %v", err)
	}
	notesB, err := srv.store.CreateCollection(wsB.ID, models.CollectionCreate{Name: "Notes", Slug: "notes", Prefix: "NOTE"})
	if err != nil {
		t.Fatalf("create notesB: %v", err)
	}
	target, err := srv.store.CreateItem(wsA.ID, tasksA.ID, models.ItemCreate{
		Title: "Popular Target", CreatedBy: "user", Source: "test",
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	targetRef := "TASK-" + strconv.Itoa(*target.ItemNumber)

	// 301 cross-workspace backlinks: asking for limit=300 must return all 300
	// (NOT 50 from store's >300 reset) with has-more=true and next-offset=300.
	for i := 0; i < 301; i++ {
		if _, err := srv.store.CreateItem(wsB.ID, notesB.ID, models.ItemCreate{
			Title:     fmt.Sprintf("Cross %03d", i),
			Content:   fmt.Sprintf("Refs [[%s::%s]]", wsA.Slug, targetRef),
			CreatedBy: "user", Source: "test",
		}); err != nil {
			t.Fatalf("create cross %d: %v", i, err)
		}
	}

	sessTok, err := srv.store.CreateSession(admin.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	baseURL := "/api/v1/workspaces/" + wsA.Slug + "/items/" + target.Slug + "/backlinks"

	rr := doRequestWithCookie(srv, "GET", baseURL+"?limit=300&offset=0", nil, sessTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("page 1: status %d body=%s", rr.Code, rr.Body.String())
	}
	var page1 []models.Backlink
	if err := json.Unmarshal(rr.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode page 1: %v", err)
	}
	if len(page1) != 300 {
		t.Fatalf("page 1 returned %d rows, want 300 (store must not reset limit>300 to 50)", len(page1))
	}
	if got := rr.Header().Get(itemListHasMoreHeader); got != "true" {
		t.Fatalf("page 1 X-Pad-Has-More = %q, want true", got)
	}
	if got := rr.Header().Get(itemListNextOffsetHeader); got != "300" {
		t.Fatalf("page 1 X-Pad-Next-Offset = %q, want 300", got)
	}

	rr2 := doRequestWithCookie(srv, "GET", baseURL+"?limit=300&offset=300", nil, sessTok)
	if rr2.Code != http.StatusOK {
		t.Fatalf("page 2: status %d body=%s", rr2.Code, rr2.Body.String())
	}
	var page2 []models.Backlink
	if err := json.Unmarshal(rr2.Body.Bytes(), &page2); err != nil {
		t.Fatalf("decode page 2: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("page 2 returned %d rows, want 1", len(page2))
	}
	if got := rr2.Header().Get(itemListHasMoreHeader); got != "false" {
		t.Fatalf("page 2 X-Pad-Has-More = %q, want false", got)
	}
	if got := rr2.Header().Get(itemListNextOffsetHeader); got != "" {
		t.Fatalf("page 2 X-Pad-Next-Offset = %q, want empty", got)
	}
}
