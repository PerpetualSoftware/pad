package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// TASK-2695. edit-comment / delete-comment through the REMOTE door, end to
// end: raw JSON-RPC → catalog → HTTPHandlerDispatcher → the real handler
// chain → the store. The wrong-item rows are the point: the comment-is-on-ref
// check lives on the server, so a dispatcher that addressed the
// workspace-scoped route instead would pass the happy rows and fail these.
func TestCommentEditDelete_Remote(t *testing.T) {
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	owner, err := s.CreateUser(models.UserCreate{Email: "o@example.com", Name: "O", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "W", Slug: "w2695", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TASK", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	itemA, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "A"})
	if err != nil {
		t.Fatal(err)
	}
	itemB, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "B"})
	if err != nil {
		t.Fatal(err)
	}
	onA, err := s.CreateComment(ws.ID, itemA.ID, owner.ID, models.CommentCreate{Body: "on A", CreatedBy: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	onB, err := s.CreateComment(ws.ID, itemB.ID, owner.ID, models.CommentCreate{Body: "on B", CreatedBy: "agent"})
	if err != nil {
		t.Fatal(err)
	}

	m := mcpserver.NewMCPServer("t", "1", mcpserver.WithToolCapabilities(true))
	d := &HTTPHandlerDispatcher{Handler: srv, UserResolver: func(context.Context) *models.User { return owner }}
	if _, err := RegisterCatalog(m, CatalogOptions{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState(ws.Slug), Dispatcher: d, PadVersion: "test"}); err != nil {
		t.Fatal(err)
	}
	call := func(id int, args string) string {
		t.Helper()
		return respText(t, m.HandleMessage(context.Background(), toolsCall(id, args)))
	}
	body := func(id string) string {
		t.Helper()
		c, err := s.GetComment(id)
		if err != nil {
			t.Fatal(err)
		}
		if c == nil {
			return "<deleted>"
		}
		return c.Body
	}

	refA := fmt.Sprintf("TASK-%d", *itemA.ItemNumber)

	resp := call(1, fmt.Sprintf(`{"action":"edit-comment","ref":%q,"comment_id":%q,"message":"on A, fixed"}`, refA, onA.ID))
	if strings.Contains(resp, `"isError":true`) {
		t.Fatalf("edit through the right item failed: %s", resp)
	}
	if got := body(onA.ID); got != "on A, fixed" {
		t.Fatalf("edit did not land: body = %q", got)
	}

	resp = call(2, fmt.Sprintf(`{"action":"edit-comment","ref":%q,"comment_id":%q,"message":"hijacked"}`, refA, onB.ID))
	if !strings.Contains(resp, `"isError":true`) {
		t.Fatalf("edit of B's comment through A succeeded: %s", resp)
	}
	if got := body(onB.ID); got != "on B" {
		t.Fatalf("B's comment changed through A: %q", got)
	}

	resp = call(3, fmt.Sprintf(`{"action":"delete-comment","ref":%q,"comment_id":%q}`, refA, onB.ID))
	if !strings.Contains(resp, `"isError":true`) {
		t.Fatalf("delete of B's comment through A succeeded: %s", resp)
	}
	if got := body(onB.ID); got != "on B" {
		t.Fatalf("B's comment deleted through A: %q", got)
	}

	resp = call(4, fmt.Sprintf(`{"action":"list-comments","ref":%q}`, refA))
	if !strings.Contains(resp, `\"edited\":`) {
		t.Fatalf("list-comments rows carry no edited key: %s", resp)
	}

	resp = call(5, fmt.Sprintf(`{"action":"delete-comment","ref":%q,"comment_id":%q}`, refA, onA.ID))
	if strings.Contains(resp, `"isError":true`) {
		t.Fatalf("delete through the right item failed: %s", resp)
	}
	if !strings.Contains(resp, `\"deleted\":true`) || !strings.Contains(resp, onA.ID) {
		t.Fatalf("delete result is not the CLI's JSON shape: %s", resp)
	}
	if got := body(onA.ID); got != "<deleted>" {
		t.Fatalf("comment still present after delete: %q", got)
	}
}

// TestDeleteCommentWithReplies_Remote pins BUG-3252 on the remote door:
// delete-comment on a comment with replies succeeds and leaves a tombstone
// the reply still hangs off, and an edit-comment addressed to that tombstone
// arrives as comment_deleted with the shared hint, not as server_error.
func TestDeleteCommentWithReplies_Remote(t *testing.T) {
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	owner, err := s.CreateUser(models.UserCreate{Email: "o@example.com", Name: "O", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "W", Slug: "w3252", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TASK", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "A"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.CreateComment(ws.ID, item.ID, owner.ID, models.CommentCreate{Body: "parent", CreatedBy: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := s.CreateComment(ws.ID, item.ID, owner.ID, models.CommentCreate{Body: "reply", CreatedBy: "agent", ParentID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}

	m := mcpserver.NewMCPServer("t", "1", mcpserver.WithToolCapabilities(true))
	d := &HTTPHandlerDispatcher{Handler: srv, UserResolver: func(context.Context) *models.User { return owner }}
	if _, err := RegisterCatalog(m, CatalogOptions{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState(ws.Slug), Dispatcher: d, PadVersion: "test"}); err != nil {
		t.Fatal(err)
	}
	ref := fmt.Sprintf("TASK-%d", *item.ItemNumber)
	resp := respText(t, m.HandleMessage(context.Background(),
		toolsCall(1, fmt.Sprintf(`{"action":"delete-comment","ref":%q,"comment_id":%q}`, ref, parent.ID))))
	if strings.Contains(resp, `"isError":true`) {
		t.Fatalf("delete-comment on a comment with replies failed: %s", resp)
	}
	if c, err := s.GetComment(parent.ID); err != nil || c == nil || !c.Deleted || c.Body != "" {
		t.Fatalf("parent = %+v (err %v), want a tombstone", c, err)
	}
	if c, err := s.GetComment(reply.ID); err != nil || c == nil || c.ParentID != parent.ID {
		t.Fatalf("reply = %+v (err %v), want it under its parent", c, err)
	}

	resp = respText(t, m.HandleMessage(context.Background(),
		toolsCall(2, fmt.Sprintf(`{"action":"edit-comment","ref":%q,"comment_id":%q,"message":"revived"}`, ref, parent.ID))))
	for _, want := range []string{`"isError":true`, `comment_deleted`, "placeholder for its replies"} {
		if !strings.Contains(resp, want) {
			t.Fatalf("edit of a tombstone: response lacks %s: %s", want, resp)
		}
	}
}

// TestCommentEditDelete_Stdio pins the local door's argv: the actions reach
// the CLI verbs with ref, comment id and body as positionals behind `--`, so
// a body that starts with "-" is not parsed as a flag (the v0.42 rule).
func TestCommentEditDelete_Stdio(t *testing.T) {
	fd := &fakeDispatcher{}
	m := mcpserver.NewMCPServer("t", "1", mcpserver.WithToolCapabilities(true))
	if _, err := RegisterCatalog(m, CatalogOptions{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("ws"), Dispatcher: fd, PadVersion: "test"}); err != nil {
		t.Fatal(err)
	}

	resp := respText(t, m.HandleMessage(context.Background(),
		toolsCall(1, `{"action":"edit-comment","ref":"TASK-1","comment_id":"c-1","message":"- corrected"}`)))
	if strings.Contains(resp, `"isError":true`) {
		t.Fatalf("edit-comment failed: %s", resp)
	}
	if got := strings.Join(fd.gotPath, " "); got != "item comment-edit" {
		t.Fatalf("edit-comment dispatched to %q", got)
	}
	if got := strings.Join(fd.gotArgs, "\x00"); !strings.HasSuffix(got, strings.Join([]string{"--", "TASK-1", "c-1", "- corrected"}, "\x00")) {
		t.Fatalf("edit-comment argv = %q", fd.gotArgs)
	}

	resp = respText(t, m.HandleMessage(context.Background(),
		toolsCall(2, `{"action":"delete-comment","ref":"TASK-1","comment_id":"c-1"}`)))
	if strings.Contains(resp, `"isError":true`) {
		t.Fatalf("delete-comment failed: %s", resp)
	}
	if got := strings.Join(fd.gotPath, " "); got != "item comment-delete" {
		t.Fatalf("delete-comment dispatched to %q", got)
	}
	if got := strings.Join(fd.gotArgs, "\x00"); !strings.HasSuffix(got, strings.Join([]string{"--", "TASK-1", "c-1"}, "\x00")) {
		t.Fatalf("delete-comment argv = %q", fd.gotArgs)
	}
}
