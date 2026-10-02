package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3321 U2b: the ChatGPT catalog's mount at /mcp/chatgpt, driven
// through the REAL router (route ordering included). Each mount's stub
// transport answers with its own name, so a request reaching the wrong one
// is visible.

func namedStub(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, name)
	})
}

func chatGPTMountServer(t *testing.T, enabled bool) *Server {
	t.Helper()
	srv := twoResourceOAuthServer(t)
	srv.SetMCPTransport(namedStub("mcp"), testCanonicalAudience, testAuthServerURL, nil)
	srv.SetChatGPTMCPTransport(namedStub("chatgpt"), testChatGPTResource, enabled, nil)
	return srv
}

func postMCP(srv *Server, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func TestU2b_MountRoutesAndBindsTokens(t *testing.T) {
	srv := chatGPTMountServer(t, true)
	sess := newOAuthSession(t, srv)
	mcpTok, _ := mintWithResource(t, srv, sess, testCanonicalAudience)
	gptTok, _ := mintWithResource(t, srv, sess, testChatGPTResource)

	// The ChatGPT token reaches the ChatGPT transport, not /mcp's.
	rr := postMCP(srv, "/mcp/chatgpt", gptTok)
	if rr.Code != http.StatusOK || rr.Body.String() != "chatgpt" {
		t.Fatalf("ChatGPT token at /mcp/chatgpt: %d %q, want 200 from the chatgpt transport", rr.Code, rr.Body.String())
	}
	// /mcp is unchanged and still its own transport.
	if rr := postMCP(srv, "/mcp", mcpTok); rr.Code != http.StatusOK || rr.Body.String() != "mcp" {
		t.Fatalf("/mcp token at /mcp: %d %q", rr.Code, rr.Body.String())
	}
	// Cross-refusal through the router, both ways.
	if rr := postMCP(srv, "/mcp/chatgpt", mcpTok); rr.Code != http.StatusUnauthorized {
		t.Errorf("/mcp token at /mcp/chatgpt: %d, want 401", rr.Code)
	}
	if rr := postMCP(srv, "/mcp", gptTok); rr.Code != http.StatusUnauthorized {
		t.Errorf("ChatGPT token at /mcp: %d, want 401", rr.Code)
	}
	// The 401 on the ChatGPT mount names ITS metadata document.
	rr = postMCP(srv, "/mcp/chatgpt", "")
	if got := rr.Header().Get("WWW-Authenticate"); !strings.Contains(got, `resource_metadata="`+testCanonicalAudience+`/.well-known/oauth-protected-resource/mcp/chatgpt"`) {
		t.Errorf("ChatGPT mount challenge = %q, want its own resource_metadata", got)
	}
}

// OAuth-only (lead's ruling): a personal access token is refused at the
// ChatGPT mount with a clear 401, before the PAT branch that would accept it
// without the audience check.
func TestU2b_PersonalAccessTokensAreRefused(t *testing.T) {
	srv := chatGPTMountServer(t, true)
	// A REAL, valid PAT: the case the PAT branch would otherwise accept.
	user, _ := loginTestUserAs(t, srv, "pat-owner@example.com", "Pat Owner", "correct-horse-battery-staple")
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "PAT WS", Slug: "pat-ws", OwnerID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{Name: "u2b", WorkspaceID: ws.ID}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	pat := tok.Token
	if rr := postMCP(srv, "/mcp", pat); rr.Code != http.StatusOK {
		t.Fatalf("fixture: the PAT is not valid at /mcp (%d %s)", rr.Code, rr.Body.String())
	}
	rr := postMCP(srv, "/mcp/chatgpt", pat)
	if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), "OAuth sign-in only") {
		t.Fatalf("PAT at /mcp/chatgpt: %d %s, want 401 naming OAuth-only", rr.Code, rr.Body.String())
	}
	if rr.Body.String() == "chatgpt" {
		t.Fatal("a PAT reached the ChatGPT transport")
	}
}

// Off, the mount and its metadata document are a 404; on, the document
// names the ChatGPT resource, and /mcp's document is unchanged.
func TestU2b_SwitchAndMetadata(t *testing.T) {
	off := chatGPTMountServer(t, false)
	for _, p := range []string{"/mcp/chatgpt", "/.well-known/oauth-protected-resource/mcp/chatgpt"} {
		req := httptest.NewRequest("GET", p, nil)
		if p == "/mcp/chatgpt" {
			req = httptest.NewRequest("POST", p, strings.NewReader(`{}`))
		}
		rr := httptest.NewRecorder()
		off.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("switched off: %s = %d, want 404", p, rr.Code)
		}
	}
	on := chatGPTMountServer(t, true)
	read := func(p string) map[string]any {
		rr := doRequest(on, "GET", p, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s = %d", p, rr.Code)
		}
		var doc map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &doc)
		return doc
	}
	if got := read("/.well-known/oauth-protected-resource/mcp/chatgpt")["resource"]; got != testChatGPTResource {
		t.Errorf("ChatGPT metadata resource = %v, want %s", got, testChatGPTResource)
	}
	if got := read("/.well-known/oauth-protected-resource")["resource"]; got != testCanonicalAudience {
		t.Errorf("/mcp metadata resource = %v, want %s (unchanged)", got, testCanonicalAudience)
	}
}

// The consent screen names what is being connected when the request is for
// the ChatGPT resource (lead's ruling), and says nothing extra for /mcp.
func TestU2b_ConsentNamesTheChatGPTSurface(t *testing.T) {
	srv := chatGPTMountServer(t, true)
	_, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	page := func(resource string) string {
		q := url.Values{
			"client_id":             {clientID},
			"response_type":         {"code"},
			"redirect_uri":          {"https://app.test/cb"},
			"scope":                 {"pad:read"},
			"code_challenge":        {s256Challenge("verifier-u2b-consent-abcdefghijklmnopqrstuvwxyz0123")},
			"code_challenge_method": {"S256"},
			"state":                 {"state-u2b-consent"},
			"resource":              {resource},
		}
		rr := doRequestWithCookie(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sessionToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("consent page for %s: %d %s", resource, rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}
	if body := page(testChatGPTResource); !strings.Contains(body, "Connecting Pad's <strong>ChatGPT</strong> tools") {
		t.Errorf("the ChatGPT consent page does not name the surface")
	}
	if body := page(testCanonicalAudience); strings.Contains(body, "Connecting Pad's") {
		t.Errorf("the /mcp consent page names a surface it is not connecting")
	}
}

// The ChatGPT mount is only wired while ChatGPTDoorVersionsContent says the
// door versions content (TASK-3321 U2b's prerequisite gate). The constant
// is held to the behaviour: true must mean two ChatGPT body edits within the
// throttle window leave two versions.
func TestChatGPTDoorVersionsContentIsTrue(t *testing.T) {
	if !ChatGPTDoorVersionsContent {
		t.Skip("the door does not claim to version content; the mount stays unwired")
	}
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	path := "/api/v1/workspaces/" + slug + "/items/" + item.Slug
	for _, body := range []string{"one", "two"} {
		if rr := chatGPTPatch(t, srv, path, map[string]any{"content": body}); rr.Code != http.StatusOK {
			t.Fatalf("edit: %d %s", rr.Code, rr.Body.String())
		}
	}
	if n := countSource(versionSources(t, srv, item.ID), models.VersionSourceChatGPT); n != 2 {
		t.Fatalf("ChatGPTDoorVersionsContent is true but two edits left %d chatgpt versions", n)
	}
}
