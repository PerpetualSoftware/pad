package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	padserver "github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
)

const boardTestSchema = `{"fields":[{"key":"status","type":"select","options":["open","done","cancelled"],"terminal_options":["done","cancelled"]},{"key":"priority","type":"select","options":["high","low"]}]}`

type boardFixture struct {
	d          *HTTPHandlerDispatcher
	st         *store.Store
	ws         *models.Workspace
	col, other *models.Collection
	user       *models.User
}

func newBoardFixture(t *testing.T) *boardFixture {
	t.Helper()
	srv, st := newPadServer(t)
	user, err := st.CreateUser(models.UserCreate{Email: "board@example.com", Name: "Board", Password: "irrelevant"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := st.CreateWorkspace(models.WorkspaceCreate{Name: "Board", OwnerID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	col, err := st.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TASK", Schema: boardTestSchema})
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateCollection(ws.ID, models.CollectionCreate{Name: "Others", Slug: "others", Prefix: "OTHER", Schema: boardTestSchema})
	if err != nil {
		t.Fatal(err)
	}
	return &boardFixture{&HTTPHandlerDispatcher{Handler: srv, UserResolver: fixedUserResolver(user)}, st, ws, col, other, user}
}
func (f *boardFixture) item(t *testing.T, col *models.Collection, status string) *models.Item {
	t.Helper()
	it, err := f.st.CreateItem(f.ws.ID, col.ID, models.ItemCreate{Title: "Board item", Content: "secret body", Fields: fmt.Sprintf(`{"status":%q,"priority":"high"}`, status)})
	if err != nil {
		t.Fatal(err)
	}
	return it
}
func (f *boardFixture) call(t *testing.T, ctx context.Context, action string, extra map[string]any) *CallToolResult {
	t.Helper()
	in := map[string]any{"workspace": f.ws.Slug, "collection": "tasks"}
	for k, v := range extra {
		in[k] = v
	}
	r, err := f.d.Dispatch(WithDispatchInput(ctx, in), []string{"board", action}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func boardJSON(t *testing.T, r *CallToolResult) map[string]any {
	t.Helper()
	if r.IsError {
		t.Fatalf("board failed: %+v", r)
	}
	raw, err := json.Marshal(r.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func boardCols(t *testing.T, r *CallToolResult) []any {
	t.Helper()
	return boardJSON(t, r)["columns"].([]any)
}
func TestPadBoard_ShowColumns(t *testing.T) {
	f := newBoardFixture(t)
	it := f.item(t, f.col, "open")
	f.item(t, f.col, "mystery")
	r := f.call(t, context.Background(), "show", nil)
	out := boardJSON(t, r)
	cols := out["columns"].([]any)
	if len(cols) != 3 {
		t.Fatalf("columns: %v", cols)
	}
	for i, s := range []string{"open", "done", "cancelled"} {
		c := cols[i].(map[string]any)
		if c["status"] != s || c["terminal"] != (i > 0) || c["abandoned"] != (i == 2) {
			t.Fatalf("column %d: %v", i, c)
		}
	}
	row := cols[0].(map[string]any)["items"].([]any)[0].(map[string]any)
	keys := []string{}
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"assignee", "id", "parent_ref", "priority", "ref", "seq", "status", "title", "updated_at"}
	if !reflect.DeepEqual(keys, want) || row["id"] != it.ID || row["priority"] != "high" || row["assignee"] != "" || out["unplaced"] != float64(1) {
		t.Fatalf("projection: %v, unplaced=%v", row, out["unplaced"])
	}
	alias := boardJSON(t, f.call(t, context.Background(), "show", map[string]any{"collection": "task"}))
	if alias["collection"] != "tasks" {
		t.Fatalf("alias: %v", alias)
	}
	empty := boardCols(t, f.call(t, context.Background(), "show", map[string]any{"parent": "TASK-999"}))
	if empty[0].(map[string]any)["total"] != float64(0) {
		t.Fatalf("parent: %v", empty)
	}
	door, _ := f.d.executeRequest(context.Background(), "test", f.user, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/items-index?collection=tasks", nil)
	d := boardJSON(t, door)
	if out["cursor"] != d["cursor"] || out["access_epoch"] != d["access_epoch"] {
		t.Fatalf("cursor or epoch changed: %v vs %v", out, d)
	}
	// Declared abandoned options replace the negative-name fallback.
	explicit := strings.Replace(boardTestSchema, `"terminal_options":["done","cancelled"]`, `"terminal_options":["done","cancelled"],"abandoned_options":["done"]`, 1)
	if _, err := f.st.UpdateCollection(f.col.ID, models.CollectionUpdate{Schema: &explicit}); err != nil {
		t.Fatal(err)
	}
	declared := boardCols(t, f.call(t, context.Background(), "show", nil))
	if declared[1].(map[string]any)["abandoned"] != true || declared[2].(map[string]any)["abandoned"] != false {
		t.Fatalf("declared abandoned options: %v", declared)
	}
	noStatus := `{"fields":[]}`
	if _, err := f.st.UpdateCollection(f.col.ID, models.CollectionUpdate{Schema: &noStatus}); err != nil {
		t.Fatal(err)
	}
	invalid := f.call(t, context.Background(), "show", nil)
	raw, _ := json.Marshal(invalid)
	if !invalid.IsError || !strings.Contains(string(raw), "validation_failed") {
		t.Fatalf("missing status: %s", raw)
	}

}
func TestPadBoard_ShowCaps(t *testing.T) {
	f := newBoardFixture(t)
	for range 305 {
		f.item(t, f.col, "open")
	}
	for range 25 {
		f.item(t, f.col, "done")
	}
	out := boardJSON(t, f.call(t, context.Background(), "show", nil))
	cols := out["columns"].([]any)
	for i, want := range []int{300, 20} {
		c := cols[i].(map[string]any)
		if len(c["items"].([]any)) != want || c["total"] != float64([]int{305, 25}[i]) {
			t.Fatalf("cap %d: %v", i, c)
		}
	}
	if out["truncated"] != true {
		t.Fatal("not truncated")
	}
}
func TestPadBoard_ChangesUpsertRemove(t *testing.T) {
	f := newBoardFixture(t)
	up := f.item(t, f.col, "open")
	del := f.item(t, f.col, "open")
	move := f.item(t, f.col, "open")
	other := f.item(t, f.other, "open")
	base := boardJSON(t, f.call(t, context.Background(), "show", nil))
	title := "Updated"
	if _, err := f.st.UpdateItem(up.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.DeleteItem(del.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.MoveItem(move.ID, f.other.ID, `{"status":"open"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpdateItem(other.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	out := boardJSON(t, f.call(t, context.Background(), "changes", map[string]any{"since": base["cursor"], "access_epoch": base["access_epoch"]}))
	ups := out["upserts"].([]any)
	if len(ups) != 1 || ups[0].(map[string]any)["id"] != up.ID || out["resync"] != false {
		t.Fatalf("upserts: %v", out)
	}
	// A full member gets no moved-out tombstone: the move arrives as a live
	// row in the target collection, as indistinguishable from the other
	// collection's update as it is.
	if got := boardIDs(out["removed_ids"]); !reflect.DeepEqual(got, []string{del.ID}) {
		t.Fatalf("removed: %v want %v", got, []string{del.ID})
	}
	departed := []string{move.ID, other.ID}
	sort.Strings(departed)
	if got := boardIDs(out["departed_ids"]); !reflect.DeepEqual(got, departed) {
		t.Fatalf("departed: %v want %v", got, departed)
	}
	resync := boardJSON(t, f.call(t, context.Background(), "changes", map[string]any{"since": out["cursor"], "access_epoch": "stale"}))
	if resync["resync"] != true {
		t.Fatal("epoch mismatch must resync")
	}
	bad := f.call(t, context.Background(), "changes", map[string]any{"since": "bad", "access_epoch": base["access_epoch"]})
	door, _ := f.d.executeRequest(context.Background(), "board changes", f.user, http.MethodGet, "/api/v1/workspaces/"+f.ws.Slug+"/items-changes?limit=500&since=bad", nil)
	a, _ := json.Marshal(bad)
	b, _ := json.Marshal(door)
	if !bad.IsError || string(a) != string(b) {
		t.Fatalf("bad cursor differs: %s vs %s", a, b)
	}
	for range 500 {
		f.item(t, f.col, "open")
	}
	full := boardJSON(t, f.call(t, context.Background(), "changes", map[string]any{"since": out["cursor"], "access_epoch": out["access_epoch"]}))
	if len(full["upserts"].([]any)) != 500 || full["resync"] != true {
		t.Fatalf("full page: upserts=%d resync=%v", len(full["upserts"].([]any)), full["resync"])
	}

}
func boardIDs(v any) []string {
	ids := []string{}
	for _, id := range v.([]any) {
		ids = append(ids, id.(string))
	}
	sort.Strings(ids)
	return ids
}
func (f *boardFixture) restrict(t *testing.T) {
	t.Helper()
	u, err := f.st.CreateUser(models.UserCreate{Email: "restricted@example.com", Name: "Restricted", Password: "irrelevant"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.st.AddWorkspaceMember(f.ws.ID, u.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if err = f.st.SetMemberCollectionAccess(f.ws.ID, u.ID, "specific", []string{f.col.ID}); err != nil {
		t.Fatal(err)
	}
	f.user = u
	f.d.UserResolver = fixedUserResolver(u)
}
func TestPadBoard_RestrictedCursorNoLeak(t *testing.T) {
	f := newBoardFixture(t)
	visible := f.item(t, f.col, "open")
	hidden := f.item(t, f.other, "open")
	f.restrict(t)
	door, err := f.d.executeRequest(context.Background(), "board changes", f.user, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/items-changes?limit=500&since=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	d := boardJSON(t, door)
	out := boardJSON(t, f.call(t, context.Background(), "changes", map[string]any{"since": "0", "access_epoch": d["access_epoch"]}))
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), hidden.ID) || !strings.Contains(string(raw), visible.ID) || out["cursor"] != d["cursor"] || out["access_epoch"] != d["access_epoch"] || len(out["upserts"].([]any)) != len(d["changes"].([]any)) {
		t.Fatalf("door parity: %v vs %v", out, d)
	}
	base := out["cursor"]
	if _, err = f.st.MoveItem(visible.ID, f.other.ID, `{"status":"open"}`); err != nil {
		t.Fatal(err)
	}
	out = boardJSON(t, f.call(t, context.Background(), "changes", map[string]any{"since": base, "access_epoch": d["access_epoch"]}))
	ids := out["removed_ids"].([]any)
	if len(ids) != 1 || ids[0] != visible.ID || len(out["departed_ids"].([]any)) != 0 {
		t.Fatalf("moved_out: %v", out)
	}
}
func TestPadBoard_HiddenVsMissingIdentical(t *testing.T) {
	f := newBoardFixture(t)
	f.restrict(t)
	a := f.call(t, context.Background(), "show", map[string]any{"collection": "others"})
	b := f.call(t, context.Background(), "show", map[string]any{"collection": "missing"})
	ar, _ := json.Marshal(a)
	br, _ := json.Marshal(b)
	if !a.IsError || !strings.Contains(string(ar), "not_found") || string(ar) != string(br) || strings.Contains(string(ar), "others") || strings.Contains(string(br), "missing") {
		t.Fatalf("hidden/missing differ: %s vs %s", ar, br)
	}
}
func TestPadBoard_AllowListRefusal(t *testing.T) {
	f := newBoardFixture(t)
	ctx := padserver.WithTokenAllowedWorkspaces(context.Background(), []string{"elsewhere"})
	for _, action := range []string{"show", "changes"} {
		got := f.call(t, ctx, action, map[string]any{"since": "0", "access_epoch": ""})
		door, _ := f.d.executeRequest(ctx, "board "+action, f.user, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/collections", nil)
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(door)
		if !got.IsError || string(a) != string(b) {
			t.Fatalf("allowlist %s: %s vs %s", action, a, b)
		}
	}
}
func TestPadBoard_ReadOnly(t *testing.T) {
	tools := appsWireResult(t, appsTestServer(t, true), "tools/list", nil)["tools"].([]any)
	found := false
	for _, v := range tools {
		tool := v.(map[string]any)
		if tool["name"] == "pad_board" {
			found = true
			if tool["annotations"].(map[string]any)["readOnlyHint"] != true {
				t.Fatalf("annotations: %v", tool)
			}
		}
	}
	if !found {
		t.Fatal("pad_board missing")
	}
	f := newBoardFixture(t)
	ctx := padserver.WithTokenScopes(context.Background(), `["pad:read"]`)
	for _, a := range []string{"show", "changes"} {
		if CallNeedsWriteScope("pad_board", a) {
			t.Fatalf("%s requires write", a)
		}
		boardJSON(t, f.call(t, ctx, a, map[string]any{"since": "0", "access_epoch": ""}))
	}
	for _, action := range []string{"show", "changes"} {
		res, err := (&ExecDispatcher{}).Dispatch(context.Background(), []string{"board", action}, nil)
		if err != nil || !res.IsError || !containsToolText(res, "remote HTTP") {
			t.Fatalf("stdio must refuse board %s: %v %v", action, res, err)
		}
	}

}
func TestMCPApps_BoardRegistration(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		srv := appsTestServer(t, enabled)
		found := false
		for _, v := range appsWireResult(t, srv, "tools/list", nil)["tools"].([]any) {
			tool := v.(map[string]any)
			if tool["name"] == "pad_board" {
				found = true
				meta := tool["_meta"].(map[string]any)
				if meta["ui/resourceUri"] != "ui://pad/board" || meta["ui"].(map[string]any)["resourceUri"] != "ui://pad/board" {
					t.Fatalf("meta: %v", meta)
				}
			}
		}
		if found != enabled {
			t.Fatalf("board registered=%v apps=%v", found, enabled)
		}
		resources := appsWireResult(t, srv, "resources/list", nil)["resources"].([]any)
		found = false
		for _, v := range resources {
			if v.(map[string]any)["uri"] == "ui://pad/board" {
				found = true
			}
		}
		if found != enabled {
			t.Fatalf("board resource=%v apps=%v", found, enabled)
		}
		if enabled {
			c := appsWireResult(t, srv, "resources/read", map[string]any{"uri": "ui://pad/board"})["contents"].([]any)[0].(map[string]any)
			ui := c["_meta"].(map[string]any)["ui"].(map[string]any)
			if c["mimeType"] != "text/html;profile=mcp-app" || !reflect.DeepEqual(ui, map[string]any{"preferredDisplayMode": "fullscreen"}) {
				t.Fatalf("resource: %v", c)
			}
		}
	}
}

// A parent-filtered board stays filtered across changes when changes is
// passed the collection show returned, and the assignee is projected.
func TestPadBoard_ParentFilterAndSlugForms(t *testing.T) {
	f := newBoardFixture(t)
	parent := f.item(t, f.other, "open")
	in := f.item(t, f.col, "open")
	out := f.item(t, f.col, "open")
	if _, err := f.st.SetParentLink(f.ws.ID, in.ID, parent.ID, "test"); err != nil {
		t.Fatal(err)
	}
	assignee := f.user.ID
	if _, err := f.st.UpdateItem(in.ID, models.ItemUpdate{AssignedUserID: &assignee}); err != nil {
		t.Fatal(err)
	}
	ref := strings.ToLower(parent.Ref)
	if ref == "" {
		t.Fatal("parent has no ref")
	}
	base := boardJSON(t, f.call(t, context.Background(), "show", map[string]any{"collection": "task", "parent": ref}))
	if base["collection"] != "tasks" {
		t.Fatalf("show resolved %v", base["collection"])
	}
	items := boardCols(t, f.call(t, context.Background(), "show", map[string]any{"parent": ref}))[0].(map[string]any)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != in.ID || items[0].(map[string]any)["assignee"] != "Board" {
		t.Fatalf("parent-filtered show: %v", items)
	}
	title := "Touched"
	for _, id := range []string{in.ID, out.ID} {
		if _, err := f.st.UpdateItem(id, models.ItemUpdate{Title: &title}); err != nil {
			t.Fatal(err)
		}
	}
	ch := boardJSON(t, f.call(t, context.Background(), "changes", map[string]any{"collection": base["collection"], "parent": ref, "since": base["cursor"], "access_epoch": base["access_epoch"]}))
	ups := ch["upserts"].([]any)
	if len(ups) != 1 || ups[0].(map[string]any)["id"] != in.ID {
		t.Fatalf("upserts: %v", ch)
	}
	removed := ch["removed_ids"].([]any)
	if len(removed) != 1 || removed[0] != out.ID {
		t.Fatalf("removed_ids: %v", ch)
	}
}

// Two collections whose slugs differ only by a trailing s never mix: changes
// matches the collection show returned exactly.
func TestPadBoard_ChangesExactCollection(t *testing.T) {
	f := newBoardFixture(t)
	single, err := f.st.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Task", Slug: "task", Prefix: "TSK", Schema: boardTestSchema})
	if err != nil {
		t.Fatal(err)
	}
	plural := f.item(t, f.col, "open")
	one := f.item(t, single, "open")
	for _, slug := range []string{"task", "tasks"} {
		base := boardJSON(t, f.call(t, context.Background(), "show", map[string]any{"collection": slug}))
		if base["collection"] != slug {
			t.Fatalf("show %s resolved %v", slug, base["collection"])
		}
		ch := boardJSON(t, f.call(t, context.Background(), "changes", map[string]any{"collection": base["collection"], "since": "0", "access_epoch": base["access_epoch"]}))
		want, other := plural.ID, one.ID
		if slug == "task" {
			want, other = one.ID, plural.ID
		}
		ups := ch["upserts"].([]any)
		if len(ups) != 1 || ups[0].(map[string]any)["id"] != want {
			t.Fatalf("changes %s upserts: %v", slug, ch)
		}
		if got := boardIDs(ch["removed_ids"]); len(got) != 0 {
			t.Fatalf("changes %s removes another collection's row: %v", slug, ch)
		}
		if got := boardIDs(ch["departed_ids"]); !reflect.DeepEqual(got, []string{other}) {
			t.Fatalf("changes %s departed: %v", slug, ch)
		}
	}
}

// With Apps on, the instructions count pad_board among the tools; without
// Apps the sentence is absent.
func TestMCPApps_BoardInstructionsCount(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		init := appsWireResult(t, appsTestServer(t, enabled), "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}})
		text, _ := init["instructions"].(string)
		if enabled && !strings.Contains(text, "Twelve tools total") {
			t.Fatal("base instructions missing")
		}
		// The apps text has its own heading, corrects the base count and says
		// pad_board takes action like the others.
		got := strings.Contains(text, "\n## MCP Apps (this connection only)\n") &&
			strings.Contains(text, "thirteen tools total") &&
			strings.Contains(text, "Pass `action` to `pad_board`")
		if got != enabled {
			t.Fatalf("apps=%v: apps section present=%v", enabled, got)
		}
	}
}

// A status field with no terminal_options takes the default terminal list,
// matched case-insensitively, like the rest of Pad.
func TestPadBoard_ShowDefaultTerminals(t *testing.T) {
	f := newBoardFixture(t)
	if _, err := f.st.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Legacy", Slug: "legacy", Prefix: "LEG", Schema: `{"fields":[{"key":"status","type":"select","options":["open","Done","cancelled"]}]}`}); err != nil {
		t.Fatal(err)
	}
	got := map[string][2]bool{}
	for _, c := range boardCols(t, f.call(t, context.Background(), "show", map[string]any{"collection": "legacy"})) {
		col := c.(map[string]any)
		got[col["status"].(string)] = [2]bool{col["terminal"].(bool), col["abandoned"].(bool)}
	}
	want := map[string][2]bool{"open": {false, false}, "Done": {true, false}, "cancelled": {true, true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("columns terminal/abandoned = %v, want %v", got, want)
	}
}
