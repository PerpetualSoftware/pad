package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2308: the /mcp 401 names the scope set, and an OAuth connection that
// can only read gets a 403 insufficient_scope challenge for a write tool
// call, before the transport sees it. Driven through the router, so the
// middleware's place in the /mcp chain is what is tested (CONVE-19).

const wantInsufficientScopeChallenge = `Bearer realm="pad", error="insufficient_scope", ` +
	`error_description="This connection can only read. Reconnect Pad and allow it to edit your workspaces.", ` +
	`scope="pad:read pad:write", ` +
	`resource_metadata="` + testCanonicalAudience + `/.well-known/oauth-protected-resource"`

// recordingMCPTransport answers 200 "mcp" and keeps the body of every
// request that reached it.
type recordingMCPTransport struct{ bodies []string }

func (rt *recordingMCPTransport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rt.bodies = append(rt.bodies, string(b))
	_, _ = w.Write([]byte("mcp"))
}

func (rt *recordingMCPTransport) take() []string {
	out := rt.bodies
	rt.bodies = nil
	return out
}

// scopeTestServer is an OAuth-capable server whose /mcp transport records
// what reaches it, with a classifier calling pad_item.create a write.
func scopeTestServer(t *testing.T) (*Server, *recordingMCPTransport) {
	t.Helper()
	srv := twoResourceOAuthServer(t)
	rt := &recordingMCPTransport{}
	srv.SetMCPTransport(rt, testCanonicalAudience, testAuthServerURL, nil)
	srv.SetMCPWriteCallClassifier(func(tool, action string) bool {
		return tool == "pad_item" && action == "create"
	})
	return srv, rt
}

func postMCPBody(srv *Server, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func toolCallBody(tool, action string) string {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": map[string]any{"action": action, "workspace": "ws"}},
	})
	return string(b)
}

func TestTASK2308_UnauthorizedChallengeNamesScope(t *testing.T) {
	srv, _ := scopeTestServer(t)
	rr := postMCPBody(srv, "/mcp", "", toolCallBody("pad_item", "create"))
	want := `Bearer realm="pad", scope="pad:read pad:write", resource_metadata="` +
		testCanonicalAudience + `/.well-known/oauth-protected-resource"`
	if rr.Code != http.StatusUnauthorized || rr.Header().Get("WWW-Authenticate") != want {
		t.Fatalf("401: %d %q, want %q", rr.Code, rr.Header().Get("WWW-Authenticate"), want)
	}
}

func TestTASK2308_ReadOnlyOAuthWriteCallIsChallenged(t *testing.T) {
	srv, rt := scopeTestServer(t)
	tok, code := mintWithResource(t, srv, newOAuthSession(t, srv), testCanonicalAudience)
	if code != http.StatusOK {
		t.Fatalf("mint: %d", code)
	}
	rr := postMCPBody(srv, "/mcp", tok, toolCallBody("pad_item", "create"))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("read-only OAuth write call: %d %s, want 403", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != wantInsufficientScopeChallenge {
		t.Errorf("challenge = %q\nwant      %q", got, wantInsufficientScopeChallenge)
	}
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Error.Code != "insufficient_scope" {
		t.Errorf("body = %s, want error.code insufficient_scope", rr.Body.String())
	}
	if got := rt.take(); len(got) != 0 {
		t.Errorf("the refused call reached the transport: %q", got)
	}
}

// Everything the check is not about reaches the transport, with its body
// byte for byte.
func TestTASK2308_PassThroughCases(t *testing.T) {
	srv, rt := scopeTestServer(t)
	sess := newOAuthSession(t, srv)
	readTok, code := mintWithResource(t, srv, sess, testCanonicalAudience)
	if code != http.StatusOK {
		t.Fatalf("mint read: %d", code)
	}
	writeTok, code := mintWithResourceTier(t, srv, sess, testCanonicalAudience, "pad:read pad:write", "write")
	if code != http.StatusOK {
		t.Fatalf("mint write: %d", code)
	}
	user, err := srv.store.GetUserByEmail("oauth-test@example.com")
	if err != nil || user == nil {
		t.Fatal(err)
	}
	pat, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{Name: "read pat", Scopes: `["read"]`}, 90, 365)
	if err != nil {
		t.Fatal(err)
	}

	oversized := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pad_item","arguments":{"action":"create","content":"` +
		strings.Repeat("x", mcpScopePeekMaxBytes) + `"}}}`

	cases := []struct {
		name, token, body string
	}{
		{"read token, read action", readTok, toolCallBody("pad_item", "get")},
		{"read token, unknown tool", readTok, toolCallBody("pad_nope", "create")},
		{"read token, initialize", readTok, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`},
		{"read token, batch", readTok, `[` + toolCallBody("pad_item", "create") + `]`},
		{"read token, action under another key's case", readTok,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pad_item","arguments":{"Action":"create"}}}`},
		{"read token, body past the peek bound", readTok, oversized},
		{"write token, write action", writeTok, toolCallBody("pad_item", "create")},
		// A PAT cannot re-authorize, so it keeps the dispatcher's tool error.
		{"read PAT, write action", pat.Token, toolCallBody("pad_item", "create")},
	}
	for _, c := range cases {
		rr := postMCPBody(srv, "/mcp", c.token, c.body)
		if rr.Code != http.StatusOK || rr.Body.String() != "mcp" {
			t.Errorf("%s: %d %q, want 200 from the transport", c.name, rr.Code, rr.Body.String())
			continue
		}
		got := rt.take()
		if len(got) != 1 || got[0] != c.body {
			t.Errorf("%s: the transport did not receive the body intact (%d requests)", c.name, len(got))
		}
	}
}

// With no classifier wired the check is off, and a read-only write call
// reaches the transport as it did before TASK-2308.
func TestTASK2308_NoClassifierNoCheck(t *testing.T) {
	srv, rt := scopeTestServer(t)
	srv.SetMCPWriteCallClassifier(nil)
	tok, code := mintWithResource(t, srv, newOAuthSession(t, srv), testCanonicalAudience)
	if code != http.StatusOK {
		t.Fatalf("mint: %d", code)
	}
	if rr := postMCPBody(srv, "/mcp", tok, toolCallBody("pad_item", "create")); rr.Code != http.StatusOK {
		t.Fatalf("no classifier: %d, want 200", rr.Code)
	}
	if len(rt.take()) != 1 {
		t.Fatal("the call did not reach the transport")
	}
}

// The ChatGPT mount keeps its own tool-result challenge (TASK-3321 U2c):
// the 403 is a /mcp-chain middleware and must not run there.
func TestTASK2308_ChatGPTMountIsNotChallengedHere(t *testing.T) {
	srv := chatGPTMountServer(t, true)
	srv.SetMCPWriteCallClassifier(func(tool, action string) bool { return true })
	gptTok, code := mintWithResource(t, srv, newOAuthSession(t, srv), testChatGPTResource)
	if code != http.StatusOK {
		t.Fatalf("mint: %d", code)
	}
	rr := postMCPBody(srv, "/mcp/chatgpt", gptTok, toolCallBody("pad_item", "create"))
	if rr.Code != http.StatusOK || rr.Body.String() != "chatgpt" {
		t.Fatalf("ChatGPT mount: %d %q, want 200 from its transport", rr.Code, rr.Body.String())
	}
}
