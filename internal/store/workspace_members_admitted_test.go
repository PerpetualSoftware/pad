package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The admitted-workspace list carries each workspace's owner username for
// members and for grant-only guests alike, and an empty string when the
// workspace has no owner row.
func TestListUserAdmittedWorkspaces_CarriesOwnerUsername(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	newUser := func(name string) *models.User {
		t.Helper()
		u, err := s.CreateUser(models.UserCreate{
			Email: name + "@example.com", Name: name, Username: name, Password: "pw-test-12345",
		})
		if err != nil {
			t.Fatalf("CreateUser %s: %v", name, err)
		}
		return u
	}
	owner := newUser("olive")
	guest := newUser("gus")

	owned, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Owned WS", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := s.AddWorkspaceMember(owned.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	col := createTestCollection(t, s, owned.ID, "Bugs")
	if _, err := s.CreateCollectionGrant(owned.ID, col.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatalf("CreateCollectionGrant: %v", err)
	}

	unowned := createTestWorkspace(t, s, "Unowned WS")
	if err := s.AddWorkspaceMember(unowned.ID, guest.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember unowned: %v", err)
	}

	byID := func(userID string) map[string]models.Workspace {
		t.Helper()
		wss, _, err := s.ListUserAdmittedWorkspacesAndMembershipsQ(s.db, userID)
		if err != nil {
			t.Fatalf("ListUserAdmittedWorkspacesAndMembershipsQ: %v", err)
		}
		out := make(map[string]models.Workspace, len(wss))
		for _, ws := range wss {
			out[ws.ID] = ws
		}
		return out
	}

	ownerView := byID(owner.ID)
	if got := ownerView[owned.ID].OwnerUsername; got != "olive" {
		t.Errorf("member view of owned workspace: owner_username = %q, want %q", got, "olive")
	}

	guestView := byID(guest.ID)
	g, ok := guestView[owned.ID]
	if !ok {
		t.Fatalf("guest is not admitted to the owned workspace through the collection grant")
	}
	if !g.IsGuest {
		t.Errorf("guest view of owned workspace: IsGuest = false, want true")
	}
	if g.OwnerUsername != "olive" {
		t.Errorf("guest view of owned workspace: owner_username = %q, want %q", g.OwnerUsername, "olive")
	}
	u, ok := guestView[unowned.ID]
	if !ok {
		t.Fatalf("member is not admitted to the unowned workspace")
	}
	if u.OwnerUsername != "" {
		t.Errorf("unowned workspace: owner_username = %q, want empty", u.OwnerUsername)
	}
}
