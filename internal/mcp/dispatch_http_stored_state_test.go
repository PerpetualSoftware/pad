package mcp

// Codex round 2 nit on BUG-2675 — the classifier test called
// structuredAppendErrorResult directly, so deleting either CALL SITE in
// dispatchItemNote / dispatchItemDecide would have left it green. This drives
// the real dispatcher against the real server and store, which is the only
// thing that proves the call sites are wired.
//
// It also covers the half the unit test structurally cannot: that the refusal
// happens BEFORE the PATCH. The destructive act is the write, not the error.

import (
	"context"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

type unreadableFixture struct {
	store     *store.Store
	srv       *server.Server
	owner     *models.User
	workspace *models.Workspace
	item      *models.Item
}

// newUnreadableFixture builds an item in the exact defect state: the entries
// array stored as a JSON-encoded STRING under both structured keys.
func newUnreadableFixture(t *testing.T) *unreadableFixture {
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
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Legacy WS", Slug: "legacy-ws", OwnerID: owner.ID})
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

	fields := `{"status":"open",` +
		`"implementation_notes":"[{\"summary\":\"legacy note\"}]",` +
		`"decision_log":"[{\"decision\":\"legacy decision\"}]"}`
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Legacy row", Fields: fields})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// Precondition. If the store normalized the value, everything below would
	// be testing a healthy item and passing for the wrong reason.
	if !isEncodedString(t, item.Fields, models.ItemFieldImplementationNotes) {
		t.Fatalf("fixture did not reproduce the defect shape: %s", item.Fields)
	}

	return &unreadableFixture{store: s, srv: srv, owner: owner, workspace: ws, item: item}
}

func isEncodedString(t *testing.T, fieldsJSON, key string) bool {
	t.Helper()
	return !models.StructuredFieldIsAppendable(fieldsJSON, key)
}

func (f *unreadableFixture) dispatch(t *testing.T, cmd []string, input map[string]any) ErrorEnvelope {
	t.Helper()
	d := &HTTPHandlerDispatcher{
		Handler:      f.srv,
		UserResolver: func(context.Context) *models.User { return f.owner },
	}
	res, err := d.Dispatch(WithDispatchInput(context.Background(), input), cmd, nil)
	if err != nil {
		t.Fatalf("Dispatch(%v): %v", cmd, err)
	}
	if !res.IsError {
		t.Fatalf("Dispatch(%v) SUCCEEDED against an unreadable stored value — that append destroys it: %s",
			cmd, textOf(res))
	}
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("expected ErrorEnvelope, got %T", res.StructuredContent)
	}
	return env
}

func TestDispatchItemNote_RefusesUnreadableWithRetryHostileCode(t *testing.T) {
	f := newUnreadableFixture(t)

	env := f.dispatch(t, []string{"item", "note"}, map[string]any{
		"workspace": f.workspace.Slug,
		"ref":       f.item.Slug,
		"summary":   "a new note",
	})

	if env.Error.Code != ErrStoredStateUnreadable {
		t.Errorf("code: got %q, want %q — server_error invites the retry this code exists to stop",
			env.Error.Code, ErrStoredStateUnreadable)
	}
	if !strings.Contains(strings.ToLower(env.Error.Hint), "retry") {
		t.Errorf("hint must say retrying is pointless; got: %s", env.Error.Hint)
	}

	// The stored value must be untouched — the refusal exists to protect it,
	// and "an error came back" is also true of a build that errored after
	// writing.
	after, err := f.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if after.Fields != f.item.Fields {
		t.Fatalf("refused note still mutated the item's fields:\n before: %s\n after:  %s",
			f.item.Fields, after.Fields)
	}
}

func TestDispatchItemDecide_RefusesUnreadableWithRetryHostileCode(t *testing.T) {
	f := newUnreadableFixture(t)

	env := f.dispatch(t, []string{"item", "decide"}, map[string]any{
		"workspace": f.workspace.Slug,
		"ref":       f.item.Slug,
		"decision":  "a new decision",
	})

	if env.Error.Code != ErrStoredStateUnreadable {
		t.Errorf("code: got %q, want %q", env.Error.Code, ErrStoredStateUnreadable)
	}
	after, err := f.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if after.Fields != f.item.Fields {
		t.Fatalf("refused decision still mutated the item's fields:\n before: %s\n after:  %s",
			f.item.Fields, after.Fields)
	}
}

// TestDispatchItemNote_HealthyItemStillAppends is the over-breadth control: the
// two tests above would also pass against a dispatcher that refused every note.
func TestDispatchItemNote_HealthyItemStillAppends(t *testing.T) {
	f := newUnreadableFixture(t)

	healthy, err := f.store.CreateItem(f.workspace.ID, f.item.CollectionID, models.ItemCreate{
		Title:  "Healthy row",
		Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	d := &HTTPHandlerDispatcher{
		Handler:      f.srv,
		UserResolver: func(context.Context) *models.User { return f.owner },
	}
	input := map[string]any{
		"workspace": f.workspace.Slug,
		"ref":       healthy.Slug,
		"summary":   "a new note",
	}
	res, err := d.Dispatch(WithDispatchInput(context.Background(), input), []string{"item", "note"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.IsError {
		t.Fatalf("note on a healthy item must succeed: %s", textOf(res))
	}

	after, err := f.store.GetItem(healthy.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if notes := models.ExtractItemImplementationNotes(after.Fields); len(notes) != 1 {
		t.Fatalf("expected the note to persist, got %d entries: %s", len(notes), after.Fields)
	}
}
