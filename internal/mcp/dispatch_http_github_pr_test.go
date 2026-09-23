package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2696: a remote MCP `field: ["github_pr=..."]` setter used to store the
// PR as a string (no link rendered) and `github_pr=null` stored "null". It is
// now refused, naming the CLI writer, and nothing is stored.
func TestDispatch_ItemUpdate_RefusesAGithubPRFieldSetter(t *testing.T) {
	srv, st := newPadServer(t)
	wsRec := doJSONReq(t, srv, http.MethodPost, "/api/v1/workspaces", map[string]any{"name": "GP"})
	var ws models.Workspace
	_ = json.Unmarshal(wsRec.Body.Bytes(), &ws)
	owner, err := st.CreateUser(models.UserCreate{Email: "gp@example.com", Name: "GP", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	d := &HTTPHandlerDispatcher{Handler: srv, UserResolver: fixedUserResolver(owner)}

	res, err := d.Dispatch(WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "collection": "tasks", "title": "T",
	}), []string{"item", "create"}, nil)
	if err != nil || res.IsError {
		t.Fatalf("create: %v %#v", err, res)
	}
	ref := res.StructuredContent.(map[string]any)["ref"].(string)

	for _, v := range []string{
		`github_pr={"number":7,"url":"https://github.com/o/r/pull/7"}`,
		`github_pr=null`,
	} {
		up, err := d.Dispatch(WithDispatchInput(context.Background(), map[string]any{
			"workspace": ws.Slug, "ref": ref, "field": []any{v},
		}), []string{"item", "update"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !up.IsError {
			t.Fatalf("%s: the field setter was accepted: %#v", v, up.StructuredContent)
		}
		raw, _ := json.Marshal(up.Content)
		if !strings.Contains(string(raw), "pad github link") {
			t.Errorf("%s: the refusal must name the writer: %s", v, raw)
		}
	}

	show, _ := d.Dispatch(WithDispatchInput(context.Background(), map[string]any{"workspace": ws.Slug, "ref": ref}), []string{"item", "show"}, nil)
	fields := itemFieldsAsMap(t, show.StructuredContent.(map[string]any))
	if _, stored := fields["github_pr"]; stored {
		t.Fatalf("github_pr was stored despite the refusal: %v", fields["github_pr"])
	}
}
