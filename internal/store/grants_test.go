package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestResolveUserPermission_GrantsDoNotCrossWorkspaces pins PLAN-2391 DR-5.
//
// A guest holds grants in workspace B only. Resolving with workspace A's ID
// while passing B's item/collection ID — the shape a caller produces when it
// trusts a resource ID that was never verified to belong to the request's
// workspace — must resolve to no permission at all. Before the fix the item
// and collection grant lookups matched on the resource ID alone, so B's grant
// answered for a request scoped to A.
//
// The positive half of each assertion (same grant, correct workspace) is what
// keeps the added predicate from being a silent lockout of legitimate guests.
func TestResolveUserPermission_GrantsDoNotCrossWorkspaces(t *testing.T) {
	s := testStore(t)

	owner := createTestUser(t, s, "owner@example.com", "Owner", "password123")
	guest := createTestUser(t, s, "guest@example.com", "Guest", "password123")

	wsA, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Workspace A", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace A: %v", err)
	}
	wsB, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Workspace B", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace B: %v", err)
	}

	collB := createTestCollection(t, s, wsB.ID, "Tasks")
	itemB := createTestItem(t, s, wsB.ID, collB.ID, "Secret B Item", "")

	if _, err := s.CreateItemGrant(wsB.ID, itemB.ID, guest.ID, "edit", owner.ID); err != nil {
		t.Fatalf("create item grant: %v", err)
	}

	// Item grant, wrong workspace → denied.
	perm, err := s.ResolveUserPermission(wsA.ID, guest.ID, itemB.ID, "")
	if err != nil {
		t.Fatalf("resolve (foreign workspace, item grant): %v", err)
	}
	if perm != "" {
		t.Fatalf("item grant in workspace B resolved for a request scoped to workspace A: got %q, want \"\"", perm)
	}

	// Item grant, own workspace → still works.
	perm, err = s.ResolveUserPermission(wsB.ID, guest.ID, itemB.ID, "")
	if err != nil {
		t.Fatalf("resolve (own workspace, item grant): %v", err)
	}
	if perm != "edit" {
		t.Fatalf("item grant did not resolve in its own workspace: got %q, want \"edit\"", perm)
	}

	// Same boundary for the collection grant lookup.
	if _, err := s.CreateCollectionGrant(wsB.ID, collB.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatalf("create collection grant: %v", err)
	}

	perm, err = s.ResolveUserPermission(wsA.ID, guest.ID, "", collB.ID)
	if err != nil {
		t.Fatalf("resolve (foreign workspace, collection grant): %v", err)
	}
	if perm != "" {
		t.Fatalf("collection grant in workspace B resolved for a request scoped to workspace A: got %q, want \"\"", perm)
	}

	perm, err = s.ResolveUserPermission(wsB.ID, guest.ID, "", collB.ID)
	if err != nil {
		t.Fatalf("resolve (own workspace, collection grant): %v", err)
	}
	if perm != "view" {
		t.Fatalf("collection grant did not resolve in its own workspace: got %q, want \"view\"", perm)
	}
}
