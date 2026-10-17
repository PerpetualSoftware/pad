package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TASK-3866 done-when: a key made read-only through the same request the
// web settings form sends can read and cannot write, both over REST and
// over remote MCP. The MCP leg runs through wireMCP, the production
// wiring, so the catalog, transport and dispatcher are the shipped ones.
func TestTASK3866_ReadOnlyKeyRefusesWritesOverRESTAndMCP(t *testing.T) {
	f := newCapFixture(t, cfgHTTP, false, nil, "true")
	if err := f.store.SeedCollectionsFromTemplate(f.ws.ID, "startup"); err != nil {
		t.Fatal(err)
	}
	session, err := f.store.CreateSession(f.member.ID, "web", "192.0.2.10", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	do := func(method, path, bearer string, body any, header map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&buf).Encode(body); err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(method, path, &buf)
		req.Host = f.host
		req.RemoteAddr = "192.0.2.10:4242"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		rr := newMCPRecorder()
		f.srv.ServeHTTP(rr, req)
		return rr.ResponseRecorder
	}
	mint := func(name, scopes string) string {
		t.Helper()
		rr := do("POST", "/api/v1/auth/tokens", session, map[string]any{"name": name, "scopes": scopes}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("mint %s: %d %s", name, rr.Code, rr.Body.String())
		}
		var tok struct {
			Token  string `json:"token"`
			Scopes string `json:"scopes"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &tok); err != nil || tok.Token == "" {
			t.Fatalf("mint %s: %s", name, rr.Body.String())
		}
		if tok.Scopes != scopes {
			t.Fatalf("mint %s: stored %q, want %q", name, tok.Scopes, scopes)
		}
		return tok.Token
	}
	readKey := mint("bot read", `["read"]`)
	writeKey := mint("bot write", `["*"]`)

	itemsPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/tasks/items"
	countTitled := func(title string) int {
		t.Helper()
		rr := do("GET", itemsPath, writeKey, nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
		}
		return strings.Count(rr.Body.String(), `"title":`+mustJSON(t, title))
	}

	t.Run("REST", func(t *testing.T) {
		if rr := do("GET", itemsPath, readKey, nil, nil); rr.Code != http.StatusOK {
			t.Errorf("read key, GET items: %d %s", rr.Code, rr.Body.String())
		}
		rr := do("POST", itemsPath, readKey, map[string]any{"title": "rest by read key"}, nil)
		if rr.Code != http.StatusForbidden {
			t.Errorf("read key, POST item: %d %s, want 403", rr.Code, rr.Body.String())
		}
		if n := countTitled("rest by read key"); n != 0 {
			t.Errorf("the refused REST write created %d items", n)
		}
		if rr := do("POST", itemsPath, writeKey, map[string]any{"title": "rest by write key"}, nil); rr.Code != http.StatusCreated {
			t.Errorf("write key, POST item: %d %s, want 201", rr.Code, rr.Body.String())
		}
	})

	t.Run("MCP", func(t *testing.T) {
		const proto = "2025-11-25"
		mcpCall := func(key string, id int, method string, params map[string]any) string {
			t.Helper()
			body := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
			rr := do("POST", "/mcp", key, body, map[string]string{
				"Accept":               "application/json, text/event-stream",
				"MCP-Protocol-Version": proto,
			})
			if rr.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", method, rr.Code, rr.Body.String())
			}
			return rr.Body.String()
		}
		tool := func(key, action string, args map[string]any) string {
			t.Helper()
			args["action"] = action
			args["workspace"] = f.ws.Slug
			mcpCall(key, 1, "initialize", map[string]any{
				"protocolVersion": proto, "capabilities": map[string]any{},
				"clientInfo": map[string]any{"name": "task3866", "version": "1"},
			})
			return mcpCall(key, 2, "tools/call", map[string]any{"name": "pad_item", "arguments": args})
		}

		if out := tool(readKey, "list", map[string]any{"collection": "tasks"}); strings.Contains(out, `"isError":true`) {
			t.Errorf("read key, pad_item list: %s", out)
		}
		out := tool(readKey, "create", map[string]any{"collection": "tasks", "title": "mcp by read key"})
		if !strings.Contains(out, `"isError":true`) || !strings.Contains(out, "permission_denied") {
			t.Errorf("read key, pad_item create: want an isError permission_denied result, got %s", out)
		}
		if n := countTitled("mcp by read key"); n != 0 {
			t.Errorf("the refused MCP write created %d items", n)
		}
		if out := tool(writeKey, "create", map[string]any{"collection": "tasks", "title": "mcp by write key"}); strings.Contains(out, `"isError":true`) {
			t.Errorf("write key, pad_item create: %s", out)
		}
		if n := countTitled("mcp by write key"); n != 1 {
			t.Errorf("write key over MCP created %d items, want 1", n)
		}
	})
}
