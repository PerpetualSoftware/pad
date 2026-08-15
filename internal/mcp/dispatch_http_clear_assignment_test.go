package mcp

// TASK-2571 — an MCP agent must be able to UNASSIGN an item.
//
// Two layers of the MCP dispatch path used to drop an empty-string
// assignment value before the request body was built, so
// `assigned_user_id=""` (and `--field assigned_user_id=`) was a silent
// no-op rather than a clear:
//
//   - mapItemUpdate's top-level pass-through (dispatch_http_advanced.go)
//   - liftFieldsToColumns (dispatch_http.go), which lifts `--field`
//     entries onto their columns
//
// Both filters were correct when written — `""` had no defined meaning
// at the store and bound an empty string into a FK column. BUG-2566 gave
// `""` clear-to-NULL semantics for exactly `assigned_user_id` and
// `agent_role_id`, and the HTTP surface inherited it, which left MCP the
// only surface with no way to unassign.
//
// These tests drive the REAL server + store, not a recording handler.
// Asserting only that the dispatcher puts `assigned_user_id: ""` in the
// payload would restate the fix rather than test it: the claim is that
// the item ends up unassigned, and only the full path can show that.
// internal/store/items_empty_assignment_test.go covers the store half.

import (
	"context"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// clearFixture is one workspace with an owner, a collection, a role, and
// an item assigned to the owner AND carrying that role — the state an
// agent would be trying to undo.
type clearFixture struct {
	store     *store.Store
	srv       *server.Server
	owner     *models.User
	workspace *models.Workspace
	item      *models.Item
	role      *models.AgentRole
}

func newClearFixture(t *testing.T) *clearFixture {
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

	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Clear WS", Slug: "clear-ws", OwnerID: owner.ID})
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

	role, err := s.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Implementer"})
	if err != nil {
		t.Fatalf("CreateAgentRole: %v", err)
	}

	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		Title:          "Assigned to somebody",
		AssignedUserID: &owner.ID,
		AgentRoleID:    &role.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if item.AssignedUserID == nil || item.AgentRoleID == nil {
		t.Fatalf("fixture precondition: item must start assigned AND roled, got user=%v role=%v",
			item.AssignedUserID, item.AgentRoleID)
	}

	return &clearFixture{store: s, srv: srv, owner: owner, workspace: ws, item: item, role: role}
}

// dispatch runs one MCP tool call through the in-process HTTP dispatcher
// against the real server, as the fixture's owner.
func (f *clearFixture) dispatch(t *testing.T, cmd []string, input map[string]any) {
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

func (f *clearFixture) reload(t *testing.T) *models.Item {
	t.Helper()
	got, err := f.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	return got
}

// TestMCPUpdate_EmptyAssignedUserIDClearsAssignment covers the top-level
// pass-through in mapItemUpdate.
func TestMCPUpdate_EmptyAssignedUserIDClearsAssignment(t *testing.T) {
	f := newClearFixture(t)

	f.dispatch(t, []string{"item", "update"}, map[string]any{
		"workspace":        f.workspace.Slug,
		"ref":              f.item.Slug,
		"assigned_user_id": "",
	})

	got := f.reload(t)
	if got.AssignedUserID != nil {
		t.Fatalf("assignment should be cleared, got %q", *got.AssignedUserID)
	}
	// The role must be untouched: the caller asked to clear ONE column,
	// and an update that also wiped the other would be a far worse bug
	// than the no-op it replaces.
	if got.AgentRoleID == nil || *got.AgentRoleID != f.role.ID {
		t.Fatalf("role should be untouched, got %v want %q", got.AgentRoleID, f.role.ID)
	}
}

// TestMCPUpdate_EmptyAgentRoleIDClearsRole is the sibling column.
func TestMCPUpdate_EmptyAgentRoleIDClearsRole(t *testing.T) {
	f := newClearFixture(t)

	f.dispatch(t, []string{"item", "update"}, map[string]any{
		"workspace":     f.workspace.Slug,
		"ref":           f.item.Slug,
		"agent_role_id": "",
	})

	got := f.reload(t)
	if got.AgentRoleID != nil {
		t.Fatalf("role should be cleared, got %q", *got.AgentRoleID)
	}
	if got.AssignedUserID == nil || *got.AssignedUserID != f.owner.ID {
		t.Fatalf("assignment should be untouched, got %v want %q", got.AssignedUserID, f.owner.ID)
	}
}

// TestMCPUpdate_EmptyAssignmentViaFieldFlagClears covers the OTHER
// filter — liftFieldsToColumns — which is the path an agent actually
// reaches today: the catalog exposes `assign` (a name) and `field`, but
// no `assigned_user_id` param, so `field: ["assigned_user_id="]` is the
// only schema-visible way to ask for a clear.
func TestMCPUpdate_EmptyAssignmentViaFieldFlagClears(t *testing.T) {
	f := newClearFixture(t)

	f.dispatch(t, []string{"item", "update"}, map[string]any{
		"workspace": f.workspace.Slug,
		"ref":       f.item.Slug,
		"field":     []any{"assigned_user_id=", "agent_role_id="},
	})

	got := f.reload(t)
	if got.AssignedUserID != nil {
		t.Fatalf("assignment should be cleared via --field, got %q", *got.AssignedUserID)
	}
	if got.AgentRoleID != nil {
		t.Fatalf("role should be cleared via --field, got %q", *got.AgentRoleID)
	}
	// The lifted keys must not ALSO be left behind in the fields blob —
	// that was liftFieldsToColumns' original job and the empty-string
	// path must not skip it.
	if fields := got.Fields; strings.Contains(fields, "assigned_user_id") || strings.Contains(fields, "agent_role_id") {
		t.Fatalf("column keys leaked into the fields blob: %s", fields)
	}
}

// TestMCPUpdate_EmptyTagsStillFiltered is the control leg, and the
// reason this change is narrow rather than a blanket "stop filtering
// empty strings".
//
// `tags: ""` is NOT a clear — it is a corrupt write into a JSONB column
// on Postgres and a TEXT column on SQLite (codex #547 r3 P2). The guard
// that drops it sits three lines above the two this task changed and
// looks identical; the justification is opposite. Without this test,
// "unify the empty-string handling" is a plausible-looking future edit
// with no failing test to stop it.
func TestMCPUpdate_EmptyTagsStillFiltered(t *testing.T) {
	f := newClearFixture(t)

	if _, err := f.store.UpdateItem(f.item.ID, models.ItemUpdate{Tags: strPtr(`["keep-me"]`)}); err != nil {
		t.Fatalf("seed tags: %v", err)
	}

	f.dispatch(t, []string{"item", "update"}, map[string]any{
		"workspace": f.workspace.Slug,
		"ref":       f.item.Slug,
		"tags":      "",
	})

	got := f.reload(t)
	if !strings.Contains(got.Tags, "keep-me") {
		t.Fatalf("empty-string tags must be dropped, not applied; tags = %q", got.Tags)
	}
}

func strPtr(s string) *string { return &s }

// TestMCPUpdate_EmptyAssignAliasIsStillANoOp pins the deliberate LIMIT of
// TASK-2571's fix, so the next reader (or the next reviewer) finds a
// decision here rather than an oversight.
//
// `assign` is schema-declared; `assigned_user_id` is not. Every other
// schema-declared string on this mapper — title, content, comment, tags —
// treats empty as NOT PROVIDED, so a client that fills declared optional
// params with "" is harmless today. Making `assign: ""` mean "clear" would
// turn that same client into one that silently unassigns every item it
// touches. The gap is real and the remedy is explicit clear_* params
// (option (b), deferred), not a destructive meaning on an optional string.
//
// If a future change DOES make the alias clear, this test fails — which is
// the point: it should be a decision, not a drive-by.
func TestMCPUpdate_EmptyAssignAliasIsStillANoOp(t *testing.T) {
	f := newClearFixture(t)

	f.dispatch(t, []string{"item", "update"}, map[string]any{
		"workspace": f.workspace.Slug,
		"ref":       f.item.Slug,
		"assign":    "",
		"role":      "",
	})

	got := f.reload(t)
	if got.AssignedUserID == nil || *got.AssignedUserID != f.owner.ID {
		t.Fatalf("empty `assign` must not clear the assignment; got %v", got.AssignedUserID)
	}
	if got.AgentRoleID == nil || *got.AgentRoleID != f.role.ID {
		t.Fatalf("empty `role` must not clear the role; got %v", got.AgentRoleID)
	}
}
