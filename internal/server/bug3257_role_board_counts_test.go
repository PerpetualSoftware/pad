package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3257: each lane of GET /roles/board embeds its role, and the store fills
// that role's item_count from every item in the workspace. The lane's items
// were filtered to the caller; the count was not, so a guest and a
// collection-restricted member read how many items they cannot see sat under
// each role. GET /agent-roles already recomputed the count from the visible
// set. Both doors now share that rule. Every role is still listed on both:
// roles are workspace metadata, and only the count describes items.
func TestBUG3257_RoleCountsDescribeOnlyVisibleItems(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug := createWSWithCollections(t, srv)
		ws, err := srv.store.GetWorkspaceBySlug(slug)
		if err != nil || ws == nil {
			t.Fatalf("GetWorkspaceBySlug: %v", err)
		}
		shown, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Shown"})
		if err != nil {
			t.Fatalf("CreateAgentRole: %v", err)
		}
		hidden, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Hidden"})
		if err != nil {
			t.Fatalf("CreateAgentRole: %v", err)
		}
		granted := createItem(t, srv, slug, "tasks", map[string]any{"title": "Granted task", "agent_role_id": shown.ID})
		createItem(t, srv, slug, "tasks", map[string]any{"title": "Other task", "agent_role_id": shown.ID})
		for _, title := range []string{"Idea 1", "Idea 2", "Idea 3"} {
			createItem(t, srv, slug, "ideas", map[string]any{"title": title, "agent_role_id": hidden.ID})
		}

		mkUser := func(name string) *models.User {
			u, err := srv.store.CreateUser(models.UserCreate{Email: name + "-3257@example.com", Name: name, Username: name + "-3257", Password: "pw-test-12345"})
			if err != nil {
				t.Fatalf("CreateUser %s: %v", name, err)
			}
			return u
		}
		owner, guest, restricted := mkUser("owner"), mkUser("guest"), mkUser("restricted")
		if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
			t.Fatalf("AddWorkspaceMember owner: %v", err)
		}
		if _, err := srv.store.CreateItemGrant(ws.ID, granted.ID, guest.ID, "view", owner.ID); err != nil {
			t.Fatalf("CreateItemGrant: %v", err)
		}
		if err := srv.store.AddWorkspaceMember(ws.ID, restricted.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember restricted: %v", err)
		}
		tasks, err := srv.store.GetCollectionBySlug(ws.ID, "tasks")
		if err != nil || tasks == nil {
			t.Fatalf("GetCollectionBySlug tasks: %v", err)
		}
		if err := srv.store.SetMemberCollectionAccess(ws.ID, restricted.ID, "specific", []string{tasks.ID}); err != nil {
			t.Fatalf("SetMemberCollectionAccess: %v", err)
		}

		cases := []struct {
			name string
			user *models.User
			want map[string]int // role slug -> item_count
		}{
			// The owner is the control: the workspace-wide count, unchanged.
			{"owner", owner, map[string]int{"shown": 2, "hidden": 3}},
			{"guest", guest, map[string]int{"shown": 1, "hidden": 0}},
			{"restricted editor", restricted, map[string]int{"shown": 2, "hidden": 0}},
		}
		for _, tc := range cases {
			token, err := srv.store.CreateSession(tc.user.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
			if err != nil {
				t.Fatalf("CreateSession %s: %v", tc.name, err)
			}

			rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+slug+"/roles/board", nil, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s GET /roles/board: %d %s", tc.name, rr.Code, rr.Body.String())
			}
			var board struct {
				Lanes []struct {
					Role *models.AgentRole `json:"role"`
				} `json:"lanes"`
			}
			parseJSON(t, rr, &board)
			gotBoard := map[string]int{}
			for _, lane := range board.Lanes {
				if lane.Role != nil {
					gotBoard[lane.Role.Slug] = lane.Role.ItemCount
				}
			}

			rr = doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+slug+"/agent-roles", nil, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s GET /agent-roles: %d %s", tc.name, rr.Code, rr.Body.String())
			}
			var roles []models.AgentRole
			parseJSON(t, rr, &roles)
			gotList := map[string]int{}
			for _, role := range roles {
				gotList[role.Slug] = role.ItemCount
			}

			for slugKey, want := range tc.want {
				// Every role is listed on both doors, whatever the caller sees.
				if _, ok := gotBoard[slugKey]; !ok {
					t.Errorf("%s /roles/board: no lane for role %q", tc.name, slugKey)
				}
				if _, ok := gotList[slugKey]; !ok {
					t.Errorf("%s /agent-roles: role %q not listed", tc.name, slugKey)
				}
				if gotBoard[slugKey] != want {
					t.Errorf("%s /roles/board: role %q item_count = %d, want %d", tc.name, slugKey, gotBoard[slugKey], want)
				}
				if gotList[slugKey] != want {
					t.Errorf("%s /agent-roles: role %q item_count = %d, want %d", tc.name, slugKey, gotList[slugKey], want)
				}
			}
		}
	})
}
