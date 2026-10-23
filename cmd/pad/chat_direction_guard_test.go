package main

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/mcp"
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
	for action := range def.Actions {
		actions = append(actions, action)
	}
	slices.Sort(actions)
	if !reflect.DeepEqual(actions, []string{"delete", "inbox", "list", "read", "send", "show"}) {
		t.Fatalf("action set: %v", actions)
	}
	for _, action := range actions {
		d := &chatGuardDispatcher{}
		input := map[string]any{"target": "C12", "body": "hello", "seq": float64(4), "to": float64(4),
			"arm": true, "auto_arm": true, "accept": true, "accept_instructions": true, "instructions": true, "instruction": true}
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
				for _, forbidden := range []string{"arm", "accept", "instruction"} {
					if strings.Contains(arg, forbidden) {
						t.Fatalf("%s emits %q", action, arg)
					}
				}
			}
		}
	}
}
