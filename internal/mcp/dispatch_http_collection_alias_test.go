package mcp

// BUG-2578, the MCP half of the claim. The bug's body says an agent hitting a
// non-default collection's singular over remote MCP gets "Collection not
// found" for a collection that plainly exists in its own bootstrap payload.
// The fix lives on the SERVER, so proving it reaches MCP means driving the
// REAL server and store rather than a recording handler — a stub can only show
// which URL the dispatcher built, which is a claim about the dispatcher, not
// about what an agent receives. (Same rationale as the item-list summary
// tests in this package.)
//
// SCOPE, stated so nobody reads more into it: this exercises the dispatcher
// against a real server in-process. It does NOT go over the remote /mcp HTTP
// transport or its OAuth layer, so it proves the resolution reaches an MCP
// tool call, not that the transport in front of it is correct.

import (
	"context"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// newAliasFixture builds a workspace whose collections include `specs` — a
// name the client-side alias map has never heard of — and returns a
// dispatcher wired to the real server.
func newAliasFixture(t *testing.T) *HTTPHandlerDispatcher {
	t.Helper()
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)

	owner, err := s.CreateUser(models.UserCreate{
		Email:    "alias-owner@example.com",
		Name:     "Owner",
		Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{
		Name: "Alias WS", Slug: "alias-ws", OwnerID: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if _, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Specs", Slug: "specs", Prefix: "SPEC",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	}); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	return &HTTPHandlerDispatcher{
		Handler:      srv,
		UserResolver: func(context.Context) *models.User { return owner },
	}
}

// The reported symptom, end to end over the transport an agent actually uses.
func TestHTTPItemCreate_AcceptsSingularOfANonDefaultCollection(t *testing.T) {
	d := newAliasFixture(t)

	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":  "alias-ws",
		"collection": "spec",
		"title":      "created over MCP",
	})
	res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch(item create): %v", err)
	}
	if res.IsError {
		t.Fatalf("creating in `spec` over MCP failed — the reported bug: %s", textOf(res))
	}
	if text := textOf(res); !strings.Contains(text, "created over MCP") {
		t.Errorf("create result does not describe the created item: %s", text)
	}

	// It has to be READABLE by the same shorthand, or the agent can create
	// something it cannot then list.
	listCtx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":  "alias-ws",
		"collection": "spec",
	})
	listRes, err := d.Dispatch(listCtx, []string{"item", "list"}, nil)
	if err != nil {
		t.Fatalf("Dispatch(item list): %v", err)
	}
	if listRes.IsError {
		t.Fatalf("listing `spec` over MCP failed: %s", textOf(listRes))
	}
	if text := textOf(listRes); !strings.Contains(text, "created over MCP") {
		t.Errorf("listing by the singular returned nothing: %s", text)
	}
}

// A collection that does not exist under any spelling must still fail, so the
// fallback cannot be mistaken for "any name works".
func TestHTTPItemCreate_UnknownCollectionStillFails(t *testing.T) {
	d := newAliasFixture(t)

	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":  "alias-ws",
		"collection": "definitely-not-a-collection",
		"title":      "nope",
	})
	res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.IsError {
		t.Errorf("creating in an unknown collection succeeded: %s", textOf(res))
	}
}
