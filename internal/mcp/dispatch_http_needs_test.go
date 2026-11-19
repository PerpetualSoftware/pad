package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4643: pad_needs over the remote dispatcher, against the real server and
// store (newClearFixture: an item assigned to the owner and carrying a role).

func (f *clearFixture) needsDispatch(t *testing.T, h http.Handler, verb string, input map[string]any) *CallToolResult {
	t.Helper()
	if h == nil {
		h = f.srv
	}
	d := &HTTPHandlerDispatcher{Handler: h, UserResolver: func(context.Context) *models.User { return f.owner }}
	res, err := d.Dispatch(WithDispatchInput(context.Background(), input), []string{"needs", verb}, nil)
	if err != nil {
		t.Fatalf("Dispatch(needs %s): %v", verb, err)
	}
	return res
}

func TestPadNeeds_RemoteNotMineToRole_PatchThenPut(t *testing.T) {
	f := newClearFixture(t)
	other, err := f.store.CreateAgentRole(f.workspace.ID, models.AgentRoleCreate{Name: "Reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	res := f.needsDispatch(t, nil, "not-mine", map[string]any{
		"workspace": f.workspace.Slug, "ref": f.item.Slug, "to_role": other.Slug,
	})
	if res.IsError {
		t.Fatalf("not-mine: %s", textOf(res))
	}
	got := f.reload(t)
	if got.AgentRoleID == nil || *got.AgentRoleID != other.ID {
		t.Fatalf("role = %v, want %s", got.AgentRoleID, other.ID)
	}
	if got.AssignedUserID != nil && *got.AssignedUserID != "" {
		t.Fatalf("a role give must clear the person, got %v", *got.AssignedUserID)
	}
	nm, err := f.store.GetNeedsNotMine(f.owner.ID, f.item.ID)
	if err != nil || nm == nil {
		t.Fatalf("not-mine row: %+v %v", nm, err)
	}
	if nm.Source != models.NeedsSourceCLI {
		t.Fatalf("source = %q, want cli (taken from the auth, never the body)", nm.Source)
	}
	if nm.Marker.SeenAt == "" {
		t.Fatalf("no marker sent, so the server records now: %+v", nm.Marker)
	}
	var out struct {
		Results []struct {
			Ref     string                    `json:"ref"`
			NotMine json.RawMessage           `json:"not_mine"`
			GivenTo struct{ Kind, ID string } `json:"given_to"`
		} `json:"results"`
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Results) != 1 || out.Results[0].GivenTo.Kind != "role" ||
		out.Results[0].GivenTo.ID != other.ID || len(out.Results[0].NotMine) == 0 || string(out.Results[0].NotMine) == "null" {
		t.Fatalf("result shape: %s (%v)", raw, err)
	}

	// The cross-workspace list route reaches the real handler.
	list := f.needsDispatch(t, nil, "list", map[string]any{"view": "hidden"})
	if list.IsError {
		t.Fatalf("list: %s", textOf(list))
	}
}

func TestPadNeeds_RemoteNotMineToUser(t *testing.T) {
	f := newClearFixture(t)
	res := f.needsDispatch(t, nil, "not-mine", map[string]any{
		"workspace": f.workspace.Slug, "ref": f.item.Slug, "to_user": "OWNER@example.com",
	})
	if res.IsError {
		t.Fatalf("not-mine: %s", textOf(res))
	}
	got := f.reload(t)
	if got.AssignedUserID == nil || *got.AssignedUserID != f.owner.ID || got.AgentRoleID == nil || *got.AgentRoleID != f.role.ID {
		t.Fatalf("a person give sets only the person: user=%v role=%v", got.AssignedUserID, got.AgentRoleID)
	}
}

// seqBumper serves the real server and, right after the first GET of the
// item, writes to it, so the PATCH that follows carries a stale seq.
type seqBumper struct {
	f      *clearFixture
	t      *testing.T
	bumped bool
	calls  []string
}

func (b *seqBumper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.calls = append(b.calls, r.Method+" "+r.URL.Path)
	b.f.srv.ServeHTTP(w, r)
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/items/"+b.f.item.Slug) && !b.bumped {
		b.bumped = true
		body := "Changed under the reader" // content, not title: a title write can move the slug
		if _, err := b.f.store.UpdateItem(b.f.item.ID, models.ItemUpdate{Content: &body}); err != nil {
			b.t.Fatalf("bump: %v", err)
		}
	}
}

func TestPadNeeds_RemoteNotMinePatchConflictWritesNothing(t *testing.T) {
	f := newClearFixture(t)
	other, err := f.store.CreateAgentRole(f.workspace.ID, models.AgentRoleCreate{Name: "Reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	b := &seqBumper{f: f, t: t}
	res := f.needsDispatch(t, b, "not-mine", map[string]any{
		"workspace": f.workspace.Slug, "ref": f.item.Slug, "to_role": other.Slug,
	})
	if !res.IsError || !strings.Contains(textOf(res), "update_conflict") {
		t.Fatalf("want update_conflict, got %s", textOf(res))
	}
	for _, c := range b.calls {
		if strings.HasPrefix(c, "PUT ") {
			t.Fatalf("PUT sent after a refused PATCH: %v", b.calls)
		}
	}
	got := f.reload(t)
	if got.AgentRoleID == nil || *got.AgentRoleID != f.role.ID || got.AssignedUserID == nil || *got.AssignedUserID != f.owner.ID {
		t.Fatalf("assignment changed: user=%v role=%v", got.AssignedUserID, got.AgentRoleID)
	}
	if nm, err := f.store.GetNeedsNotMine(f.owner.ID, f.item.ID); err != nil || nm != nil {
		t.Fatalf("not-mine written: %+v %v", nm, err)
	}

	// An unknown role is refused before any write too.
	res = f.needsDispatch(t, nil, "not-mine", map[string]any{
		"workspace": f.workspace.Slug, "ref": f.item.Slug, "to_role": "no-such-role",
	})
	if !res.IsError {
		t.Fatalf("unknown role accepted: %s", textOf(res))
	}
	if nm, err := f.store.GetNeedsNotMine(f.owner.ID, f.item.ID); err != nil || nm != nil {
		t.Fatalf("not-mine written for an unknown role: %+v %v", nm, err)
	}
	res = f.needsDispatch(t, nil, "not-mine", map[string]any{
		"workspace": f.workspace.Slug, "ref": f.item.Slug, "to_role": other.Slug, "to_user": "owner@example.com",
	})
	if !res.IsError || !strings.Contains(textOf(res), "mutually exclusive") {
		t.Fatalf("both targets accepted: %s", textOf(res))
	}
}

func TestPadNeeds_RemoteRestore(t *testing.T) {
	f := newClearFixture(t)
	if _, err := f.store.MarkNeedsNotMine(f.owner.ID, f.workspace.ID, f.item.ID, models.NeedsSourceCLI, nil); err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"workspace": f.workspace.Slug, "ref": f.item.Slug}
	if res := f.needsDispatch(t, nil, "restore", in); res.IsError {
		t.Fatalf("restore: %s", textOf(res))
	}
	if nm, err := f.store.GetNeedsNotMine(f.owner.ID, f.item.ID); err != nil || nm != nil {
		t.Fatalf("not-mine survived restore: %+v %v", nm, err)
	}
	if _, err := f.store.SnoozeNeedsItem(f.owner.ID, f.workspace.ID, f.item.ID, models.NeedsSnoozeDismiss, time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	if res := f.needsDispatch(t, nil, "restore", in); res.IsError {
		t.Fatalf("restore: %s", textOf(res))
	}
	if sn, err := f.store.GetNeedsSnooze(f.owner.ID, f.item.ID, time.Now()); err != nil || sn != nil {
		t.Fatalf("snooze survived restore: %+v %v", sn, err)
	}
}

// The inbox spans every workspace, so list applies only an explicit
// workspace; the item actions take the session default as usual.
func TestPadNeeds_ListIgnoresSessionWorkspace(t *testing.T) {
	doc := liveCmdhelpDoc(t)
	for _, tc := range []struct {
		action string
		input  map[string]any
		want   string // the --workspace value, "" for none
	}{
		{"list", map[string]any{}, ""},
		{"list", map[string]any{"workspace": "explicit-ws"}, "explicit-ws"},
		{"not-mine", map[string]any{"ref": "TASK-1", "to_role": "reviewer"}, "session-ws"},
		{"restore", map[string]any{"ref": "TASK-1"}, "session-ws"},
	} {
		disp := &fakeDispatcher{}
		env := ActionEnv{Doc: doc, Workspace: NewWorkspaceState("session-ws"), Dispatcher: disp}
		handler := makeFanOutHandler(padNeedsTool, env)
		input := map[string]any{"action": tc.action}
		for k, v := range tc.input {
			input[k] = v
		}
		res, err := handler(context.Background(), callToolRequest(input))
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %s", tc.action, err, textOf(res))
		}
		got := ""
		for i, a := range disp.gotArgs {
			if a == "--workspace" && i+1 < len(disp.gotArgs) {
				got = disp.gotArgs[i+1]
			}
		}
		if got != tc.want {
			t.Errorf("%s %v: --workspace %q, want %q (args %v)", tc.action, tc.input, got, tc.want, disp.gotArgs)
		}
	}
}

// TestPadNeeds_ListPages: list carries cursor and limit on both transports,
// so a caller can follow next_cursor past the first page.
func TestPadNeeds_ListPages(t *testing.T) {
	input := map[string]any{"view": "hidden", "cursor": "abc", "limit": float64(10)}

	_, path, _, err := routeTable["needs list"](input)
	if err != nil {
		t.Fatalf("remote route: %v", err)
	}
	u, err := url.Parse(path)
	if err != nil {
		t.Fatalf("parse %q: %v", path, err)
	}
	q := u.Query()
	if q.Get("cursor") != "abc" || q.Get("limit") != "10" || q.Get("view") != "hidden" {
		t.Errorf("remote query = %q, want cursor=abc limit=10 view=hidden", u.RawQuery)
	}

	disp := &fakeDispatcher{}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState(""), Dispatcher: disp}
	handler := makeFanOutHandler(padNeedsTool, env)
	call := map[string]any{"action": "list"}
	for k, v := range input {
		call[k] = v
	}
	res, err := handler(context.Background(), callToolRequest(call))
	if err != nil || res.IsError {
		t.Fatalf("stdio list: %v %s", err, textOf(res))
	}
	args := strings.Join(disp.gotArgs, " ")
	for _, want := range []string{"--cursor abc", "--limit 10"} {
		if !strings.Contains(args, want) {
			t.Errorf("stdio args %q missing %q", args, want)
		}
	}
}
