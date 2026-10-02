package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3331 over remote /mcp: pad_search shares /api/v1/search's handler, so
// a signed-in non-member naming another workspace's slug must find nothing
// there either. Built through wireMCP (newCapFixture), the production binding.
func TestBUG3331_RemoteMCPSearchScopesToVisibleItems(t *testing.T) {
	f := newCapFixture(t, cfgHTTP, false, nil, "true")
	if err := f.store.SeedDefaultCollections(f.ws.ID); err != nil {
		t.Logf("seed collections: %v (may already be seeded)", err)
	}
	docs, err := f.store.GetCollectionBySlug(f.ws.ID, "docs")
	if err != nil || docs == nil {
		t.Fatalf("docs collection: %v", err)
	}
	if _, err := f.store.CreateItem(f.ws.ID, docs.ID, models.ItemCreate{Title: "Wombat plan", Content: "wombat"}); err != nil {
		t.Fatal(err)
	}
	outsider, err := f.store.CreateUser(models.UserCreate{Email: "outsider@example.com", Name: "Outsider", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := f.store.CreateAPIToken(outsider.ID, models.APITokenCreate{Name: "o"}, 30, 0)
	if err != nil {
		t.Fatal(err)
	}

	count := func(pat string) int {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"pad_search","arguments":{"action":"query","workspace":"` + f.ws.Slug + `","query":"wombat"}}}`
		rr := f.doWithBearer(t, capRoute{method: "POST", path: "/mcp", contentType: "application/json", body: body}, pat)
		if rr.Code != http.StatusOK {
			t.Fatalf("tools/call pad_search: %d %.300s", rr.Code, rr.Body.String())
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
		if err := json.Unmarshal([]byte(raw), &env); err != nil || len(env.Result.Content) == 0 {
			t.Fatalf("decode: %v (%.400s)", err, raw)
		}
		if env.Result.IsError {
			t.Fatalf("pad_search errored: %.400s", env.Result.Content[0].Text)
		}
		var resp struct {
			Results []json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal([]byte(env.Result.Content[0].Text), &resp); err != nil {
			t.Fatalf("decode results: %v (%.400s)", err, env.Result.Content[0].Text)
		}
		return len(resp.Results)
	}

	if n := count(f.memberPAT); n != 1 {
		t.Fatalf("control: the workspace owner should find the item once, found %d", n)
	}
	if n := count(tok.Token); n != 0 {
		t.Fatalf("a non-member found %d result(s) in another workspace over remote MCP", n)
	}
}
