package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/models"
	padserver "github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func chatTestDoc() *cmdhelp.Document {
	return &cmdhelp.Document{Commands: map[string]cmdhelp.Command{
		"chat list":   {},
		"chat inbox":  {Flags: map[string]cmdhelp.Flag{"agent": {Type: "string"}}},
		"chat show":   {Args: []cmdhelp.Arg{{Name: "target", Required: true}}, Flags: map[string]cmdhelp.Flag{"since": {Type: "int64"}, "limit": {Type: "int"}, "no-mark": {Type: "bool"}}},
		"chat send":   {Args: []cmdhelp.Arg{{Name: "target", Required: true}, {Name: "body", Required: true}}, Flags: map[string]cmdhelp.Flag{"reply-to": {Type: "int64"}, "op-key": {Type: "string"}, "context-workspace": {Type: "string"}}},
		"chat delete": {Args: []cmdhelp.Arg{{Name: "target", Required: true}, {Name: "seq", Required: true}}},
		"chat read":   {Args: []cmdhelp.Arg{{Name: "target", Required: true}}, Flags: map[string]cmdhelp.Flag{"to": {Type: "int64"}}},
	}}
}

func TestChatRemoteStoredIdentity(t *testing.T) {
	s := storetest.NewSQLite(t)
	srv := padserver.New(s)
	t.Cleanup(srv.Stop)
	owner, err := s.CreateUser(models.UserCreate{Email: "chat@example.com", Name: "Owner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	clients := NewClientRegistry()
	d := &HTTPHandlerDispatcher{Handler: srv, Clients: clients, UserResolver: func(context.Context) *models.User { return owner }}
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
	session := world.initialize("rook")
	raw := world.call("pad_chat", `{"action":"send","target":"@rook","body":"hello"}`, session)
	if strings.Contains(raw, `"isError":true`) {
		t.Fatalf("send: %s", raw)
	}
	actor := models.ConversationActor{UserID: owner.ID, AgentKey: "rook"}
	convs, err := s.ListConversations(actor)
	if err != nil || len(convs) != 1 {
		t.Fatalf("conversations: %+v %v; response %s", convs, err, raw)
	}
	page, err := s.ListConversationMessages(actor, convs[0].ID, nil, nil, 50)
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("messages: %+v %v", page, err)
	}
	// MUTANT KILLED: changing handleSendChatMessage's source rule to read
	// Authorization instead of isBearerAuth (or source="api" for API-token
	// auth) loses source="cli" on these headerless in-process requests.
	msg := page.Messages[0]
	if msg.Source != "cli" || msg.AuthorAgentKey != "rook" {
		t.Fatalf("stored source=%q agent=%q", msg.Source, msg.AuthorAgentKey)
	}

	// Exercise the real handler/store read markers and the remaining routes.
	assertRead := func(want int64) {
		t.Helper()
		page, err := s.ListConversationMessages(actor, convs[0].ID, nil, nil, 50)
		if err != nil || page.ReadSeq != want {
			t.Fatalf("read_seq: page=%+v err=%v, want %d", page, err, want)
		}
	}
	call := func(args string) string {
		t.Helper()
		raw := world.call("pad_chat", args, session)
		var result struct {
			Result struct {
				IsError bool `json:"isError"`
			}
		}
		if err := json.Unmarshal([]byte(raw), &result); err != nil || result.Result.IsError || strings.Contains(raw, `"error":`) {
			t.Fatalf("call %s: %s (%v)", args, raw, err)
		}
		return raw
	}
	assertRead(0) // A plain send leaves the sender's marker alone.
	for i := 0; i < 4; i++ {
		if _, err := s.SendConversationMessage(models.ConversationSend{
			ConversationID: convs[0].ID, Author: models.ConversationActor{UserID: owner.ID},
			Body: fmt.Sprintf("owner %d", i), Source: "web",
		}); err != nil {
			t.Fatal(err)
		}
	}
	call(`{"action":"show","target":"@rook","since":0,"limit":2}`)
	assertRead(2) // last_seq is 5, while this page returned only 1 and 2.
	call(`{"action":"show","target":"@rook","since":2,"limit":1,"no_mark":true}`)
	assertRead(2)
	call(`{"action":"send","target":"@rook","body":"reply","reply_to":4}`)
	assertRead(4) // The new message is seq 6.
	call(`{"action":"list"}`)
	call(`{"action":"inbox","agent":"rook"}`)
	call(fmt.Sprintf(`{"action":"delete","target":"C%d","seq":6}`, convs[0].Number))
	page, err = s.ListConversationMessages(actor, convs[0].ID, nil, nil, 50)
	if err != nil || len(page.Messages) != 6 || !page.Messages[5].Deleted || page.Messages[5].Body != "" {
		t.Fatalf("delete: %+v %v", page, err)
	}
	call(fmt.Sprintf(`{"action":"read","target":"%d","to":5}`, convs[0].Number))
	assertRead(5)
	raw = world.call("pad_chat", `{"action":"show","target":"@missing"}`, session)
	if !strings.Contains(raw, `"isError":true`) || !strings.Contains(raw, `"code":"not_found"`) {
		t.Fatalf("unknown DM show: %s", raw)
	}
	if rows, err := s.ListConversations(models.ConversationActor{UserID: owner.ID}); err != nil || len(rows) != 1 {
		t.Fatalf("show unexpectedly ensured DM: %+v %v", rows, err)
	}

	unnamed := world.initialize("")
	raw = world.call("pad_chat", `{"action":"list"}`, unnamed)
	var response struct {
		Result struct {
			IsError           bool `json:"isError"`
			StructuredContent struct {
				Error struct {
					Code string `json:"code"`
				}
			} `json:"structuredContent"`
		}
	}
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Result.IsError || response.Result.StructuredContent.Error.Code != "agent_name_required" {
		t.Fatalf("want structured agent_name_required: %s", raw)
	}
}

func TestChatRemoteReadMarkers(t *testing.T) {
	for _, tc := range []struct {
		name, verb string
		input      map[string]any
		page       string
		want       int64
	}{
		{"show returned max", "show", map[string]any{"target": "C12", "since": float64(2), "limit": float64(3)}, `{"messages":[{"seq":7},{"seq":4}],"last_seq":99,"newest_seq":98}`, 7},
		{"show zero limit", "show", map[string]any{"target": "12", "limit": float64(0), "no_mark": true}, `{"messages":[{"seq":7}],"last_seq":99}`, 0},
		{"show no mark", "show", map[string]any{"target": "12", "no_mark": true}, `{"messages":[{"seq":7}],"last_seq":99}`, 0},
		{"show empty", "show", map[string]any{"target": "12"}, `{"messages":[],"last_seq":99}`, 0},
		{"send reply", "send", map[string]any{"target": "C12", "body": "hi", "reply_to": float64(4)}, `{"message":{"seq":100},"replayed":false}`, 4},
		{"send plain", "send", map[string]any{"target": "12", "body": "hi"}, `{"message":{"seq":100},"replayed":false}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var marks []int64
			d := &HTTPHandlerDispatcher{UserResolver: func(context.Context) *models.User { return &models.User{ID: "u"} },
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/me/conversations/12/messages":
						if tc.name == "show zero limit" && r.URL.Query().Get("limit") != "0" {
							t.Errorf("zero limit should reach the server: %s", r.URL.RawQuery)
						}
						if tc.name == "show returned max" && (r.URL.Query().Get("after_seq") != "2" || r.URL.Query().Get("limit") != "3") {
							t.Errorf("query: %s", r.URL.RawQuery)
						}
						if tc.verb == "send" {
							var in map[string]any
							_ = json.NewDecoder(r.Body).Decode(&in)
							if _, ok := in["op_key"]; ok {
								t.Error("unsolicited op_key")
							}
							if tc.want != 0 && in["reply_to_seq"] != float64(4) {
								t.Errorf("send: %v", in)
							}
						}
						fmt.Fprint(w, tc.page)
					case "/api/v1/me/conversations/12/read":
						var in struct{ Seq int64 }
						_ = json.NewDecoder(r.Body).Decode(&in)
						marks = append(marks, in.Seq)
						fmt.Fprintf(w, `{"read_seq":%d}`, in.Seq)
					default:
						t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
					}
				}),
			}
			res, err := d.Dispatch(WithDispatchInput(context.Background(), tc.input), []string{"chat", tc.verb}, nil)
			if err != nil || res.IsError {
				t.Fatalf("result: %v %s", err, textOf(res))
			}
			if tc.want == 0 && len(marks) != 0 || tc.want != 0 && (len(marks) != 1 || marks[0] != tc.want) {
				t.Fatalf("marks: %v want=%d", marks, tc.want)
			}
		})
	}
}
