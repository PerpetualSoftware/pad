package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	mcpserver "github.com/PerpetualSoftware/pad/internal/mcp"
	padserver "github.com/PerpetualSoftware/pad/internal/server"
)

// TASK-3321 U2c: a ChatGPT write tool called with a read-only grant answers
// the insufficient_scope challenge that makes ChatGPT ask for the wider
// grant, and nothing is dispatched. Driven through the production
// transport, behind the mount's resource stamp, so the request context the
// challenge is built from is the one a real call carries.

const scopeTestResource = "https://mcp.example.test/mcp/chatgpt"

type chatGPTScopeHarness struct {
	t   *testing.T
	ts  *httptest.Server
	rec *recordingDispatcher
}

func newChatGPTScopeHarness(t *testing.T) *chatGPTScopeHarness {
	t.Helper()
	root := newRootCmd()
	doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", Version: fullVersion(), Homepage: padHomepage, MaxDepth: -1})
	rec := &recordingDispatcher{}
	gpt, err := newChatGPTMCPServer(doc, rec)
	if err != nil {
		t.Fatal(err)
	}
	transport := newChatGPTTransport(gpt)
	// What MCPBearerAuth stashes: the token's scopes, as a JSON array.
	withScopes := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := padserver.WithTokenScopes(r.Context(), r.Header.Get("X-Test-Scopes"))
		transport.ServeHTTP(w, r.WithContext(ctx))
	})
	ts := httptest.NewServer(padserver.WithMCPResource(scopeTestResource)(withScopes))
	t.Cleanup(ts.Close)
	return &chatGPTScopeHarness{t: t, ts: ts, rec: rec}
}

func (h *chatGPTScopeHarness) post(body, session, scopes string) ([]byte, string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("X-Test-Scopes", scopes)
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return raw, resp.Header.Get("Mcp-Session-Id")
}

type scopeCallResult struct {
	IsError bool           `json:"isError"`
	Meta    map[string]any `json:"_meta"`
}

// call runs one tools/call with the given token scopes.
func (h *chatGPTScopeHarness) call(tool string, args map[string]any, scopes string) scopeCallResult {
	h.t.Helper()
	_, session := h.post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+goldenClientInfo+`}`, "", scopes)
	h.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, session, scopes)
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	raw, _ := h.post(string(body), session, scopes)
	var env struct {
		Result scopeCallResult `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		h.t.Fatalf("%s: undecodable response %s", tool, raw)
	}
	return env.Result
}

func scopeTestArgs(e mcpserver.ChatGPTTool) map[string]any {
	sample := map[string]any{
		"workspace": "ws", "ref": "TASK-1", "query": "login", "collection": "tasks",
		"title": "A title", "message": "A comment", "target": "TASK-2", "content": "Body",
		"status": "open", "limit": float64(5), "link_type": "blocks",
	}
	args := map[string]any{}
	for _, p := range e.Params {
		if v, ok := sample[p]; ok {
			args[p] = v
		}
	}
	return args
}

func challengeOf(r scopeCallResult) string {
	list, _ := r.Meta["mcp/www_authenticate"].([]any)
	if len(list) != 1 {
		return ""
	}
	s, _ := list[0].(string)
	return s
}

func TestChatGPTScope_ReadGrantOnAWriteToolIsChallenged(t *testing.T) {
	h := newChatGPTScopeHarness(t)
	const wantMeta = `resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource/mcp/chatgpt"`
	writes := 0
	for _, e := range mcpserver.ChatGPTCatalog {
		if e.Hints.ReadOnly {
			continue
		}
		writes++
		// `null` is what an OAuth token with no granted scope is stashed
		// as (oauthScopesToJSON); the legacy unrestricted PAT forms never
		// reach this mount, which refuses PATs.
		for _, scopes := range []string{`["pad:read"]`, `null`} {
			res := h.call(e.Name, scopeTestArgs(e), scopes)
			c := challengeOf(res)
			if !res.IsError || !strings.HasPrefix(c, "Bearer ") || !strings.Contains(c, wantMeta) ||
				!strings.Contains(c, `error="insufficient_scope"`) || !strings.Contains(c, `error_description="`) {
				t.Errorf("%s with %s: isError=%v challenge=%q", e.Name, scopes, res.IsError, c)
			}
			if calls := h.rec.take(); len(calls) != 0 {
				t.Errorf("%s with %s dispatched %d calls", e.Name, scopes, len(calls))
			}
		}
	}
	if writes == 0 {
		t.Fatal("the catalog has no write tools; the leg proves nothing")
	}
}

// The challenge fires only on a missing scope: a read tool with a read
// grant, and a write tool with a write grant, reach the dispatcher.
func TestChatGPTScope_SufficientGrantsAreNotChallenged(t *testing.T) {
	h := newChatGPTScopeHarness(t)
	for _, e := range mcpserver.ChatGPTCatalog {
		scopes := `["pad:read"]`
		if !e.Hints.ReadOnly {
			scopes = `["pad:write"]`
		}
		res := h.call(e.Name, scopeTestArgs(e), scopes)
		if c := challengeOf(res); c != "" {
			t.Errorf("%s with %s was challenged: %q", e.Name, scopes, c)
		}
		if calls := h.rec.take(); len(calls) == 0 {
			t.Errorf("%s with %s dispatched nothing", e.Name, scopes)
		}
	}
}

// tools/list asked for the way ChatGPT asks (Accept: application/json,
// text/event-stream) answers a JSON body whose every tool carries
// securitySchemes at the top level. Streamable HTTP can answer with an
// event stream instead, but only by flushing notifications mid-request,
// and WithChatGPTToolSchemes hands the transport a writer that cannot
// flush (TestBufferedResponseCannotStream), so the answer is JSON.
func TestChatGPTToolsList_JSONWithTopLevelSchemes(t *testing.T) {
	h := newChatGPTScopeHarness(t)
	_, session := h.post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+goldenClientInfo+`}`, "", `["pad:read"]`)
	h.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, session, `["pad:read"]`)
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("tools/list answered %q, want a JSON body: %s", ct, raw)
	}
	var env struct {
		Result struct {
			Tools []map[string]json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("undecodable tools/list: %s", raw)
	}
	if len(env.Result.Tools) != len(mcpserver.ChatGPTCatalog) {
		t.Fatalf("tools/list listed %d tools, want %d", len(env.Result.Tools), len(mcpserver.ChatGPTCatalog))
	}
	for _, tool := range env.Result.Tools {
		var meta struct {
			SecuritySchemes json.RawMessage `json:"securitySchemes"`
		}
		_ = json.Unmarshal(tool["_meta"], &meta)
		if len(tool["securitySchemes"]) == 0 || string(tool["securitySchemes"]) != string(meta.SecuritySchemes) {
			t.Errorf("%s: top-level securitySchemes %s, _meta copy %s", tool["name"], tool["securitySchemes"], meta.SecuritySchemes)
		}
	}
}
