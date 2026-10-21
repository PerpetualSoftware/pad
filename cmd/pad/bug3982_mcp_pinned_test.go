package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3982: over remote /mcp, a PAT minted with a workspace_id is confined to
// that workspace, as it is over REST. The server is built through wireMCP, the
// production binding, so the stash in the PAT door and the dispatcher carrying
// it onto its synthesized requests are both under test.

// mcpToolText calls a tool through /mcp with pat and returns (isError, text).
func (f *capFixture) mcpToolText(t *testing.T, pat, tool, args string) (bool, string) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
	rr := f.doWithBearer(t, capRoute{method: "POST", path: "/mcp", contentType: "application/json", body: body}, pat)
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/call %s: status %d body %.300s", tool, rr.Code, rr.Body.String())
	}
	raw := rr.Body.String()
	if i := strings.Index(raw, "data: "); i >= 0 {
		raw = strings.TrimSpace(strings.SplitN(raw[i+len("data: "):], "\n", 2)[0])
	}
	var env struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("decode JSON-RPC response: %v (%.300s)", err, raw)
	}
	var text []string
	for _, c := range env.Result.Content {
		text = append(text, c.Text)
	}
	return env.Result.IsError, strings.Join(text, "\n")
}

func TestBUG3982_MCPToolsConfinedToPin(t *testing.T) {
	f := newCapFixture(t, cfgHTTP, false, nil, "true")
	other, err := f.store.CreateWorkspace(models.WorkspaceCreate{Name: "Other WS", Slug: "other-ws", OwnerID: f.member.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := f.store.AddWorkspaceMember(other.ID, f.member.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	tok, err := f.store.CreateAPIToken(f.member.ID, models.APITokenCreate{Name: "pinned", WorkspaceID: f.ws.ID}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// Controls: the unpinned PAT reads both, the pinned one its own.
	for _, ws := range []string{f.ws.Slug, other.Slug} {
		if isErr, text := f.mcpToolText(t, f.memberPAT, "pad_collection", `{"action":"list","workspace":"`+ws+`"}`); isErr {
			t.Fatalf("control: unpinned PAT, pad_collection list on %s: %.300s", ws, text)
		}
	}
	if isErr, text := f.mcpToolText(t, tok.Token, "pad_collection", `{"action":"list","workspace":"`+f.ws.Slug+`"}`); isErr {
		t.Fatalf("pinned PAT, pad_collection list on its own workspace: %.300s", text)
	}

	// The pinned PAT on another workspace is refused, and the refusal says why.
	isErr, text := f.mcpToolText(t, tok.Token, "pad_collection", `{"action":"list","workspace":"`+other.Slug+`"}`)
	if !isErr || !strings.Contains(text, "limited to workspace") {
		t.Fatalf("pinned PAT, pad_collection list on %s: isError=%v %.400s, want a refusal naming the pin", other.Slug, isErr, text)
	}

	// The workspace list is the pinned one alone.
	isErr, text = f.mcpToolText(t, tok.Token, "pad_workspace", `{"action":"list"}`)
	if isErr || !strings.Contains(text, f.ws.Slug) || strings.Contains(text, other.Slug) {
		t.Fatalf("pinned PAT, pad_workspace list: isError=%v %.400s, want %s only", isErr, text, f.ws.Slug)
	}
}

// A tool that maps to a global route (pad_workspace create is POST
// /api/v1/workspaces) meets the route gate too: the dispatcher's synthesized
// request goes through the full router with the pin on its context.
func TestBUG3982_MCPGlobalRouteToolRefusedForPin(t *testing.T) {
	f := newCapFixture(t, cfgHTTP, false, nil, "true")
	tok, err := f.store.CreateAPIToken(f.member.ID, models.APITokenCreate{Name: "pinned", WorkspaceID: f.ws.ID}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	isErr, text := f.mcpToolText(t, tok.Token, "pad_workspace", `{"action":"create","name":"Pinned Made This","slug":"pinned-made-this"}`)
	if !isErr || !strings.Contains(text, "limited to workspace") {
		t.Fatalf("pinned PAT, pad_workspace create: isError=%v %.400s, want a refusal naming the pin", isErr, text)
	}
	if ws, err := f.store.GetWorkspaceBySlug("pinned-made-this"); err != nil || ws != nil {
		t.Fatalf("pinned PAT's refused create left a workspace: %v, %v", ws, err)
	}

	// Control: the unpinned PAT creates through the same tool.
	if isErr, text := f.mcpToolText(t, f.memberPAT, "pad_workspace", `{"action":"create","name":"Unpinned Made This","slug":"unpinned-made-this"}`); isErr {
		t.Fatalf("control: unpinned PAT, pad_workspace create: %.400s", text)
	}
	if ws, err := f.store.GetWorkspaceBySlug("unpinned-made-this"); err != nil || ws == nil {
		t.Fatalf("control: unpinned PAT's create left no workspace: %v, %v", ws, err)
	}
}
