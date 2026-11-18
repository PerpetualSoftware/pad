package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestWorkspaceRenameMCPContract(t *testing.T) {
	if _, ok := padWorkspaceTool.Actions["rename"]; !ok {
		t.Fatal("rename is not registered")
	}
	props := buildToolFromDef(padWorkspaceTool).InputSchema.Properties
	if !extractEnumStrings(props["action"].(map[string]any)["enum"])["rename"] {
		t.Fatal("rename missing from action enum")
	}
	for _, key := range []string{"workspace", "new_slug", "name"} {
		if _, ok := props[key]; !ok {
			t.Errorf("missing schema property %s", key)
		}
	}
	mapper := routeTable["workspace rename"]
	if mapper == nil {
		t.Fatal("missing HTTP route")
	}
	for _, input := range []map[string]any{{}, {"workspace": "old"}, {"new_slug": "new"}, {"workspace": "", "new_slug": "new"}, {"workspace": "old", "new_slug": ""}} {
		if _, _, _, err := mapper(input); err == nil {
			t.Errorf("accepted incomplete input %v", input)
		}
	}
	method, path, body, err := mapper(map[string]any{"workspace": "old", "new_slug": "new", "name": "New name"})
	if err != nil || method != http.MethodPatch || path != "/api/v1/workspaces/old/slug" {
		t.Fatalf("mapping: %s %s %s %v", method, path, body, err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"expected_slug": "old", "slug": "new", "name": "New name"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("body = %v, want %v", got, want)
	}
	_, _, body, err = mapper(map[string]any{"workspace": "old", "new_slug": "new"})
	if err != nil {
		t.Fatal(err)
	}
	// Unmarshal into a fresh map so an absent name cannot be carried over.
	got = nil
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["name"]; ok {
		t.Fatalf("optional name must be omitted: %v", got)
	}
}

func TestWorkspaceRenameStdioDispatch(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("session-default"), Dispatcher: disp}
	res, err := actionWorkspaceRename(context.Background(), map[string]any{"workspace": "old", "new_slug": "new", "name": "New Name"}, env)
	if err != nil || res == nil || res.IsError {
		t.Fatalf("dispatch: %v, %v", res, err)
	}
	if !equalStrings(disp.gotPath, []string{"workspace", "rename"}) {
		t.Fatalf("path = %v", disp.gotPath)
	}
	if !equalStrings(disp.gotArgs, []string{"--name", "New Name", "--workspace", "old", "--format", "json", "--", "old", "new"}) {
		t.Fatalf("args = %v", disp.gotArgs)
	}
	for _, in := range []map[string]any{{"new_slug": "new"}, {"workspace": "old"}} {
		result, err := actionWorkspaceRename(context.Background(), in, env)
		if err != nil || result == nil || !result.IsError {
			t.Errorf("incomplete input %v accepted: %v, %v", in, result, err)
		}
	}
}
