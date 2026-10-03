package store

import (
	"reflect"
	"sort"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3338: a connection stored with the wildcard on and "future" off is
// narrowed to its user's current memberships; nothing else is touched.

func narrowFixtureWorkspace(t *testing.T, s *Store, userID, name, role string) *models.Workspace {
	t.Helper()
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: name, OwnerID: userID})
	if err != nil {
		t.Fatalf("CreateWorkspace %s: %v", name, err)
	}
	if err := s.AddWorkspaceMember(ws.ID, userID, role); err != nil {
		t.Fatalf("AddWorkspaceMember %s: %v", name, err)
	}
	return ws
}

func narrowFixtureConnection(t *testing.T, s *Store, requestID, userID string, allCurrent, future bool) {
	t.Helper()
	if err := s.CreateOAuthConnection(OAuthConnection{
		RequestID:               requestID,
		UserID:                  userID,
		MayCreateWorkspaces:     true,
		AllCurrentWorkspaces:    allCurrent,
		IncludeFutureWorkspaces: future,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection %s: %v", requestID, err)
	}
}

func TestNarrowCurrentOnlyWildcardConnections(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateUser(models.UserCreate{Email: "narrow3338@example.test", Name: "N", Password: "pw-3338-abcdefgh"})
	if err != nil {
		t.Fatal(err)
	}
	lonely, err := s.CreateUser(models.UserCreate{Email: "lonely3338@example.test", Name: "L", Password: "pw-3338-abcdefgh"})
	if err != nil {
		t.Fatal(err)
	}
	wsA := narrowFixtureWorkspace(t, s, u.ID, "Narrow A", "owner")
	wsB := narrowFixtureWorkspace(t, s, u.ID, "Narrow B", "viewer")
	gone := narrowFixtureWorkspace(t, s, u.ID, "Narrow Gone", "owner")
	if err := s.DeleteWorkspace(gone.Slug); err != nil {
		t.Fatal(err)
	}

	// A workspace the user reaches only as a guest, through an item grant.
	other, err := s.CreateUser(models.UserCreate{Email: "other3338n@example.test", Name: "O", Password: "pw-3338-abcdefgh"})
	if err != nil {
		t.Fatal(err)
	}
	wsG := narrowFixtureWorkspace(t, s, other.ID, "Narrow Guest", "owner")
	coll, err := s.CreateCollection(wsG.ID, models.CollectionCreate{Name: "Tasks"})
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.CreateItem(wsG.ID, coll.ID, models.ItemCreate{Title: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateItemGrant(wsG.ID, it.ID, u.ID, "view", other.ID); err != nil {
		t.Fatal(err)
	}

	narrowFixtureConnection(t, s, "split", u.ID, true, false)       // the promise the checkbox broke
	narrowFixtureConnection(t, s, "wildcard", u.ID, true, true)     // a real wildcard: untouched
	narrowFixtureConnection(t, s, "specific", u.ID, false, false)   // already a list: untouched
	narrowFixtureConnection(t, s, "lonely", lonely.ID, true, false) // no memberships
	if err := s.AddConnectionWorkspace("specific", wsA.ID, AddedByUser); err != nil {
		t.Fatal(err)
	}

	res, err := s.NarrowCurrentOnlyWildcardConnections()
	if err != nil {
		t.Fatalf("narrow: %v", err)
	}
	if want := (NarrowCurrentOnlyResult{Connections: 2, WorkspacesAdded: 3, Emptied: 1}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}

	access := func(rid string) OAuthConnectionAccess {
		t.Helper()
		a, err := s.GetOAuthConnectionAccess(rid)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	want := []string{wsA.Slug, wsB.Slug, wsG.Slug}
	sort.Strings(want)
	if a := access("split"); a.AllCurrentWorkspaces || !reflect.DeepEqual(a.WorkspaceSlugs, want) {
		t.Errorf("split: %+v, want specific %v", a, want)
	}
	if a := access("wildcard"); !a.AllCurrentWorkspaces {
		t.Errorf("a real wildcard was narrowed: %+v", a)
	}
	if a := access("specific"); a.AllCurrentWorkspaces || !reflect.DeepEqual(a.WorkspaceSlugs, []string{wsA.Slug}) {
		t.Errorf("an existing list changed: %+v", a)
	}
	if a := access("lonely"); a.AllCurrentWorkspaces || len(a.WorkspaceSlugs) != 0 {
		t.Errorf("lonely: %+v, want an empty specific list", a)
	}

	// A workspace joined afterwards is not reached.
	narrowFixtureWorkspace(t, s, u.ID, "Narrow Later", "owner")
	if a := access("split"); !reflect.DeepEqual(a.WorkspaceSlugs, want) {
		t.Errorf("a workspace joined after narrowing is reached: %v", a.WorkspaceSlugs)
	}

	// Idempotent.
	again, err := s.NarrowCurrentOnlyWildcardConnections()
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again != (NarrowCurrentOnlyResult{}) {
		t.Errorf("second run changed something: %+v", again)
	}
}
