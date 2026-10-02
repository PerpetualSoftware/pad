package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3326: over remote /mcp, pad_set_workspace embeds the workspace
// bootstrap, as it always did over stdio. The server is built through
// wireMCP (the production binding, CONVE-19), so a fetcher constructed but
// not passed to the registry fails here.

// setWorkspaceOverRemote calls pad_set_workspace through /mcp with the
// given PAT and returns the tool's decoded JSON payload.
func (f *capFixture) setWorkspaceOverRemote(t *testing.T, pat, ws string) map[string]any {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"pad_set_workspace","arguments":{"workspace":"` + ws + `"}}}`
	req := capRoute{method: "POST", path: "/mcp", contentType: "application/json", body: body}
	rr := f.doWithBearer(t, req, pat)
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/call pad_set_workspace: status %d body %.300s", rr.Code, rr.Body.String())
	}
	raw := rr.Body.String()
	// The streamable transport may answer as an SSE frame; take its data line.
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
		Error any `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("decode JSON-RPC response: %v (%.300s)", err, raw)
	}
	if env.Error != nil || env.Result.IsError || len(env.Result.Content) != 1 {
		t.Fatalf("pad_set_workspace did not succeed: %.500s", raw)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(env.Result.Content[0].Text), &out); err != nil {
		t.Fatalf("decode tool payload: %v (%.300s)", err, env.Result.Content[0].Text)
	}
	return out
}

func (f *capFixture) doWithBearer(t *testing.T, rt capRoute, pat string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
	req.Host = f.host
	req.RemoteAddr = "192.0.2.10:4242"
	req.Header.Set("Content-Type", rt.contentType)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+pat)
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, req)
	return rr
}

func bootstrapWorkspaceSlug(t *testing.T, out map[string]any) (string, bool) {
	t.Helper()
	bs, ok := out["bootstrap"].(map[string]any)
	if !ok {
		return "", false
	}
	ws, _ := bs["workspace"].(map[string]any)
	slug, _ := ws["slug"].(string)
	return slug, true
}

func TestBUG3326_RemoteSetWorkspaceEmbedsBootstrap(t *testing.T) {
	f := newCapFixture(t, cfgHTTP, false, nil, "true")

	out := f.setWorkspaceOverRemote(t, f.memberPAT, f.ws.Slug)
	if out["status"] != "not_persisted" {
		t.Fatalf("status = %v, want not_persisted (shared state): %v", out["status"], out)
	}
	slug, ok := bootstrapWorkspaceSlug(t, out)
	if !ok {
		t.Fatalf("remote pad_set_workspace carries no bootstrap: %v", out)
	}
	if slug != f.ws.Slug {
		t.Fatalf("bootstrap.workspace.slug = %q, want %q", slug, f.ws.Slug)
	}
}

// The embed is read as the TOKEN's user: a workspace the caller is not a
// member of embeds nothing, while its owner gets it (the control showing
// the absence is scoping, not a broken fetch).
func TestBUG3326_RemoteSetWorkspaceBootstrapIsScopedToCaller(t *testing.T) {
	f := newCapFixture(t, cfgHTTP, false, nil, "true")
	other, err := f.store.CreateWorkspace(models.WorkspaceCreate{Name: "Admin Only", Slug: "admin-only", OwnerID: f.admin.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := f.store.AddWorkspaceMember(other.ID, f.admin.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}

	if slug, ok := bootstrapWorkspaceSlug(t, f.setWorkspaceOverRemote(t, f.adminPAT, other.Slug)); !ok || slug != other.Slug {
		t.Fatalf("control: the owner's call should embed %q's bootstrap, got ok=%v slug=%q", other.Slug, ok, slug)
	}
	out := f.setWorkspaceOverRemote(t, f.memberPAT, other.Slug)
	if _, ok := out["bootstrap"]; ok {
		t.Fatalf("a non-member's pad_set_workspace embedded another workspace's bootstrap: %v", out)
	}
}
