package main

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	mcpserver "github.com/PerpetualSoftware/pad/internal/mcp"
	"github.com/PerpetualSoftware/pad/internal/models"
	padserver "github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-3321 U3: every ChatGPT tool's response, driven through the REAL
// HTTP dispatcher and server over a fixture that holds every kind of thing
// that must not leak (an assignee with an email, a relation field, a reply,
// a link, versions, seeded playbooks), carries no UUID, no email and no id
// key, except the two comment handles reply_to needs.

var (
	uuidShaped  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	emailShaped = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

type projectionFixture struct {
	srv   *mcpserver.Server
	ws    string
	task1 string // a task assigned to a user with an email, with a relation field
	task2 string
	reply string // a comment id, the add_comment reply_to handle
}

func newProjectionFixture(t *testing.T) projectionFixture {
	t.Helper()
	s := storetest.NewSQLite(t)
	api := padserver.New(s)
	t.Cleanup(api.Stop)

	owner, err := s.CreateUser(models.UserCreate{Email: "owner@example.com", Name: "Olivia Owner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Acme Launch", Slug: "acme", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || tasks == nil {
		t.Fatalf("tasks collection: %v", err)
	}
	people, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "People", Slug: "people", Prefix: "PERS",
		Schema: `{"fields":[{"key":"role","type":"text"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	deliverables, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Deliverables", Slug: "deliverables", Prefix: "DLV",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"},{"key":"owner","type":"relation","collection":"people"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	person, err := s.CreateItem(ws.ID, people.ID, models.ItemCreate{Title: "Pat Person"})
	if err != nil {
		t.Fatal(err)
	}
	uid := owner.ID
	t1, err := s.CreateItem(ws.ID, deliverables.ID, models.ItemCreate{
		Title: "Login timeout bug", Content: "The session expires too early.",
		Fields: `{"status":"open","owner":"` + person.ID + `"}`, AssignedUserID: &uid,
	})
	if err != nil {
		t.Fatal(err)
	}
	t2, err := s.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: "Write onboarding checklist", Fields: `{"status":"open","priority":"high"}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: t2.ID, LinkType: "blocks"}, t1.ID); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateComment(ws.ID, t1.ID, owner.ID, models.CommentCreate{Body: "Seen in prod.", Author: "Olivia Owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateComment(ws.ID, t1.ID, owner.ID, models.CommentCreate{Body: "Fix incoming.", Author: "Olivia Owner", ParentID: c.ID}); err != nil {
		t.Fatal(err)
	}

	root := newRootCmd()
	doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", Version: fullVersion(), Homepage: padHomepage, MaxDepth: -1})
	srv, err := newChatGPTMCPServer(doc, &mcpserver.HTTPHandlerDispatcher{
		Handler:      api,
		UserResolver: func(context.Context) *models.User { return owner },
	})
	if err != nil {
		t.Fatal(err)
	}
	return projectionFixture{srv: srv, ws: ws.Slug, task1: t1.Ref, task2: t2.Ref, reply: c.ID}
}

func (f projectionFixture) call(t *testing.T, tool string, args map[string]any) (*mcp.CallToolResult, any) {
	t.Helper()
	raw, _ := json.Marshal(args)
	req := mcp.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	req.Params.RawArguments = raw
	st := f.srv.MCP().GetTool(tool)
	if st == nil {
		t.Fatalf("no tool %s", tool)
	}
	res, err := st.Handler(context.Background(), req)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var v any
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(b, &v)
	}
	return res, v
}

// leaks lists every UUID-shaped value, email-shaped string, and id/email
// key in v, by path. allowed names the comment handle keys.
func leaks(path string, v any, allowedIDKeys map[string]bool) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			p := path + "." + k
			if k == "email" || strings.HasSuffix(k, "_email") {
				out = append(out, p+" (email key)")
			}
			if (k == "id" || strings.HasSuffix(k, "_id")) && !allowedIDKeys[k] {
				out = append(out, p+" (id key)")
			}
			if allowedIDKeys[k] {
				continue
			}
			out = append(out, leaks(p, e, allowedIDKeys)...)
		}
	case []any:
		for i, e := range t {
			out = append(out, leaks(path+"["+string(rune('0'+i%10))+"]", e, allowedIDKeys)...)
		}
	case string:
		if uuidShaped.MatchString(t) {
			out = append(out, path+" (UUID value)")
		}
		if emailShaped.MatchString(t) {
			out = append(out, path+" (email value)")
		}
	}
	return out
}

func TestChatGPTProjection_NoIdentifiersOrEmailsLeak(t *testing.T) {
	f := newProjectionFixture(t)
	ws := f.ws
	calls := []struct {
		tool string
		args map[string]any
	}{
		{"list_workspaces", map[string]any{}},
		{"get_workspace_overview", map[string]any{"workspace": ws}},
		{"list_collections", map[string]any{"workspace": ws}},
		{"search", map[string]any{"workspace": ws, "query": "login"}},
		{"list_items", map[string]any{"workspace": ws}},
		{"get_item", map[string]any{"workspace": ws, "ref": f.task1}},
		{"create_item", map[string]any{"workspace": ws, "collection": "tasks", "title": "A new task"}},
		{"update_item", map[string]any{"workspace": ws, "ref": f.task1, "content": "Edited through ChatGPT."}},
		{"add_comment", map[string]any{"workspace": ws, "ref": f.task1, "message": "Thanks", "reply_to": f.reply}},
		{"list_comments", map[string]any{"workspace": ws, "ref": f.task1}},
		{"item_history", map[string]any{"workspace": ws, "ref": f.task1}},
		{"item_dependencies", map[string]any{"workspace": ws, "ref": f.task1}},
		{"link_items", map[string]any{"workspace": ws, "ref": f.task2, "target": f.task1, "link_type": "supersedes"}},
		{"project_dashboard", map[string]any{"workspace": ws}},
		{"what_next", map[string]any{"workspace": ws}},
		{"ready_items", map[string]any{"workspace": ws}},
		{"recent_activity", map[string]any{"workspace": ws}},
		{"list_playbooks", map[string]any{"workspace": ws}},
		{"get_playbook", map[string]any{"workspace": ws, "ref": "ship"}},
		{"archive_item", map[string]any{"workspace": ws, "ref": f.task2}},
		{"restore_item", map[string]any{"workspace": ws, "ref": f.task2}},
	}
	covered := map[string]bool{}
	for _, c := range calls {
		covered[c.tool] = true
		t.Run(c.tool, func(t *testing.T) {
			res, v := f.call(t, c.tool, c.args)
			if res.IsError {
				t.Fatalf("%s returned an error: %+v", c.tool, res.Content)
			}
			if v == nil {
				t.Fatalf("%s returned no structured content: %+v", c.tool, res.Content)
			}
			allowed := map[string]bool{}
			if c.tool == "add_comment" || c.tool == "list_comments" {
				allowed = map[string]bool{"id": true, "parent_id": true}
			}
			if l := leaks(c.tool, v, allowed); len(l) > 0 {
				sort.Strings(l)
				t.Errorf("leaks:\n  %s", strings.Join(l, "\n  "))
			}
			// The text fallback is the projected value, not the raw body.
			var text string
			for _, ct := range res.Content {
				if tc, ok := ct.(mcp.TextContent); ok {
					text = tc.Text
				}
			}
			if emailShaped.MatchString(text) || strings.Contains(text, `"workspace_id"`) {
				t.Errorf("the text fallback carries what the structured content dropped: %.300s", text)
			}
		})
	}
	for _, e := range mcpserver.ChatGPTCatalog {
		if !covered[e.Name] {
			t.Errorf("tool %s is not exercised by this sweep", e.Name)
		}
	}
}

// What the projection keeps is what the tools are for: P2 of the plugin's
// test cases reads ref, title, status and priority; a relation field reads
// as a ref, not an id; archive_item confirms.
func TestChatGPTProjection_KeepsWhatTheToolsAreFor(t *testing.T) {
	f := newProjectionFixture(t)
	_, v := f.call(t, "get_item", map[string]any{"workspace": f.ws, "ref": f.task1})
	item, _ := v.(map[string]any)
	if item["ref"] != f.task1 || item["title"] != "Login timeout bug" {
		t.Errorf("get_item lost ref or title: %v", item)
	}
	fields, _ := item["fields"].(map[string]any)
	if fields["status"] != "open" {
		t.Errorf("get_item lost status: %v", fields)
	}
	if owner, _ := fields["owner"].(string); !strings.HasPrefix(owner, "PERS-") {
		t.Errorf("relation field owner = %v, want the person's ref", fields["owner"])
	}
	if _, ok := item["seq"]; !ok {
		t.Errorf("get_item lost seq, which update_item's expected_seq needs")
	}
	if item["assigned_user_name"] == nil && item["assigned_user"] == nil {
		t.Errorf("get_item lost the assignee's name: %v", item)
	}
	_, v = f.call(t, "archive_item", map[string]any{"workspace": f.ws, "ref": f.task2})
	if m, _ := v.(map[string]any); m["archived"] != true || m["ref"] != f.task2 {
		t.Errorf("archive_item answer = %v, want {ref, archived: true}", v)
	}
	_, v = f.call(t, "list_collections", map[string]any{"workspace": f.ws})
	list, _ := v.(map[string]any)
	first, _ := list["items"].([]any)
	if len(first) == 0 {
		t.Fatalf("list_collections returned nothing")
	}
	if _, isObj := first[0].(map[string]any)["schema"].(map[string]any); !isObj {
		t.Errorf("list_collections schema is not an object: %v", first[0])
	}
}
