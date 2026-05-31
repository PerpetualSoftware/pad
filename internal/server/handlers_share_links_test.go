package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestResolveCollectionShareLink_EnrichedPayload pins TASK-1678: the
// public collection share DTO must carry the collection's parsed
// settings + schema (as JSON objects, not raw strings) and each item's
// markdown content, so the anonymous viewer can reproduce the owner's
// view type/grouping and render an inline read-only row expand. It must
// NOT leak internal IDs, creator, timestamps, or authoring-only settings
// (quick_actions / content_template).
func TestResolveCollectionShareLink_EnrichedPayload(t *testing.T) {
	srv := testServer(t)
	slug := createWSForTest(t, srv)

	// Create a collection with an explicit schema + settings. settings
	// includes quick_actions + content_template precisely so we can assert
	// they're stripped from the public projection.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":   "Roadmap",
		"prefix": "ROAD",
		"icon": "🗺️",
		// schema is a JSON-encoded string on CollectionCreate.
		"schema": `{"fields":[` +
			`{"key":"status","label":"Status","type":"select","options":["open","doing","done"],"terminal_options":["done"]},` +
			`{"key":"points","label":"Points","type":"number","suffix":"pts"}` +
			`]}`,
		"settings": map[string]interface{}{
			"default_view":     "board",
			"board_group_by":   "status",
			"list_sort_by":     "points",
			"list_group_by":    "status",
			"layout":           "fields-primary",
			"content_template": "## Secret authoring template",
			"quick_actions": []map[string]interface{}{
				{"label": "Do thing", "prompt": "internal prompt", "scope": "item"},
			},
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)

	// Add an item with markdown content.
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/roadmap/items", map[string]interface{}{
		"title":   "Ship it",
		"content": "# Ship it\n\nDetailed body for inline expand.",
		"fields":  map[string]interface{}{"status": "doing", "points": 3},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Resolve the collection so we can attach saved views (TASK-1681). Views
	// are workspace-scoped + collection-scoped; ListViews returns them ordered
	// by sort_order, and the public projection must strip internal UUIDs.
	ws0, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws0 == nil {
		t.Fatalf("get workspace for views: %v", err)
	}
	boardView, err := srv.store.CreateView(ws0.ID, models.ViewCreate{
		CollectionID: &coll.ID,
		Name:         "Board",
		ViewType:     "board",
		Config:       `{"group_by":"status"}`,
	})
	if err != nil {
		t.Fatalf("create board view: %v", err)
	}
	_, err = srv.store.CreateView(ws0.ID, models.ViewCreate{
		CollectionID: &coll.ID,
		Name:         "Table",
		ViewType:     "table",
	})
	if err != nil {
		t.Fatalf("create table view: %v", err)
	}

	// Create a public collection share link directly via the store. The
	// HTTP create route gates on owner role + a created_by user FK; this
	// test exercises the public resolve path, so we mint the link with a
	// real user to satisfy the FK without standing up an auth session.
	owner, err := srv.store.CreateUser(models.UserCreate{Email: "owner@test.com", Name: "Owner", Password: "pw-owner"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("get workspace: %v", err)
	}
	link, err := srv.store.CreateShareLink(ws.ID, "collection", coll.ID, "view", owner.ID, nil)
	if err != nil {
		t.Fatalf("create share link: %v", err)
	}
	if link.Token == "" {
		t.Fatal("expected a share link token")
	}

	// Resolve the share link as an anonymous viewer.
	rr = doRequest(srv, "GET", "/api/v1/s/"+link.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve share link: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Type       string `json:"type"`
		Collection struct {
			Name        string `json:"name"`
			Icon        string `json:"icon"`
			Description string `json:"description"`
			Settings    *struct {
				Layout          string `json:"layout"`
				DefaultView     string `json:"default_view"`
				BoardGroupBy    string `json:"board_group_by"`
				ListSortBy      string `json:"list_sort_by"`
				ListGroupBy     string `json:"list_group_by"`
				QuickActions    any    `json:"quick_actions"`
				ContentTemplate any    `json:"content_template"`
			} `json:"settings"`
			Schema *models.CollectionSchema `json:"schema"`
			Views  []struct {
				Name      string         `json:"name"`
				Slug      string         `json:"slug"`
				ViewType  string         `json:"view_type"`
				Config    map[string]any `json:"config"`
				IsDefault bool           `json:"is_default"`
				SortOrder int            `json:"sort_order"`
			} `json:"views"`
		} `json:"collection"`
		Items []struct {
			Title   string `json:"title"`
			Ref     string `json:"ref"`
			Fields  any    `json:"fields"`
			Content string `json:"content"`
		} `json:"items"`
	}
	parseJSON(t, rr, &resp)

	if resp.Type != "collection" {
		t.Fatalf("expected type 'collection', got %q", resp.Type)
	}

	// settings: present, parsed, and projected to view fields only.
	if resp.Collection.Settings == nil {
		t.Fatal("expected collection.settings on the public payload")
	}
	if resp.Collection.Settings.DefaultView != "board" {
		t.Errorf("expected default_view 'board', got %q", resp.Collection.Settings.DefaultView)
	}
	if resp.Collection.Settings.BoardGroupBy != "status" {
		t.Errorf("expected board_group_by 'status', got %q", resp.Collection.Settings.BoardGroupBy)
	}
	if resp.Collection.Settings.ListSortBy != "points" {
		t.Errorf("expected list_sort_by 'points', got %q", resp.Collection.Settings.ListSortBy)
	}
	if resp.Collection.Settings.ListGroupBy != "status" {
		t.Errorf("expected list_group_by 'status', got %q", resp.Collection.Settings.ListGroupBy)
	}
	if resp.Collection.Settings.Layout != "fields-primary" {
		t.Errorf("expected layout 'fields-primary', got %q", resp.Collection.Settings.Layout)
	}
	// Authoring-only fields MUST NOT leak.
	if resp.Collection.Settings.QuickActions != nil {
		t.Errorf("quick_actions must not appear in public settings, got %#v", resp.Collection.Settings.QuickActions)
	}
	if resp.Collection.Settings.ContentTemplate != nil {
		t.Errorf("content_template must not appear in public settings, got %#v", resp.Collection.Settings.ContentTemplate)
	}

	// schema: present, parsed as an object, with the presentation fields.
	if resp.Collection.Schema == nil {
		t.Fatal("expected collection.schema on the public payload")
	}
	if len(resp.Collection.Schema.Fields) != 2 {
		t.Fatalf("expected 2 schema fields, got %d", len(resp.Collection.Schema.Fields))
	}
	statusField := resp.Collection.Schema.Fields[0]
	if statusField.Key != "status" || statusField.Type != "select" {
		t.Errorf("unexpected status field: %#v", statusField)
	}
	if len(statusField.Options) != 3 {
		t.Errorf("expected 3 status options, got %d", len(statusField.Options))
	}
	if len(statusField.TerminalOptions) != 1 || statusField.TerminalOptions[0] != "done" {
		t.Errorf("expected terminal_options [done], got %#v", statusField.TerminalOptions)
	}
	if resp.Collection.Schema.Fields[1].Suffix != "pts" {
		t.Errorf("expected suffix 'pts', got %q", resp.Collection.Schema.Fields[1].Suffix)
	}

	// views: present, projected to public shape, ordered by sort_order,
	// config parsed to an object (TASK-1681).
	if len(resp.Collection.Views) != 2 {
		t.Fatalf("expected 2 saved views, got %d", len(resp.Collection.Views))
	}
	board := resp.Collection.Views[0]
	if board.Name != "Board" || board.ViewType != "board" {
		t.Errorf("unexpected first view: %#v", board)
	}
	if board.Slug != boardView.Slug {
		t.Errorf("expected board view slug %q, got %q", boardView.Slug, board.Slug)
	}
	if got, _ := board.Config["group_by"].(string); got != "status" {
		t.Errorf("expected board config group_by 'status', got %#v", board.Config)
	}
	if resp.Collection.Views[1].ViewType != "table" {
		t.Errorf("expected second view type 'table', got %q", resp.Collection.Views[1].ViewType)
	}

	// items: content included for inline expand.
	if len(resp.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(resp.Items))
	}
	it := resp.Items[0]
	if it.Ref == "" {
		t.Error("expected item ref")
	}
	if it.Content != "# Ship it\n\nDetailed body for inline expand." {
		t.Errorf("expected item content to be included, got %q", it.Content)
	}

	// Guard against leaking internal fields in the raw JSON body.
	body := rr.Body.String()
	for _, forbidden := range []string{`"id"`, `"workspace_id"`, `"created_by"`, `"created_at"`, `"updated_at"`, "content_template", "quick_actions"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("public share payload leaks forbidden token %q: %s", forbidden, body)
		}
	}
}

// TestResolveCollectionShareLink_NoViewsEmptyArray pins TASK-1681's empty
// contract: a collection with no saved views emits `views: []` (not null), so
// the read-only switcher (TASK-1682) can reliably fall back to
// settings.default_view without a null check.
func TestResolveCollectionShareLink_NoViewsEmptyArray(t *testing.T) {
	srv := testServer(t)
	slug := createWSForTest(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":   "Notes",
		"prefix": "NOTE",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)

	owner, err := srv.store.CreateUser(models.UserCreate{Email: "noviews@test.com", Name: "Owner", Password: "pw-owner"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("get workspace: %v", err)
	}
	link, err := srv.store.CreateShareLink(ws.ID, "collection", coll.ID, "view", owner.ID, nil)
	if err != nil {
		t.Fatalf("create share link: %v", err)
	}

	rr = doRequest(srv, "GET", "/api/v1/s/"+link.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve share link: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// `views` must be present as an empty array, not null/absent.
	if !strings.Contains(rr.Body.String(), `"views":[]`) {
		t.Errorf("expected empty views array in payload, got: %s", rr.Body.String())
	}

	var resp struct {
		Collection struct {
			Views []any `json:"views"`
		} `json:"collection"`
	}
	parseJSON(t, rr, &resp)
	if resp.Collection.Views == nil {
		t.Error("expected non-nil empty views slice")
	}
	if len(resp.Collection.Views) != 0 {
		t.Errorf("expected 0 views, got %d", len(resp.Collection.Views))
	}
}
