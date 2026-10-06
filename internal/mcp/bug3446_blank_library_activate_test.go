package mcp

// BUG-3446, the MCP door: an agent running the onboard playbook over remote
// MCP in a `blank` workspace activates every library convention and playbook
// with pad_library.activate, and B3's literal create (pad_item create with a
// `field: ["trigger=…"]`) succeeds. Driven through the real dispatcher and the
// real server and store, for the reason given in
// dispatch_http_collection_alias_test.go; that file's SCOPE note applies here
// too (no transport, no OAuth).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func TestBUG3446_MCPActivatesEveryLibraryEntryInABlankWorkspace(t *testing.T) {
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)

	owner, err := s.CreateUser(models.UserCreate{Email: "blank-3446@example.com", Name: "Owner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Blank 3446", Slug: "blank-3446", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := s.SeedCollectionsFromTemplate(ws.ID, "blank"); err != nil {
		t.Fatalf("SeedCollectionsFromTemplate: %v", err)
	}
	d := &HTTPHandlerDispatcher{Handler: srv, UserResolver: func(context.Context) *models.User { return owner }}

	call := func(args []string, input map[string]any) (string, bool) {
		t.Helper()
		res, err := d.Dispatch(WithDispatchInput(context.Background(), input), args, nil)
		if err != nil {
			t.Fatalf("Dispatch %v: %v", args, err)
		}
		return textOf(res), res.IsError
	}

	// A word outside the library is still refused over MCP.
	text, isErr := call([]string{"item", "create"}, map[string]any{
		"workspace": ws.Slug, "collection": "conventions", "title": "Invented",
		"field": []any{"trigger=on-experiment-run", "status=active"},
	})
	if !isErr || !strings.Contains(text, "on-experiment-run") {
		t.Errorf("an out-of-vocabulary trigger over MCP: isError=%v %s", isErr, text)
	}

	seeded := map[string]bool{}
	items, err := s.ListItems(ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, it := range items {
		seeded[it.Title] = true
	}

	for _, cat := range collections.ConventionLibrary() {
		for _, c := range cat.Conventions {
			if text, isErr := call([]string{"library", "activate"}, map[string]any{"workspace": ws.Slug, "title": c.Title}); isErr {
				t.Errorf("pad_library.activate %q (trigger %s): %s", c.Title, c.Trigger, text)
			}
		}
	}
	for _, cat := range collections.PlaybookLibrary() {
		for _, p := range cat.Playbooks {
			if seeded[p.Title] {
				continue // the seeded onboard playbook; the library shows it as active
			}
			if text, isErr := call([]string{"library", "activate"}, map[string]any{"workspace": ws.Slug, "title": p.Title}); isErr {
				t.Errorf("pad_library.activate playbook %q (trigger %s): %s", p.Title, p.Trigger, text)
			}
		}
	}

	// B3's literal step over MCP. The activations above already added
	// on-task-complete, so this only has to succeed; the report on the first
	// adding write is TestBUG3446_MCPCreateReportsOptionsAdded below.
	text, isErr = call([]string{"item", "create"}, map[string]any{
		"workspace": ws.Slug, "collection": "conventions", "title": "Run make test before done",
		"field": []any{"trigger=on-task-complete", "status=active"},
	})
	if isErr {
		t.Fatalf("pad_item create with a library trigger: %s", text)
	}

	coll, err := s.GetCollectionBySlug(ws.ID, "conventions")
	if err != nil || coll == nil {
		t.Fatalf("GetCollectionBySlug: %v", err)
	}
	var schema models.CollectionSchema
	if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	for _, f := range schema.Fields {
		if f.Key == "trigger" {
			for _, o := range f.Options {
				if o == "on-experiment-run" {
					t.Errorf("an out-of-vocabulary word reached the options: %v", f.Options)
				}
			}
		}
	}
}

// The first MCP write that adds a word tells the agent so.
func TestBUG3446_MCPCreateReportsOptionsAdded(t *testing.T) {
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	owner, err := s.CreateUser(models.UserCreate{Email: "blank-3446b@example.com", Name: "Owner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Blank 3446b", Slug: "blank-3446b", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := s.SeedCollectionsFromTemplate(ws.ID, "blank"); err != nil {
		t.Fatalf("SeedCollectionsFromTemplate: %v", err)
	}
	d := &HTTPHandlerDispatcher{Handler: srv, UserResolver: func(context.Context) *models.User { return owner }}
	res, err := d.Dispatch(WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "collection": "conventions", "title": "Commit often",
		"field": []any{"trigger=on-commit", "status=active"},
	}), []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.IsError {
		t.Fatalf("create: %s", textOf(res))
	}
	if text := textOf(res); !strings.Contains(text, `"options_added"`) || !strings.Contains(text, "on-commit") {
		t.Errorf("the create result does not report the option it added: %s", text)
	}
}
