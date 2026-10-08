package mcp

// BUG-2772: a write over remote /mcp is an AGENT write, named by the client's
// declared clientInfo (IDEA-2791 Tier B): the per-request _meta clientInfo of
// a modern request, else the clientInfo a legacy session sent at initialize
// (remembered by the Mcp-Session-Id pad issued), else unnamed, and never the
// human. Driven end to end: the real Streamable HTTP transport, a tool whose
// handler calls the dispatcher as every catalog action does (its own ctx,
// with WithDispatchInput), the real server and store.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/PerpetualSoftware/pad/internal/models"
	padserver "github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

type bug2772Sessions struct{}

func (bug2772Sessions) Generate() string               { return "pad-mcp-" + uuid.NewString() }
func (bug2772Sessions) Validate(string) (bool, error)  { return false, nil }
func (bug2772Sessions) Terminate(string) (bool, error) { return false, nil }

type bug2772World struct {
	t     *testing.T
	s     *store.Store
	srv   *padserver.Server
	ws    *models.Workspace
	url   string
	owner *models.User
}

func newBUG2772World(t *testing.T) *bug2772World {
	t.Helper()
	s := storetest.NewSQLite(t)
	srv := padserver.New(s)
	t.Cleanup(srv.Stop)
	owner, err := s.CreateUser(models.UserCreate{Email: "mcp-2772@example.com", Name: "Owner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "MCP 2772", Slug: "mcp-2772", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(models.UserCreate{Email: "mcp-2772-other@example.com", Name: "Other", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, other.ID, "editor"); err != nil {
		t.Fatal(err)
	}

	clients := NewClientRegistry()
	d := &HTTPHandlerDispatcher{Handler: srv, Clients: clients, UserResolver: func(ctx context.Context) *models.User {
		u, _ := padserver.CurrentUserFromContext(ctx)
		return u
	}}
	mcpSrv := NewServer(Options{Version: "test", Clients: clients})
	mcpSrv.MCP().AddTool(mcp.NewTool("create_task", mcp.WithString("title")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		input := map[string]any{"workspace": ws.Slug, "collection": "tasks", "title": req.GetString("title", "")}
		res, err := d.Dispatch(WithDispatchInput(ctx, input), []string{"item", "create"}, nil)
		if err != nil {
			return nil, err
		}
		return res, nil
	})
	mcpSrv.MCP().AddTool(mcp.NewTool("claim_task", mcp.WithString("ref")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		input := map[string]any{"workspace": ws.Slug, "ref": req.GetString("ref", "")}
		return d.Dispatch(WithDispatchInput(ctx, input), []string{"item", "claim"}, nil)
	})
	mcpSrv.MCP().AddTool(mcp.NewTool("next"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return d.Dispatch(WithDispatchInput(ctx, map[string]any{"workspace": ws.Slug}), []string{"project", "next"}, nil)
	})
	// Stands in for the /mcp auth middleware: the request's user goes on its
	// context, the owner unless the test names the other member.
	transport := NewRemoteTransport(mcpSrv.MCP(), bug2772Sessions{})
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		u := owner
		if r.Header.Get(bug2772UserHeader) == "other" {
			u = other
		}
		transport.ServeHTTP(rw, r.WithContext(padserver.WithCurrentUser(r.Context(), u)))
	}))
	t.Cleanup(ts.Close)
	return &bug2772World{t: t, s: s, srv: srv, ws: ws, url: ts.URL + "/mcp", owner: owner}
}

// bug2772UserHeader picks the request's user in the test world.
const bug2772UserHeader = "X-Test-User"

func (w *bug2772World) post(body, session string, headers map[string]string) (string, string) {
	w.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, w.url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		w.t.Fatalf("POST %s: %d %s", body, resp.StatusCode, raw)
	}
	return string(raw), resp.Header.Get("Mcp-Session-Id")
}

// initialize runs a legacy handshake with the given clientInfo name and
// returns the session id pad issued.
func (w *bug2772World) initialize(clientName string) string {
	w.t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":` + mustJSONString(clientName) + `,"version":"9.9"}}}`
	_, session := w.post(body, "", nil)
	if session == "" {
		w.t.Fatal("precondition: initialize issued no Mcp-Session-Id")
	}
	w.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, session, nil)
	return session
}

// create calls the tool and returns the stored item's activity.
func (w *bug2772World) create(title, session string) (createdBy, agent string) {
	w.t.Helper()
	raw, _ := w.post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_task","arguments":{"title":`+mustJSONString(title)+`}}}`, session, map[string]string{"MCP-Protocol-Version": "2025-11-25"})
	if strings.Contains(raw, `"isError":true`) {
		w.t.Fatalf("create %q failed: %s", title, raw)
	}
	return w.stored(title)
}

func (w *bug2772World) stored(title string) (createdBy, agent string) {
	w.t.Helper()
	items, err := w.s.ListItems(w.ws.ID, models.ItemListParams{CollectionSlug: "tasks"})
	if err != nil {
		w.t.Fatal(err)
	}
	for _, it := range items {
		if it.Title != title {
			continue
		}
		acts, err := w.s.ListDocumentActivity(it.ID, models.ActivityListParams{})
		if err != nil {
			w.t.Fatal(err)
		}
		for _, a := range acts {
			if a.Action == "created" {
				return it.CreatedBy, models.AgentNameFromMetadata(a.Metadata)
			}
		}
		w.t.Fatalf("no created activity for %q", title)
	}
	w.t.Fatalf("no item titled %q", title)
	return "", ""
}

// call runs a tool and returns its raw JSON-RPC response.
func (w *bug2772World) call(tool, args, session string) string {
	w.t.Helper()
	raw, _ := w.post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+args+`}}`, session, map[string]string{"MCP-Protocol-Version": "2025-11-25"})
	return raw
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestBUG2772_RemoteMCPWritesAreAgentWrites(t *testing.T) {
	w := newBUG2772World(t)

	t.Run("a legacy session's declared client names the write", func(t *testing.T) {
		session := w.initialize("claude-code")
		by, agent := w.create("from claude code", session)
		if by != "agent" || agent != "claude-code" {
			t.Fatalf("created_by=%q agent=%q, want agent / claude-code", by, agent)
		}
	})

	t.Run("two sessions keep their own names", func(t *testing.T) {
		a := w.initialize("cursor")
		b := w.initialize("chatgpt")
		if _, agent := w.create("from cursor", a); agent != "cursor" {
			t.Errorf("session a wrote as %q", agent)
		}
		if _, agent := w.create("from chatgpt", b); agent != "chatgpt" {
			t.Errorf("session b wrote as %q", agent)
		}
	})

	t.Run("a client that declared no name: still an agent write, unnamed", func(t *testing.T) {
		session := w.initialize("")
		by, agent := w.create("from a nameless client", session)
		if by != "agent" || agent != "" {
			t.Fatalf("created_by=%q agent=%q, want agent / (none)", by, agent)
		}
	})

	t.Run("a session this instance never saw: an agent write, unnamed, never a guessed name", func(t *testing.T) {
		by, agent := w.create("from an unknown session", "pad-mcp-"+uuid.NewString())
		if by != "agent" || agent != "" {
			t.Fatalf("created_by=%q agent=%q, want agent / (none)", by, agent)
		}
	})

	t.Run("another user's session id carries no name", func(t *testing.T) {
		// The transport accepts any Mcp-Session-Id, so a caller who learned
		// another account's session id can send it. The name that session
		// declared stays with the account that declared it.
		session := w.initialize("claude-code")
		title := "borrowed session id"
		raw, _ := w.post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_task","arguments":{"title":`+mustJSONString(title)+`}}}`,
			session, map[string]string{"MCP-Protocol-Version": "2025-11-25", bug2772UserHeader: "other"})
		if strings.Contains(raw, `"isError":true`) {
			t.Fatalf("create failed: %s", raw)
		}
		if by, agent := w.stored(title); by != "agent" || agent != "" {
			t.Fatalf("created_by=%q agent=%q, want agent / (none): the other user wrote under the owner's session name", by, agent)
		}
		// The owner, on the same session, still gets its name.
		if _, agent := w.create("owner on its own session", session); agent != "claude-code" {
			t.Fatalf("the owner's own session lost its name: %q", agent)
		}
	})

	t.Run("CONTROL: the dispatcher's own request, minus the remote-caller mark, is the human's", func(t *testing.T) {
		// buildHTTPRequest is what the dispatcher sends, without the
		// WithRemoteMCPCaller mark: so the mark, and nothing else about the
		// in-process request, is what makes the writes above agent writes.
		req, err := buildHTTPRequest(context.Background(), http.MethodPost,
			"/api/v1/workspaces/"+w.ws.Slug+"/collections/tasks/items", []byte(`{"title":"typed by the human"}`), w.owner)
		if err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		w.srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
		}
		if by, agent := w.stored("typed by the human"); by != "user" || agent != "" {
			t.Fatalf("created_by=%q agent=%q, want user / (none)", by, agent)
		}
	})
}

func TestBUG2772_RemoteLeaseHolderIsPerConnection(t *testing.T) {
	// Lead ruling: a remote caller's default lease holder is its client name
	// plus a short session id, so two connections of one client do not
	// silently share a claim, and next/ready agree with the claim default
	// (both go through defaultLeaseHolder).
	w := newBUG2772World(t)
	tasks, err := w.s.GetCollectionBySlug(w.ws.ID, "tasks")
	if err != nil || tasks == nil {
		t.Fatalf("tasks: %v", err)
	}
	it, err := w.s.CreateItem(w.ws.ID, tasks.ID, models.ItemCreate{Title: "Claim me", Fields: `{"status":"open","priority":"critical"}`})
	if err != nil {
		t.Fatal(err)
	}
	a := w.initialize("claude-code")
	b := w.initialize("claude-code")

	if raw := w.call("claim_task", `{"ref":"`+it.Ref+`"}`, a); strings.Contains(raw, `"isError":true`) {
		t.Fatalf("session a's claim failed: %s", raw)
	}
	lease, err := w.s.GetItemLease(it.ID)
	if err != nil || lease == nil {
		t.Fatalf("no lease stored: %v", err)
	}
	if want := "claude-code#" + shortSessionID(a); lease.Holder != want {
		t.Fatalf("holder = %q, want %q", lease.Holder, want)
	}

	// The same client on another connection is a different holder, refused
	// as lease_held with the holder and expiry (BUG-3496: the remote
	// classifier used to drop that code and answer `conflict`).
	raw := w.call("claim_task", `{"ref":"`+it.Ref+`"}`, b)
	var resp struct {
		Result struct {
			IsError           bool `json:"isError"`
			StructuredContent struct {
				Error struct {
					Code    string `json:"code"`
					Hint    string `json:"hint"`
					Details struct {
						Holder    string `json:"holder"`
						ExpiresAt string `json:"expires_at"`
					} `json:"details"`
				} `json:"error"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil || !resp.Result.IsError {
		t.Fatalf("session b claimed an item session a holds (err %v): %s", err, raw)
	}
	e := resp.Result.StructuredContent.Error
	if e.Code != string(ErrLeaseHeld) || e.Hint != LeaseHeldHint {
		t.Fatalf("code = %q hint = %q, want lease_held with LeaseHeldHint: %s", e.Code, e.Hint, raw)
	}
	if e.Details.Holder != lease.Holder || e.Details.ExpiresAt == "" {
		t.Fatalf("details holder = %q expires_at = %q, want %q and an expiry: %s",
			e.Details.Holder, e.Details.ExpiresAt, lease.Holder, raw)
	}

	// next/ready: the holder still sees its own claim; the other does not.
	if raw := w.call("next", `{}`, a); !strings.Contains(raw, it.Ref) {
		t.Errorf("session a's own claim was hidden from its next: %s", raw)
	}
	if raw := w.call("next", `{}`, b); strings.Contains(raw, it.Ref) {
		t.Errorf("session b was offered an item session a holds: %s", raw)
	}
}

func TestBUG2772_ResolveRemoteCaller(t *testing.T) {
	r := NewClientRegistry()

	t.Run("a modern request's own _meta clientInfo wins", func(t *testing.T) {
		ctx := server.WithRequestProtocolInfo(context.Background(), &server.RequestProtocolInfo{
			Modern: true, ClientInfo: &mcp.Implementation{Name: "modern-client"},
		})
		c := r.resolveRemoteCaller(ctx)
		if c.Name != "modern-client" || c.LeaseHolder != "modern-client" {
			t.Fatalf("got %+v", c)
		}
	})

	t.Run("nothing declared and no session: unnamed, with a fallback lease holder", func(t *testing.T) {
		c := r.resolveRemoteCaller(context.Background())
		if c.Name != "" || c.LeaseHolder != remoteAgentFallback {
			t.Fatalf("got %+v", c)
		}
	})

	t.Run("short session id", func(t *testing.T) {
		if got := shortSessionID("pad-mcp-3f9a12c4-0000"); got != "3f9a12c4" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("a nil registry still resolves (unnamed)", func(t *testing.T) {
		var nilReg *ClientRegistry
		if c := nilReg.resolveRemoteCaller(context.Background()); c.Name != "" {
			t.Fatalf("got %+v", c)
		}
	})
}

func TestCLIAndMCPAgreeOnLeaseHeld(t *testing.T) {
	if cli.LeaseHeldCode != string(ErrLeaseHeld) {
		t.Errorf("code strings differ: cli %q, mcp %q", cli.LeaseHeldCode, ErrLeaseHeld)
	}
	if cli.LeaseHeldHint != LeaseHeldHint {
		t.Errorf("hints differ:\n cli %q\n mcp %q", cli.LeaseHeldHint, LeaseHeldHint)
	}
}
