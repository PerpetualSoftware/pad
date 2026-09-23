package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3056. `pad item note` / `decide` (and the remote /mcp doors) appended
// by GET, append locally, PATCH the whole fields blob back, so a write to any
// other key landing between the GET and the PATCH was reverted. The item
// PATCH now takes append_implementation_note / append_decision and appends to
// the row the store re-reads under its write lock.

// appendRequest is authedAgentRequest without the fatal on a non-2xx, for the
// refusal legs.
func appendRequest(t *testing.T, srv *Server, token, agent, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if agent != "" {
		req.Header.Set("X-Pad-Agent", agent)
	}
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func storedItem(t *testing.T, srv *Server, ws, slug string) *models.Item {
	t.Helper()
	w, err := srv.store.GetWorkspaceBySlug(ws)
	if err != nil || w == nil {
		t.Fatalf("GetWorkspaceBySlug(%q): %v", ws, err)
	}
	it, err := srv.store.ResolveItem(w.ID, slug)
	if err != nil || it == nil {
		t.Fatalf("ResolveItem(%q): %v", slug, err)
	}
	return it
}

// TestItemAppend_ConcurrentPatchSurvives is the regression test. A rival
// commits `status: done` inside the append request's read-to-write window;
// the append must land AND the rival's status must survive.
//
// The window is driven through afterItemPreRead (see BUG-2776's test for why a
// seam rather than racing goroutines). The defect it discriminates: an append
// computed from any read taken before the store's lock — the handler's own
// pre-read here, the client's GET before this change — writes `status: open`
// back over the rival.
func TestItemAppend_ConcurrentPatchSurvives(t *testing.T) {
	cases := []struct {
		name   string
		body   map[string]any
		landed func(fields string) bool
	}{
		{"note", map[string]any{"append_implementation_note": map[string]any{"summary": "checkpoint"}},
			func(f string) bool { return len(models.ExtractItemImplementationNotes(f)) == 1 }},
		{"decision", map[string]any{"append_decision": map[string]any{"decision": "use the lock"}},
			func(f string) bool { return len(models.ExtractItemDecisionLog(f)) == 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := testServer(t)
			token, ws, slug := debounceFixture(t, srv)
			path := "/api/v1/workspaces/" + ws + "/items/" + slug

			fired := 0
			srv.afterItemPreRead = func(string) {
				if fired > 0 {
					return
				}
				fired++
				srv.afterItemPreRead = nil
				authedAgentRequest(t, srv, token, "rival", "PATCH", path,
					map[string]any{"fields_patch": map[string]any{"status": "done"}})
			}
			authedAgentRequest(t, srv, token, "mine", "PATCH", path, tc.body)
			srv.afterItemPreRead = nil
			if fired != 1 {
				t.Fatalf("seam fired %d times, want 1 — the window was not exercised", fired)
			}

			got := storedItem(t, srv, ws, slug)
			var fields map[string]any
			if err := json.Unmarshal([]byte(got.Fields), &fields); err != nil {
				t.Fatalf("stored fields: %v", err)
			}
			if fields["status"] != "done" {
				t.Errorf("the rival's status write was reverted by the append: status=%v", fields["status"])
			}
			if !tc.landed(got.Fields) {
				t.Errorf("the append did not land: %s", got.Fields)
			}
		})
	}
}

// TestItemAppend_ServerMintsTheEntry: id, created_at and created_by come from
// the server, never the body — a forged id is ignored — and the response
// echoes exactly the entry that was stored.
func TestItemAppend_ServerMintsTheEntry(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, slug := debounceFixture(t, srv)
	path := "/api/v1/workspaces/" + ws + "/items/" + slug

	for _, tc := range []struct{ agent, want string }{{"some-agent", "agent"}, {"", "user"}} {
		rr := authedAgentRequest(t, srv, token, tc.agent, "PATCH", path, map[string]any{
			"append_implementation_note": map[string]any{
				"summary": "s", "details": "d",
				"id": "forged", "created_by": "forged", "created_at": "2000-01-01T00:00:00Z",
			},
		})
		var resp models.Item
		decodeAttributionBody(t, rr, &resp)
		if resp.Appended == nil || resp.Appended.ImplementationNote == nil {
			t.Fatalf("response carries no appended entry: %s", rr.Body.String())
		}
		e := resp.Appended.ImplementationNote
		if e.ID == "" || e.ID == "forged" || !strings.HasPrefix(e.ID, "note-") {
			t.Errorf("id = %q, want a server-minted note-… id", e.ID)
		}
		if e.CreatedBy != tc.want {
			t.Errorf("agent %q: created_by = %q, want %q", tc.agent, e.CreatedBy, tc.want)
		}
		if e.CreatedAt == "" || e.CreatedAt == "2000-01-01T00:00:00Z" {
			t.Errorf("created_at = %q, want the server's clock", e.CreatedAt)
		}
		var stored *models.ItemImplementationNote
		for _, n := range models.ExtractItemImplementationNotes(storedItem(t, srv, ws, slug).Fields) {
			if n.ID == e.ID {
				n := n
				stored = &n
			}
		}
		if stored == nil || *stored != *e {
			t.Errorf("echoed entry %+v is not the stored one %+v", *e, stored)
		}
	}
}

// TestItemAppend_RefusedWithFullFields: a full `fields` replace and an append
// in one request have no single meaning, so the request is refused and writes
// nothing. With `fields_patch` both apply.
func TestItemAppend_RefusedWithFullFields(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, slug := debounceFixture(t, srv)
	path := "/api/v1/workspaces/" + ws + "/items/" + slug

	rr := appendRequest(t, srv, token, "", "PATCH", path, map[string]any{
		"fields":          `{"status":"done"}`,
		"append_decision": map[string]any{"decision": "x"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("fields + append = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	after := storedItem(t, srv, ws, slug).Fields
	if strings.Contains(after, "done") || len(models.ExtractItemDecisionLog(after)) != 0 {
		t.Errorf("a refused request wrote something: %s", after)
	}

	rr = appendRequest(t, srv, token, "", "PATCH", path, map[string]any{
		"fields_patch":    map[string]any{"status": "done"},
		"append_decision": map[string]any{"decision": "x"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("fields_patch + append = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	after = storedItem(t, srv, ws, slug).Fields
	if !strings.Contains(after, `"done"`) || len(models.ExtractItemDecisionLog(after)) != 1 {
		t.Errorf("fields_patch + append did not apply both: %s", after)
	}
}

// TestItemAppend_UnreadableStoredValueIsRefused: the Append* guard (BUG-2627
// part 3) runs server-side now. An undecodable stored value answers 409
// stored_state_unreadable and is left byte-for-byte as it was.
func TestItemAppend_UnreadableStoredValueIsRefused(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, slug := debounceFixture(t, srv)
	path := "/api/v1/workspaces/" + ws + "/items/" + slug

	it := storedItem(t, srv, ws, slug)
	broken := `{"status":"open","implementation_notes":"[{\"summary\":\"legacy\"}]","decision_log":"[{\"decision\":\"legacy\"}]"}`
	if _, err := srv.store.UpdateItem(it.ID, models.ItemUpdate{Fields: &broken}); err != nil {
		t.Fatalf("seed broken row: %v", err)
	}
	before := storedItem(t, srv, ws, slug).Fields
	if models.StructuredFieldIsAppendable(before, models.ItemFieldImplementationNotes) {
		t.Fatalf("fixture is not in the defect shape: %s", before)
	}

	for _, body := range []map[string]any{
		{"append_implementation_note": map[string]any{"summary": "new"}},
		{"append_decision": map[string]any{"decision": "new"}},
	} {
		rr := appendRequest(t, srv, token, "", "PATCH", path, body)
		if rr.Code != http.StatusConflict {
			t.Fatalf("append onto unreadable = %d, want 409: %s", rr.Code, rr.Body.String())
		}
		var env struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &env)
		if env.Error.Code != "stored_state_unreadable" {
			t.Errorf("code = %q, want stored_state_unreadable: %s", env.Error.Code, rr.Body.String())
		}
		if after := storedItem(t, srv, ws, slug).Fields; after != before {
			t.Errorf("a refused append changed the row:\n before %s\n after  %s", before, after)
		}
	}
}

// TestItemAppend_PlainPatchHasNoAppendedMember: the echo is additive and
// omitempty, so a write without an append is byte-identical to before.
func TestItemAppend_PlainPatchHasNoAppendedMember(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, slug := debounceFixture(t, srv)
	rr := authedAgentRequest(t, srv, token, "", "PATCH", "/api/v1/workspaces/"+ws+"/items/"+slug,
		map[string]any{"fields_patch": map[string]any{"status": "done"}})
	if strings.Contains(rr.Body.String(), `"appended"`) {
		t.Errorf("a plain PATCH response carries appended: %s", rr.Body.String())
	}
}

// TestServerCapabilities_AdvertisesItemFieldAppend: the CLI decides BEFORE it
// writes whether to send an append, from this flag. Without it the CLI takes
// the legacy full-blob path forever.
func TestServerCapabilities_AdvertisesItemFieldAppend(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/server/capabilities", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("capabilities = %d: %s", rr.Code, rr.Body.String())
	}
	var caps map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &caps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if caps["item_field_append"] != true {
		t.Errorf("item_field_append = %v, want true: %s", caps["item_field_append"], rr.Body.String())
	}
}
