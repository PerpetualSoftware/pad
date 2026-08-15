package mcp

// BUG-2078 — an MCP agent must be able to DETACH an item from its parent.
//
// The server has honoured a present-but-empty "parent" key in fields_patch
// as "clear the link" since BUG-2013 (extractParentLink in
// internal/server/handlers_items.go), and internal/store/items_test.go's
// TestUpdateItemWithParentLink_AtomicCommit already pins the store half.
// Neither client surface could reach it: `pad item update --parent ""` was
// a silent no-op (cmd/pad/cmd_item.go's `if parentRef != "" {...}` guard),
// and the MCP `parent` param is a plain string with the same "empty means
// not provided" convention every other declared string on this tool has.
//
// `clear_parent` is the discoverable boolean, same shape as
// `clear_assigned_user` / `clear_agent_role` (IDEA-2584,
// dispatch_http_clear_assignment_test.go) — except the clear signal has to
// land in fields_patch["parent"], not a top-level ItemUpdate column,
// because "parent" is itself a fields_patch pseudo-key. These tests drive
// the REAL server + store, not a recording handler, for the same reason the
// assignment tests do: the claim is that the item ends up unparented, not
// merely that the dispatcher shaped a particular payload.

import (
	"context"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// parentFixture is one workspace with an owner, a collection, a parent item,
// and a child item already linked under it — the state an agent would be
// trying to undo.
type parentFixture struct {
	store     *store.Store
	srv       *server.Server
	owner     *models.User
	workspace *models.Workspace
	parent    *models.Item
	child     *models.Item
}

func newParentFixture(t *testing.T) *parentFixture {
	t.Helper()
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)

	owner, err := s.CreateUser(models.UserCreate{
		Email:    "owner@example.com",
		Name:     "Owner",
		Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Parent WS", Slug: "parent-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}

	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Slug:   "tasks",
		Prefix: "TASK",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	parent, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Parent item"})
	if err != nil {
		t.Fatalf("CreateItem (parent): %v", err)
	}
	child, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Child item"})
	if err != nil {
		t.Fatalf("CreateItem (child): %v", err)
	}
	if _, err := s.UpdateItemWithParentLink(
		child.ID, models.ItemUpdate{}, nil,
		&store.ParentLinkUpdate{Provided: true, ParentID: parent.ID, WorkspaceID: ws.ID, CreatedBy: "user"},
	); err != nil {
		t.Fatalf("seed parent link: %v", err)
	}
	if link, err := s.GetParentForItem(child.ID); err != nil || link == nil || link.TargetID != parent.ID {
		t.Fatalf("fixture precondition: child must start parented; link=%+v err=%v", link, err)
	}

	return &parentFixture{store: s, srv: srv, owner: owner, workspace: ws, parent: parent, child: child}
}

func (f *parentFixture) dispatch(t *testing.T, cmd []string, input map[string]any) {
	t.Helper()
	d := &HTTPHandlerDispatcher{
		Handler:      f.srv,
		UserResolver: func(context.Context) *models.User { return f.owner },
	}
	ctx := WithDispatchInput(context.Background(), input)
	res, err := d.Dispatch(ctx, cmd, nil)
	if err != nil {
		t.Fatalf("Dispatch(%v): %v", cmd, err)
	}
	if res.IsError {
		t.Fatalf("Dispatch(%v) returned an error result: %s", cmd, textOf(res))
	}
}

func (f *parentFixture) dispatchExpectingError(t *testing.T, cmd []string, input map[string]any) string {
	t.Helper()
	d := &HTTPHandlerDispatcher{
		Handler:      f.srv,
		UserResolver: func(context.Context) *models.User { return f.owner },
	}
	ctx := WithDispatchInput(context.Background(), input)
	res, err := d.Dispatch(ctx, cmd, nil)
	if err != nil {
		t.Fatalf("Dispatch(%v) errored at transport: %v", cmd, err)
	}
	if !res.IsError {
		t.Fatalf("Dispatch(%v) should have been refused, got success", cmd)
	}
	return textOf(res)
}

func (f *parentFixture) parentLink(t *testing.T) *models.ItemLink {
	t.Helper()
	link, err := f.store.GetParentForItem(f.child.ID)
	if err != nil {
		t.Fatalf("GetParentForItem: %v", err)
	}
	return link
}

func TestMCPUpdate_ClearParentBoolean(t *testing.T) {
	f := newParentFixture(t)

	f.dispatch(t, []string{"item", "update"}, map[string]any{
		"workspace":    f.workspace.Slug,
		"ref":          f.child.Slug,
		"clear_parent": true,
	})

	if link := f.parentLink(t); link != nil {
		t.Fatalf("parent link should be cleared, got %+v", link)
	}
}

// TestMCPUpdate_ClearParentFalseIsNotAClear is the same safety guard
// TestMCPUpdate_ClearFalseIsNotAClear pins for the assignment booleans: a
// client that fills every declared param with its zero value must not
// detach anything.
func TestMCPUpdate_ClearParentFalseIsNotAClear(t *testing.T) {
	f := newParentFixture(t)

	f.dispatch(t, []string{"item", "update"}, map[string]any{
		"workspace":    f.workspace.Slug,
		"ref":          f.child.Slug,
		"clear_parent": false,
		"title":        "Touched but still parented",
	})

	link := f.parentLink(t)
	if link == nil || link.TargetID != f.parent.ID {
		t.Fatalf("clear_parent=false must NOT clear; got %v", link)
	}
	got, err := f.store.GetItem(f.child.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Title != "Touched but still parented" {
		t.Fatalf("the update should still have applied; title = %q", got.Title)
	}
}

// TestMCPUpdate_EmptyParentIsStillANoOp is the control leg: unlike
// clear_parent, the plain `parent` string keeps its "empty means not
// provided" meaning. Pins the same limit TestMCPUpdate_EmptyAssignAliasIsStillANoOp
// pins for `assign`/`role` — if a future change makes `parent: ""` a clear,
// this fails, which is the point: it should be a decision, not a drive-by.
func TestMCPUpdate_EmptyParentIsStillANoOp(t *testing.T) {
	f := newParentFixture(t)

	f.dispatch(t, []string{"item", "update"}, map[string]any{
		"workspace": f.workspace.Slug,
		"ref":       f.child.Slug,
		"parent":    "",
	})

	link := f.parentLink(t)
	if link == nil || link.TargetID != f.parent.ID {
		t.Fatalf("empty `parent` must not clear the link; got %v", link)
	}
}

// TestMCPUpdate_ClearParentConflictIsRejected mirrors
// TestMCPUpdate_ClearConflictsAreRejected: both routes to a competing value
// are covered because they land in `patch["parent"]` at DIFFERENT points —
// the named `parent` param directly, and `field: ["parent=…"]` via the
// --field overlay, which runs before clear_parent's own check but after the
// named-flag loop.
func TestMCPUpdate_ClearParentConflictIsRejected(t *testing.T) {
	// A second item to use as the "competing" parent value.
	setupParent := func(t *testing.T, f *parentFixture) *models.Item {
		t.Helper()
		other, err := f.store.CreateItem(f.workspace.ID, f.parent.CollectionID, models.ItemCreate{Title: "Other parent"})
		if err != nil {
			t.Fatalf("CreateItem (other parent): %v", err)
		}
		return other
	}

	t.Run("direct parent + clear", func(t *testing.T) {
		f := newParentFixture(t)
		other := setupParent(t, f)

		msg := f.dispatchExpectingError(t, []string{"item", "update"}, map[string]any{
			"workspace":    f.workspace.Slug,
			"ref":          f.child.Slug,
			"parent":       other.Slug,
			"clear_parent": true,
		})
		if !strings.Contains(msg, "conflicts with") {
			t.Fatalf("unexpected refusal message: %s", msg)
		}
		if link := f.parentLink(t); link == nil || link.TargetID != f.parent.ID {
			t.Fatalf("a refused conflict must not touch the parent link; got %v", link)
		}
	})

	t.Run("lifted field parent + clear", func(t *testing.T) {
		f := newParentFixture(t)
		other := setupParent(t, f)

		msg := f.dispatchExpectingError(t, []string{"item", "update"}, map[string]any{
			"workspace":    f.workspace.Slug,
			"ref":          f.child.Slug,
			"field":        []any{"parent=" + other.Slug},
			"clear_parent": true,
		})
		if !strings.Contains(msg, "conflicts with") {
			t.Fatalf("unexpected refusal message: %s", msg)
		}
		if link := f.parentLink(t); link == nil || link.TargetID != f.parent.ID {
			t.Fatalf("a refused conflict must not touch the parent link; got %v", link)
		}
	})
}
