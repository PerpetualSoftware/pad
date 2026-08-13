package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2542. Two write paths took the actor from actorFromRequest and dropped
// it: item CREATE discarded it entirely (keeping only source), and the
// single-item PATCH never set LastModifiedBy at all. Both then fell through to
// the store's `"user"` defaults, so an item created and edited exclusively by
// an agent — one that DID send X-Pad-Agent — read back as human-authored.
//
// Every case runs twice, agent and human, because the bug's signature is that
// the two are indistinguishable. A test that only asserted the agent leg would
// pass against a server that hardcoded "agent", which is the same defect
// pointing the other way.
func TestItemAttribution_AgentVsHuman(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent string // X-Pad-Agent value; empty = human
		want  string
	}{
		{name: "agent write", agent: "claude-code", want: "agent"},
		{name: "human write (control)", agent: "", want: "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			ws := createTestWorkspaceViaAPI(t, srv)

			created := doAttributionRequest(t, srv, tc.agent, "POST",
				"/api/v1/workspaces/"+ws+"/collections/tasks/items",
				map[string]any{"title": "attribution probe"})
			var item models.Item
			decodeAttributionBody(t, created, &item)

			if item.CreatedBy != tc.want {
				t.Errorf("created_by = %q, want %q (X-Pad-Agent=%q)", item.CreatedBy, tc.want, tc.agent)
			}
			// Source is the pre-existing behaviour and must not regress: both
			// legs authenticate the same way, so both are "web" here.
			if item.Source == "" {
				t.Errorf("source was left empty; the create path must still stamp it")
			}

			// The updater is deliberately the OTHER kind of writer. Patching
			// with the same one proves nothing: insertItemTx seeds
			// last_modified_by FROM created_by, so a same-writer update leaves
			// the expected value in place whether or not the PATCH stamps
			// anything — verified by reverting the update stamp alone, which
			// that version of this test passed. The cross leg is the only
			// shape where the update stamp is the sole mechanism that can
			// produce the result.
			crossAgent, crossWant := "", "user"
			if tc.agent == "" {
				crossAgent, crossWant = "claude-code", "agent"
			}
			updated := doAttributionRequest(t, srv, crossAgent, "PATCH",
				"/api/v1/workspaces/"+ws+"/items/"+item.Slug,
				map[string]any{"title": "attribution probe (edited)"})
			var after models.Item
			decodeAttributionBody(t, updated, &after)

			if after.LastModifiedBy != crossWant {
				t.Errorf("last_modified_by after a %s edit = %q, want %q (creator was %q)",
					crossWant, after.LastModifiedBy, crossWant, tc.want)
			}
			// created_by must NOT be rewritten by whoever edited it.
			if after.CreatedBy != tc.want {
				t.Errorf("created_by after update = %q, want %q — an edit must not restamp the creator", after.CreatedBy, tc.want)
			}
		})
	}
}

// An explicit body value still wins, so a caller that knows better than the
// header (an agent recording a write it made on a human's behalf, say) is not
// overridden by it. This is the contract the Source field already had.
func TestItemAttribution_ExplicitBodyValueWins(t *testing.T) {
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)

	rr := doAttributionRequest(t, srv, "claude-code", "POST",
		"/api/v1/workspaces/"+ws+"/collections/tasks/items",
		map[string]any{"title": "explicit", "created_by": "user"})
	var item models.Item
	decodeAttributionBody(t, rr, &item)

	if item.CreatedBy != "user" {
		t.Errorf("created_by = %q, want %q — an explicit body value must beat the header", item.CreatedBy, "user")
	}
}

func doAttributionRequest(t *testing.T, srv *Server, agent, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if agent != "" {
		req.Header.Set("X-Pad-Agent", agent)
	}
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 && rr.Code != 201 {
		t.Fatalf("%s %s = %d: %s", method, path, rr.Code, rr.Body.String())
	}
	return rr
}

func decodeAttributionBody(t *testing.T, rr *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), out); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
}

func createTestWorkspaceViaAPI(t *testing.T, srv *Server) string {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]any{
		"name": "Attribution", "slug": "attribution", "template": "startup",
	})
	if rr.Code != 200 && rr.Code != 201 {
		t.Fatalf("create workspace = %d: %s", rr.Code, rr.Body.String())
	}
	var ws struct {
		Slug string `json:"slug"`
	}
	decodeAttributionBody(t, rr, &ws)
	if ws.Slug == "" {
		t.Fatal("workspace create returned no slug")
	}
	return ws.Slug
}
