package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/mcp"
	"github.com/PerpetualSoftware/pad/internal/models"
)

type chatGuardDispatcher struct{ path, args []string }

func (d *chatGuardDispatcher) Dispatch(_ context.Context, path, args []string) (*mcp.CallToolResult, error) {
	d.path, d.args = path, args
	return &mcp.CallToolResult{}, nil
}

func TestChatDirectionGuard(t *testing.T) {
	oldFormat, oldWorkspace, oldURL := formatFlag, workspaceFlag, urlFlag
	t.Cleanup(func() { formatFlag, workspaceFlag, urlFlag = oldFormat, oldWorkspace, oldURL })

	root := newRootCmd()
	doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", MaxDepth: -1})
	var def *mcp.ToolDef
	for i := range mcp.Catalog {
		if mcp.Catalog[i].Name == "pad_chat" {
			def = &mcp.Catalog[i]
		}
	}
	if def == nil {
		t.Fatal("pad_chat is absent")
	}
	if def.Schema.Workspace {
		t.Fatal("chat must be global")
	}
	var actions []string
	forbiddenName := regexp.MustCompile(`(?i)arm|accept|instruction|direction`)
	for _, param := range def.Schema.Params {
		if forbiddenName.MatchString(param.Name) {
			t.Errorf("forbidden chat parameter: %q", param.Name)
		}
	}
	for action := range def.Actions {
		if forbiddenName.MatchString(action) {
			t.Errorf("forbidden chat action: %q", action)
		}
		actions = append(actions, action)
	}
	slices.Sort(actions)
	if !reflect.DeepEqual(actions, []string{"delete", "inbox", "list", "read", "send", "show"}) {
		t.Fatalf("action set: %v", actions)
	}
	for _, action := range actions {
		d := &chatGuardDispatcher{}
		input := map[string]any{"target": "C12", "body": "hello", "seq": float64(4), "to": float64(4),
			"arm": true, "auto_arm": true, "accept": true, "accept_instructions": true, "instructions": true, "instruction": true, "direction": true}
		env := mcp.ActionEnv{Doc: doc, Workspace: mcp.NewWorkspaceState(""), Dispatcher: d}
		res, err := def.Actions[action](context.Background(), input, env)
		if err != nil || res.IsError {
			t.Fatalf("%s: %+v %v", action, res, err)
		}
		if !reflect.DeepEqual(d.path, []string{"chat", action}) {
			t.Fatalf("%s maps to %v", action, d.path)
		}
		for _, arg := range d.args {
			if arg == "--" {
				break
			}
			if strings.HasPrefix(arg, "--") {
				if forbiddenName.MatchString(arg) {
					t.Fatalf("%s emits %q", action, arg)
				}
			}
		}
		t.Run("remote/"+action, func(t *testing.T) {
			var requests []string
			remote := &mcp.HTTPHandlerDispatcher{
				UserResolver: func(context.Context) *models.User { return &models.User{ID: "u"} },
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body []byte
					if r.Body != nil {
						var err error
						body, err = io.ReadAll(r.Body)
						if err != nil {
							t.Fatal(err)
						}
					}
					requests = append(requests, r.Method+" "+r.URL.RequestURI()+" "+string(body))
					switch r.Method + " " + r.URL.Path {
					case "GET /api/v1/me/conversations":
						// Send exercises DM creation; other actions exercise lookup.
						if action == "send" {
							fmt.Fprint(w, `{"conversations":[]}`)
						} else {
							fmt.Fprint(w, `{"conversations":[{"number":12,"agent_key":"rook"}]}`)
						}
					case "POST /api/v1/me/conversations":
						fmt.Fprint(w, `{"conversation":{"number":12}}`)
					case "GET /api/v1/me/conversations/12/messages":
						fmt.Fprint(w, `{"messages":[{"seq":1}],"read_seq":0}`)
					case "POST /api/v1/me/conversations/12/messages":
						fmt.Fprint(w, `{"message":{"seq":2}}`)
					case "POST /api/v1/me/conversations/12/read":
						fmt.Fprint(w, `{"read_seq":1}`)
					case "DELETE /api/v1/me/conversations/12/messages/4":
						fmt.Fprint(w, `{"message":{"seq":4,"deleted":true}}`)
					case "GET /api/v1/me/chat/unread":
						fmt.Fprint(w, `{"conversations":[],"total":0}`)
					default:
						t.Errorf("unexpected request: %s %s", r.Method, r.URL)
						http.NotFound(w, r)
					}
				}),
			}
			input["target"], input["reply_to"] = "@rook", float64(1)
			env.Dispatcher = remote
			res, err := def.Actions[action](context.Background(), input, env)
			if err != nil || res == nil || res.IsError {
				t.Fatalf("result: %+v %v", res, err)
			}
			if len(requests) == 0 {
				t.Fatal("no remote requests recorded")
			}
			for _, request := range requests {
				if forbiddenName.MatchString(request) || strings.Contains(strings.ToLower(request), "true") {
					t.Errorf("direction input leaked into request: %s", request)
				}
			}
		})
	}
}

func TestChatStdioSendRefusesStdin(t *testing.T) {
	oldFormat, oldWorkspace, oldURL := formatFlag, workspaceFlag, urlFlag
	t.Cleanup(func() { formatFlag, workspaceFlag, urlFlag = oldFormat, oldWorkspace, oldURL })
	for _, def := range mcp.Catalog {
		if def.Name != "pad_chat" {
			continue
		}
		d := &chatGuardDispatcher{}
		root := newRootCmd()
		env := mcp.ActionEnv{Doc: cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", MaxDepth: -1}), Workspace: mcp.NewWorkspaceState(""), Dispatcher: d}
		res, err := def.Actions["send"](context.Background(), map[string]any{"target": "@rook", "body": "-"}, env)
		if err != nil || res == nil || !res.IsError {
			t.Fatalf("want validation error, got %+v %v", res, err)
		}
		raw, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "validation_failed") || !strings.Contains(string(raw), "body '-' reads stdin, which MCP does not provide; pass the text") {
			t.Fatalf("wrong error: %s", raw)
		}
		if len(d.path) != 0 {
			t.Fatalf("stdin body dispatched: %v", d.path)
		}
		return
	}
	t.Fatal("pad_chat is absent")
}
