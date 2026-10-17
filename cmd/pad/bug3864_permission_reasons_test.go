package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/mcp"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3864: a refused MCP call names its cause. Before, every 403 reached a
// remote caller as permission_denied with a hint that LED with "Insufficient
// role for this operation", whatever the cause, and the stdio transport,
// which reads the CLI's stderr, said server_error for any refusal whose
// message did not happen to contain "forbidden". The support case was a bot
// with a read-only API token: reads worked, writes were refused, and the
// agent was told its role was the problem.
//
// Each row is a real refusal from the real server, reached through the
// shipped dispatchers on both transports: wireMCP's /mcp for remote, and the
// real command tree run as a subprocess for stdio. Both must report the same
// code, details.reason and hint.

const (
	bug3864HintRole     = "Your role in this workspace does not allow this. Ask a workspace owner to change your role."
	bug3864HintScope    = "This API token's access does not include this action: a 'Read only' token cannot make changes. Create a token with 'Read and write' access under Account settings → API Tokens, and use it instead."
	bug3864HintNoAccess = "You do not have access to this workspace or collection. Ask a workspace owner for access."
	bug3864HintEmail    = "Verify your account's email address (the link is in your inbox, and the web app can send a new one). Changes are refused until then."
)

// bug3864Op is one refused write, in both transports' terms.
type bug3864Op struct {
	remote  map[string]any // pad_item arguments, less workspace
	cmdPath []string       // the CLI command stdio runs
	argv    []string       // its flags and, after "--", its positionals
}

var bug3864Create = bug3864Op{
	remote:  map[string]any{"action": "create", "collection": "tasks", "title": bug3864Title},
	cmdPath: []string{"item", "create"},
	argv:    []string{"--", "tasks", bug3864Title},
}

type bug3864Want struct {
	code   mcp.ErrorCode
	reason string // details.reason; "" means the field must be absent
	hint   string // the hint must START with this
}

func TestBUG3864_RefusalsNameTheirCauseOnBothTransports(t *testing.T) {
	f := newCapFixture(t, cfgHTTP, false, nil, "true")
	if err := f.store.SeedCollectionsFromTemplate(f.ws.ID, "startup"); err != nil {
		t.Fatal(err)
	}
	viewer := bug3864User(t, f, "viewer@example.com", false)
	if err := f.store.AddWorkspaceMember(f.ws.ID, viewer.ID, "viewer"); err != nil {
		t.Fatal(err)
	}

	// A guest holds a view grant on one item. Editing it reaches the edit
	// check, which a role change cannot satisfy: the guest has no role.
	guest := bug3864User(t, f, "guest@example.com", false)
	granted, err := f.store.CreateItem(f.ws.ID, bug3864TasksID(t, f), models.ItemCreate{Title: "granted"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateItemGrant(f.ws.ID, granted.ID, guest.ID, "view", f.member.ID); err != nil {
		t.Fatal(err)
	}
	guestEdit := bug3864Op{
		remote:  map[string]any{"action": "update", "ref": granted.Ref, "title": bug3864Title},
		cmdPath: []string{"item", "update"},
		argv:    []string{"--title", bug3864Title, "--", granted.Ref},
	}

	rows := []struct {
		name  string
		token string
		op    bug3864Op
		want  bug3864Want
	}{
		{"role", bug3864Token(t, f, viewer, `["*"]`), bug3864Create, bug3864Want{mcp.ErrPermissionDenied, "role", bug3864HintRole}},
		{"token_scope", bug3864Token(t, f, f.member, `["read"]`), bug3864Create, bug3864Want{mcp.ErrPermissionDenied, "token_scope", bug3864HintScope}},
		// A non-member is answered 404, which hides that the workspace exists.
		// The 403 is a platform admin's bearer token, which reaches only the
		// workspaces the admin is a member of (BUG-1616).
		{"no_access", bug3864Token(t, f, f.admin, `["*"]`), bug3864Create, bug3864Want{mcp.ErrPermissionDenied, "no_access", bug3864HintNoAccess}},
		{"guest edit", bug3864Token(t, f, guest, `["*"]`), guestEdit, bug3864Want{mcp.ErrPermissionDenied, "no_access", bug3864HintNoAccess}},
	}
	for _, r := range rows {
		t.Run(r.name+"/remote", func(t *testing.T) {
			bug3864Check(t, bug3864Remote(t, f, r.token, r.op), r.want)
		})
		t.Run(r.name+"/stdio", func(t *testing.T) {
			bug3864Check(t, bug3864Stdio(t, f, r.token, r.op), r.want)
		})
	}
	if n := bug3864CountTitled(t, f); n != 0 {
		t.Errorf("a refused write created %d items", n)
	}
}

// The Cloud-only cause: an account whose email is unverified may read and may
// not write. The remote transport refuses in its own gate before the request
// is built; stdio reaches the server's RequireVerifiedEmail middleware.
func TestBUG3864_UnverifiedEmailIsItsOwnCodeOnBothTransports(t *testing.T) {
	f := newCapFixture(t, cfgHTTP, true, nil, "true")
	if err := f.store.SeedCollectionsFromTemplate(f.ws.ID, "startup"); err != nil {
		t.Fatal(err)
	}
	unverified := bug3864User(t, f, "unverified@example.com", true)
	if err := f.store.AddWorkspaceMember(f.ws.ID, unverified.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	tok := bug3864Token(t, f, unverified, `["*"]`)
	want := bug3864Want{mcp.ErrorCode("email_not_verified"), "", bug3864HintEmail}
	t.Run("remote", func(t *testing.T) { bug3864Check(t, bug3864Remote(t, f, tok, bug3864Create), want) })
	t.Run("stdio", func(t *testing.T) { bug3864Check(t, bug3864Stdio(t, f, tok, bug3864Create), want) })
}

func bug3864User(t *testing.T, f *capFixture, email string, unverified bool) *models.User {
	t.Helper()
	u, err := f.store.CreateUser(models.UserCreate{Email: email, Name: email, Password: "correct-horse-battery-staple", Unverified: unverified})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func bug3864Token(t *testing.T, f *capFixture, u *models.User, scopes string) string {
	t.Helper()
	tok, err := f.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "bot", Scopes: scopes}, 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	return tok.Token
}

const bug3864Title = "bug3864 refused write"

func bug3864Remote(t *testing.T, f *capFixture, token string, op bug3864Op) mcp.ErrorEnvelope {
	t.Helper()
	const proto = "2025-11-25"
	call := func(id int, method string, params map[string]any) []byte {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		req := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
		req.Host = f.host
		req.RemoteAddr = "192.0.2.10:4242"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", proto)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := newMCPRecorder()
		f.srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", method, rr.Code, rr.Body.String())
		}
		return rr.Body.Bytes()
	}
	call(1, "initialize", map[string]any{
		"protocolVersion": proto, "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "bug3864", "version": "1"},
	})
	args := map[string]any{"workspace": f.ws.Slug}
	for k, v := range op.remote {
		args[k] = v
	}
	out := call(2, "tools/call", map[string]any{"name": "pad_item", "arguments": args})
	var rpc struct {
		Result struct {
			IsError           bool              `json:"isError"`
			StructuredContent mcp.ErrorEnvelope `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &rpc); err != nil {
		t.Fatalf("decode tools/call: %v: %s", err, out)
	}
	if !rpc.Result.IsError {
		t.Fatalf("PRECONDITION: the write should have been refused: %s", out)
	}
	return rpc.Result.StructuredContent
}

func bug3864Stdio(t *testing.T, f *capFixture, token string, op bug3864Op) mcp.ErrorEnvelope {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = f.host
		r.RemoteAddr = "192.0.2.10:4242"
		f.srv.ServeHTTP(w, r)
	}))
	defer ts.Close()

	t.Setenv(padHelperEnv, "1")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_TOKEN", token)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	d := &mcp.ExecDispatcher{Binary: bin}
	argv := append([]string{"--url", ts.URL, "--workspace", f.ws.Slug, "--format", "json"}, op.argv...)
	res, err := d.Dispatch(context.Background(), op.cmdPath, argv)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	env, ok := res.StructuredContent.(mcp.ErrorEnvelope)
	if !ok {
		t.Fatalf("PRECONDITION: the write should have been refused; got %T %+v", res.StructuredContent, res.Content)
	}
	return env
}

func bug3864Check(t *testing.T, env mcp.ErrorEnvelope, want bug3864Want) {
	t.Helper()
	got := ""
	if len(env.Error.Details) > 0 {
		var d struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(env.Error.Details, &d)
		got = d.Reason
	}
	if env.Error.Code != want.code || got != want.reason || !strings.HasPrefix(env.Error.Hint, want.hint) {
		t.Errorf("got code=%q reason=%q hint=%q (message %q)\nwant code=%q reason=%q hint starting %q",
			env.Error.Code, got, env.Error.Hint, env.Error.Message, want.code, want.reason, want.hint)
	}
	if want.reason != "role" && strings.Contains(strings.ToLower(env.Error.Hint), "role") {
		t.Errorf("a %q refusal must not blame a role: hint %q", want.reason, env.Error.Hint)
	}
}

func bug3864CountTitled(t *testing.T, f *capFixture) int {
	t.Helper()
	items, err := f.store.ListItems(f.ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, it := range items {
		if it.Title == bug3864Title {
			n++
		}
	}
	return n
}

func bug3864TasksID(t *testing.T, f *capFixture) string {
	t.Helper()
	c, err := f.store.GetCollectionBySlug(f.ws.ID, "tasks")
	if err != nil || c == nil {
		t.Fatalf("tasks collection: %v", err)
	}
	return c.ID
}
