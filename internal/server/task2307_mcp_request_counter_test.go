package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/metrics"
)

// TASK-2307: pad_mcp_http_requests_total{mount, method, client} counts each
// authenticated request reaching an MCP mount, so a week of Cloud traffic
// says whether any client opens the GET stream a stateless go-sdk transport
// would refuse.

func mcpRequestUA(srv *Server, method, path, token, ua string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// Through the real route table, on both mounts: authenticated requests are
// counted by mount, method and client; a refused one is not.
func TestTASK2307_CountsAuthenticatedRequestsOnBothMounts(t *testing.T) {
	srv := chatGPTMountServer(t, true)
	srv.metrics = metrics.New()
	sess := newOAuthSession(t, srv)
	mcpTok, _ := mintWithResource(t, srv, sess, testCanonicalAudience)
	gptTok, _ := mintWithResource(t, srv, sess, testChatGPTResource)

	for _, c := range []struct{ method, path, token, ua, stub string }{
		{"GET", "/mcp", mcpTok, "claude-code/2.1.0", "mcp"},
		{"GET", "/mcp", mcpTok, "claude-code/2.1.0", "mcp"},
		{"POST", "/mcp", mcpTok, "Cursor/1.4", "mcp"},
		{"DELETE", "/mcp", mcpTok, "", "mcp"},
		{"GET", "/mcp/chatgpt", gptTok, "openai-mcp/1.0.0", "chatgpt"},
	} {
		if rr := mcpRequestUA(srv, c.method, c.path, c.token, c.ua); rr.Code != http.StatusOK || rr.Body.String() != c.stub {
			t.Fatalf("premise: %s %s reached %d %q, want the %s transport", c.method, c.path, rr.Code, rr.Body.String(), c.stub)
		}
	}
	// Refused before a caller is known: left to the pre-auth counter.
	if rr := mcpRequestUA(srv, "GET", "/mcp", "", "claude-code/2.1.0"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("premise: tokenless GET answered %d, want 401", rr.Code)
	}

	c := srv.metrics.MCPHTTPRequestsTotal
	for _, w := range []struct {
		mount, method, client string
		want                  float64
	}{
		{"mcp", "GET", "claude-code", 2},
		{"mcp", "POST", "cursor", 1},
		{"mcp", "DELETE", "none", 1},
		{"chatgpt", "GET", "openai", 1},
		{"chatgpt", "GET", "claude-code", 0},
	} {
		if got := counterValue(t, c.WithLabelValues(w.mount, w.method, w.client)); got != w.want {
			t.Errorf("{%s,%s,%s} = %v, want %v", w.mount, w.method, w.client, got, w.want)
		}
	}
}

func TestTASK2307_ClientClass(t *testing.T) {
	for ua, want := range map[string]string{
		"":                                  "none",
		"   ":                               "none",
		"claude-code/2.1.0 (external, cli)": "claude-code",
		"Claude-User":                       "claude",
		"codex_cli_rs/0.40.0":               "codex",
		"openai-mcp/1.0.0":                  "openai",
		"ChatGPT-User/1.0":                  "openai",
		"Cursor/1.4.2":                      "cursor",
		"Windsurf/1.0":                      "windsurf",
		"Visual Studio Code/1.95":           "vscode",
		"mcp-remote/0.1.29":                 "mcp-remote",
		"python-httpx/0.27.0":               "python",
		"node":                              "node",
		"Go-http-client/2.0":                "go",
		"curl/8.5.0":                        "curl",
		"Mozilla/5.0 (X11; Linux x86_64)":   "other",
	} {
		if got := mcpClientClass(ua); got != want {
			t.Errorf("mcpClientClass(%q) = %q, want %q", ua, got, want)
		}
	}
}

// The label vocabulary is closed: no rule can emit a class a dashboard does
// not know, and nothing from the header itself reaches the label.
func TestTASK2307_ClientClassVocabularyIsClosed(t *testing.T) {
	known := map[string]bool{"none": true, "other": true}
	for _, r := range mcpClientRules {
		known[r.class] = true
		if r.needle != strings.ToLower(r.needle) {
			t.Errorf("rule needle %q is not lowercase, so it can never match", r.needle)
		}
	}
	if len(known) > 20 {
		t.Errorf("%d client classes; keep the label small", len(known))
	}
	for _, ua := range []string{"attacker-chosen/9.9", strings.Repeat("x", 4096), "\x00\xff"} {
		if got := mcpClientClass(ua); !known[got] {
			t.Errorf("mcpClientClass(%q) = %q, outside the vocabulary", ua, got)
		}
	}
}

// A caller refused by the per-token rate limit inside MCPBearerAuth is not
// counted (codex round 2): the counter's total equals the requests the
// transport actually received, with the 429 left to
// pad_mcp_authz_denials_total.
func TestTASK2307_RateLimitedRequestsAreNotCounted(t *testing.T) {
	srv := mcpEnabledTestServer(t)
	srv.metrics = metrics.New()
	pat := mustCreatePATForTest(t, srv, "task2307-rate-limit")

	admitted, refused := 0, 0
	for i := 0; i < 80 && refused == 0; i++ {
		rr := mcpRequestUA(srv, "POST", "/mcp", pat, "claude-code/2.1.0")
		if rr.Code == http.StatusTooManyRequests {
			refused++
		} else {
			admitted++
		}
	}
	if refused == 0 {
		t.Fatalf("premise: no 429 within 80 requests on one token")
	}
	got := counterValue(t, srv.metrics.MCPHTTPRequestsTotal.WithLabelValues("mcp", "POST", "claude-code"))
	if got != float64(admitted) {
		t.Errorf("counted %v, want the %d admitted requests (the 429 must not count)", got, admitted)
	}
}
