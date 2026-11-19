package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// An ask that only carries an agent role reaches no person: it sits in that
// role's queue and nobody sees it in Needs you. The single-item create and
// update doors say so with warnings.unrouted_ask, and stay byte-identical
// (no warnings key) for every other write.

const unroutedWaitingSchema = `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","needs-answer","done"],"default":"open","required":true,"awaits_human_options":["needs-answer"]}]}`

type unroutedFixture struct {
	*nfx
	owner, alice, outsider *models.User
	ws                     *models.Workspace
	roleID                 string
	tok                    string
}

func newUnroutedFixture(t *testing.T, srv *Server) *unroutedFixture {
	t.Helper()
	f := &unroutedFixture{nfx: newNfx(t, srv)}
	f.owner, f.alice, f.outsider = f.user("owner"), f.user("alice"), f.user("outsider")
	f.ws = f.workspace("Asks", f.owner)
	f.member(f.ws, f.alice, "editor")
	role, err := srv.store.CreateAgentRole(f.ws.ID, models.AgentRoleCreate{Name: "Seat"})
	if err != nil {
		t.Fatal(err)
	}
	f.roleID = role.ID
	tok, err := srv.store.CreateSession(f.owner.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.tok = tok
	return f
}

// warnings decodes the response's warnings object, or nil when the key is
// absent from the raw JSON.
func unroutedWarnings(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rr.Body.String(), err)
	}
	raw, ok := body["warnings"]
	if !ok {
		return nil
	}
	var w map[string]any
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestItemUpdate_UnroutedAskWarning(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newUnroutedFixture(t, srv)
		tasks, err := srv.store.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Tasks", Schema: unroutedWaitingSchema})
		if err != nil {
			t.Fatal(err)
		}
		mk := func(title string, assignee *models.User) *models.Item {
			t.Helper()
			in := models.ItemCreate{Title: title, Fields: `{"status":"open"}`, AgentRoleID: &f.roleID}
			if assignee != nil {
				in.AssignedUserID = &assignee.ID
			}
			it, err := srv.store.CreateItem(f.ws.ID, tasks.ID, in)
			if err != nil {
				t.Fatal(err)
			}
			return it
		}
		patch := func(it *models.Item, body map[string]any) *httptest.ResponseRecorder {
			t.Helper()
			rr := doRequestWithCookie(srv, http.MethodPatch, "/api/v1/workspaces/"+f.ws.Slug+"/items/"+it.ID, body, f.tok)
			if rr.Code != http.StatusOK {
				t.Fatalf("PATCH %s: %d %s", it.Title, rr.Code, rr.Body.String())
			}
			return rr
		}
		waiting := map[string]any{"fields_patch": map[string]any{"status": "needs-answer"}}

		t.Run("role only, into a waiting status: warns", func(t *testing.T) {
			w := unroutedWarnings(t, patch(mk("role only", nil), waiting))
			if w["unrouted_ask"] != true {
				t.Fatalf("warnings = %v, want unrouted_ask true", w)
			}
		})
		t.Run("an ineligible assignee and a role: warns", func(t *testing.T) {
			it := mk("ineligible assignee", nil)
			f.forceAssign(it, f.outsider)
			w := unroutedWarnings(t, patch(it, waiting))
			if w["unrouted_ask"] != true {
				t.Fatalf("warnings = %v, want unrouted_ask true", w)
			}
		})
		t.Run("an eligible assignee: no warning", func(t *testing.T) {
			if w := unroutedWarnings(t, patch(mk("assigned", f.alice), waiting)); w != nil {
				t.Fatalf("warnings = %v, want none", w)
			}
		})
		t.Run("an open mention of an eligible person: no warning", func(t *testing.T) {
			it := mk("mentioned", nil)
			f.comment(f.ws, it, f.owner, "@alice-nx can you answer this?")
			if w := unroutedWarnings(t, patch(it, waiting)); w != nil {
				t.Fatalf("warnings = %v, want none", w)
			}
		})
		t.Run("not in a waiting status: no warning", func(t *testing.T) {
			if w := unroutedWarnings(t, patch(mk("not an ask", nil), map[string]any{"title": "renamed"})); w != nil {
				t.Fatalf("warnings = %v, want none", w)
			}
		})
		t.Run("a terminal status: no warning", func(t *testing.T) {
			if w := unroutedWarnings(t, patch(mk("closed", nil), map[string]any{"fields_patch": map[string]any{"status": "done"}})); w != nil {
				t.Fatalf("warnings = %v, want none", w)
			}
		})
	})
}

func TestItemCreate_UnroutedAskWarning(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newUnroutedFixture(t, srv)
		f.collection(f.ws, "Questions", true)
		create := func(body map[string]any) *httptest.ResponseRecorder {
			t.Helper()
			rr := doRequestWithCookie(srv, http.MethodPost, "/api/v1/workspaces/"+f.ws.Slug+"/collections/questions/items", body, f.tok)
			if rr.Code != http.StatusCreated {
				t.Fatalf("POST: %d %s", rr.Code, rr.Body.String())
			}
			return rr
		}
		if w := unroutedWarnings(t, create(map[string]any{"title": "for the seat", "agent_role_id": f.roleID})); w["unrouted_ask"] != true {
			t.Fatalf("role only: warnings = %v, want unrouted_ask true", w)
		}
		rr := create(map[string]any{"title": "for alice", "agent_role_id": f.roleID, "assigned_user_id": f.alice.ID})
		if w := unroutedWarnings(t, rr); w != nil {
			t.Fatalf("assigned: warnings = %v, want no warnings key; body %s", w, rr.Body.String())
		}
	})
}
