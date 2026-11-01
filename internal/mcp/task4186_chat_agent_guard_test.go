package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	padserver "github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

type chatRecorder struct {
	next http.Handler
	mu   sync.Mutex
	reqs []string
}

func (c *chatRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.reqs = append(c.reqs, r.Method+" "+r.URL.Path)
	c.mu.Unlock()
	c.next.ServeHTTP(w, r)
}

func (c *chatRecorder) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

func (c *chatRecorder) since(n int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.reqs[n:]...)
}

// TASK-4186 PR C: the remote resolveChatTarget refuses send to another agent's @name
// before any list or POST (show, read and delete keep not_found), and does not name any other agent.
func TestChatRemoteAgentTargetGuard(t *testing.T) {
	s := storetest.NewSQLite(t)
	srv := padserver.New(s)
	t.Cleanup(srv.Stop)
	owner, err := s.CreateUser(models.UserCreate{Email: "chat4186@example.com", Name: "Owner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnsureAgentDM(owner.ID, "secret-sibling"); err != nil {
		t.Fatal(err)
	}
	rec := &chatRecorder{next: srv}
	clients := NewClientRegistry()
	d := &HTTPHandlerDispatcher{Handler: rec, Clients: clients, UserResolver: func(context.Context) *models.User { return owner }}
	ms := NewServer(Options{Version: "test", Clients: clients})
	if _, err := RegisterCatalog(ms.MCP(), CatalogOptions{Doc: chatTestDoc(), Workspace: NewWorkspaceState(""), Dispatcher: d}); err != nil {
		t.Fatal(err)
	}
	transport := NewRemoteTransport(ms.MCP(), bug2772Sessions{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		transport.ServeHTTP(w, r.WithContext(padserver.WithCurrentUser(r.Context(), owner)))
	}))
	t.Cleanup(ts.Close)
	world := &bug2772World{t: t, s: s, srv: srv, owner: owner, url: ts.URL + "/mcp"}
	session := world.initialize("director")

	// Send is guarded: a validation_failed refusal from the guard's own text,
	// and no request to /me/conversations.
	sendArgs := `{"action":"send","target":"@dave","body":"hi"}`
	before := rec.count()
	raw := world.call("pad_chat", sendArgs, session)
	if !strings.Contains(raw, `"isError":true`) {
		t.Fatalf("send: want an error result, got %s", raw)
	}
	if !strings.Contains(raw, "validation") {
		t.Fatalf("send: want a validation code, got %s", raw)
	}
	if !strings.Contains(raw, "one chat") {
		t.Fatalf("send: refusal is not the guard's text: %s", raw)
	}
	if strings.Contains(raw, "secret-sibling") {
		t.Fatalf("send: refusal names another agent: %s", raw)
	}
	if !strings.Contains(raw, "@director") {
		t.Fatalf("send: refusal does not name the caller's own DM: %s", raw)
	}
	if n := rec.count(); n != before {
		t.Fatalf("send: refusal still sent %v", rec.since(before))
	}

	// show, read and delete keep the truthful not_found: the guard is not
	// invoked, so the lookup runs and finds no such DM.
	for _, args := range []string{
		`{"action":"show","target":"@dave"}`,
		`{"action":"read","target":"@dave","to":1}`,
		`{"action":"delete","target":"@dave","seq":1}`,
	} {
		before := rec.count()
		raw := world.call("pad_chat", args, session)
		if !strings.Contains(raw, `"isError":true`) || !strings.Contains(raw, `"code":"not_found"`) {
			t.Fatalf("%s: want not_found, got %s", args, raw)
		}
		if strings.Contains(raw, "one chat") || strings.Contains(raw, "secret-sibling") {
			t.Fatalf("%s: guard text or another agent in result: %s", args, raw)
		}
		if got := rec.since(before); len(got) != 1 || got[0] != "GET /api/v1/me/conversations" {
			t.Fatalf("%s: want exactly the lookup, got %v", args, got)
		}
	}

	raw = world.call("pad_chat", `{"action":"send","target":"@director","body":"hello"}`, session)
	if strings.Contains(raw, `"isError":true`) {
		t.Fatalf("own DM send refused: %s", raw)
	}
}
