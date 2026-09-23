package mcp

import (
	"context"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-3056, codex round 1: moving the append server-side must not change who
// the remote door says wrote an entry. It has always stamped the requesting
// user's display name; the server's own stamp for an in-process request
// (which carries no X-Pad-Agent) would be "user". Driven through the real
// handler and store, because the label now crosses that boundary in a context
// value and a stub cannot show it arrives.
func TestDispatchNoteAndDecide_KeepTheUsersNameAsAuthor(t *testing.T) {
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)

	owner, err := s.CreateUser(models.UserCreate{Email: "owner@example.com", Name: "Owner Name", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Author WS", Slug: "author-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Target", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	d := &HTTPHandlerDispatcher{Handler: srv, UserResolver: func(context.Context) *models.User { return owner }}
	for _, c := range []struct {
		cmd   []string
		input map[string]any
	}{
		{[]string{"item", "note"}, map[string]any{"summary": "a note"}},
		{[]string{"item", "decide"}, map[string]any{"decision": "a decision"}},
	} {
		c.input["workspace"] = ws.Slug
		c.input["ref"] = item.Slug
		res, err := d.Dispatch(WithDispatchInput(context.Background(), c.input), c.cmd, nil)
		if err != nil || res.IsError {
			t.Fatalf("%v: err=%v result=%s", c.cmd, err, textOf(res))
		}
	}

	got, err := s.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	notes := models.ExtractItemImplementationNotes(got.Fields)
	if len(notes) != 1 || notes[0].CreatedBy != "Owner Name" {
		t.Errorf("note created_by = %+v, want the user's name", notes)
	}
	log := models.ExtractItemDecisionLog(got.Fields)
	if len(log) != 1 || log[0].CreatedBy != "Owner Name" {
		t.Errorf("decision created_by = %+v, want the user's name", log)
	}
}
