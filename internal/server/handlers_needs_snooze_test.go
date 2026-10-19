package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestNeedsSnoozeRoute_Visibility drives PUT|DELETE .../needs/snooze through
// the router (TASK-3918): a viewer and an item-grant guest may snooze, a
// member restricted away from the item's collection and a stranger get 404
// and write nothing, and a read-scoped token is refused 403.
func TestNeedsSnoozeRoute_Visibility(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
		map[string]interface{}{"title": "Asked", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatal(err)
	}
	ideas, err := srv.store.GetCollectionBySlug(ws.ID, "ideas")
	if err != nil || ideas == nil {
		t.Fatalf("ideas collection: %v", err)
	}

	user := func(email string) *models.User {
		u, err := srv.store.CreateUser(models.UserCreate{Email: email, Name: email, Password: "pw-test-12345"})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	token := func(u *models.User, scopes string) string {
		tok, err := srv.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "t", Scopes: scopes}, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok.Token
	}
	owner := user("owner@example.com")
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	viewer := user("viewer@example.com")
	if err := srv.store.AddWorkspaceMember(ws.ID, viewer.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	restricted := user("restricted@example.com")
	if err := srv.store.AddWorkspaceMember(ws.ID, restricted.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, restricted.ID, "specific", []string{ideas.ID}); err != nil {
		t.Fatal(err)
	}
	guest := user("guest@example.com")
	if _, err := srv.store.CreateItemGrant(ws.ID, item.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}
	stranger := user("stranger@example.com")

	path := "/api/v1/workspaces/" + slug + "/items/" + item.Slug + "/needs/snooze"
	dismiss, _ := json.Marshal(map[string]any{"kind": "dismiss"})
	snoozed := func(u *models.User) bool {
		t.Helper()
		sn, err := srv.store.GetNeedsSnooze(u.ID, item.ID, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return sn != nil
	}

	for _, tc := range []struct {
		name string
		u    *models.User
		want int
	}{
		{"viewer", viewer, http.StatusOK},
		{"item-grant guest", guest, http.StatusOK},
		{"restricted member", restricted, http.StatusNotFound},
		{"stranger", stranger, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := token(tc.u, "")
			rr := bearerCall(t, srv, http.MethodPut, path, tok, dismiss)
			if rr.Code != tc.want {
				t.Fatalf("PUT: %d %s, want %d", rr.Code, rr.Body.String(), tc.want)
			}
			if got := snoozed(tc.u); got != (tc.want == http.StatusOK) {
				t.Fatalf("snooze row after PUT %d: %v", rr.Code, got)
			}
			rr = bearerCall(t, srv, http.MethodDelete, path, tok, nil)
			if rr.Code != tc.want {
				t.Fatalf("DELETE: %d %s, want %d", rr.Code, rr.Body.String(), tc.want)
			}
			if snoozed(tc.u) {
				t.Fatal("snooze row survived DELETE")
			}
		})
	}

	t.Run("read-scoped token", func(t *testing.T) {
		rr := bearerCall(t, srv, http.MethodPut, path, token(viewer, `["read"]`), dismiss)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("PUT with a read token: %d %s, want 403", rr.Code, rr.Body.String())
		}
		if snoozed(viewer) {
			t.Fatal("a read token wrote a snooze")
		}
	})

	t.Run("bad defer", func(t *testing.T) {
		tok := token(viewer, "")
		for _, body := range []map[string]any{
			{"kind": "defer", "defer_until": "next week"},
			{"kind": "defer", "defer_until": time.Now().Add(-time.Hour).Format(time.RFC3339)},
			{"kind": "mute"},
		} {
			b, _ := json.Marshal(body)
			if rr := bearerCall(t, srv, http.MethodPut, path, tok, b); rr.Code != http.StatusBadRequest {
				t.Errorf("PUT %v: %d %s, want 400", body, rr.Code, rr.Body.String())
			}
		}
		if snoozed(viewer) {
			t.Fatal("a refused PUT wrote a snooze")
		}
		until := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
		b, _ := json.Marshal(map[string]any{"kind": "defer", "defer_until": until.Format(time.RFC3339)})
		rr := bearerCall(t, srv, http.MethodPut, path, tok, b)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT defer: %d %s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Ref    string             `json:"ref"`
			Snooze models.NeedsSnooze `json:"snooze"`
		}
		parseJSON(t, rr, &resp)
		if resp.Ref != item.Ref || resp.Snooze.Kind != "defer" || resp.Snooze.DeferUntil != until.Format(time.RFC3339) || !resp.Snooze.Holds {
			t.Fatalf("PUT defer response: %+v", resp)
		}
	})
}
