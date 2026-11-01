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

// TASK-4186 PR C: the remote resolveChatTarget refuses another agent's @name
// before any list or POST, and does not name any other agent.
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

	for _, args := range []string{
		`{"action":"send","target":"@dave","body":"hi"}`,
		`{"action":"show","target":"@dave"}`,
		`{"action":"read","target":"@dave","to":1}`,
		`{"action":"delete","target":"@dave","seq":1}`,
	} {
		before := rec.count()
		raw := world.call("pad_chat", args, session)
		if !strings.Contains(raw, `"isError":true`) || !strings.Contains(raw, "validation") && !strings.Contains(raw, "one chat, with your owner") {
			t.Fatalf("%s: want a validation refusal, got %s", args, raw)
		}
		if strings.Contains(raw, "secret-sibling") {
			t.Fatalf("%s: refusal names another agent: %s", args, raw)
		}
		if !strings.Contains(raw, "@director") {
			t.Fatalf("%s: refusal does not name the caller's own DM: %s", args, raw)
		}
		for _, r := range rec.reqs[before:] {
			if strings.Contains(r, "/me/conversations") {
				t.Fatalf("%s: refusal still sent %s", args, r)
			}
		}
	}

	raw := world.call("pad_chat", `{"action":"send","target":"@director","body":"hello"}`, session)
	if strings.Contains(raw, `"isError":true`) {
		t.Fatalf("own DM send refused: %s", raw)
	}
}
