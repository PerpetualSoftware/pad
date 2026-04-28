package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Phase 2 permission test suite (TASK-488).
// Tests collection-level visibility, system collection exemptions,
// and the VisibleCollectionIDs resolution logic.

func setupPermissionTest(t *testing.T) (*Store, *models.Workspace, *models.User, *models.User) {
	t.Helper()
	s := testStore(t)

	// Create owner and member users
	owner := createTestUser(t, s, "owner@test.com", "Owner", "password123")
	member := createTestUser(t, s, "member@test.com", "Member", "password123")

	// Create workspace with owner
	ws := createTestWorkspace(t, s, "Test Workspace")
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	if err := s.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	// Create collections: tasks (regular), conventions (system), ideas (regular)
	_, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", IsDefault: true,
	})
	if err != nil {
		t.Fatalf("create tasks: %v", err)
	}

	_, err = s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Conventions", Slug: "conventions", IsDefault: true, IsSystem: true,
	})
	if err != nil {
		t.Fatalf("create conventions: %v", err)
	}

	_, err = s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Ideas", Slug: "ideas", IsDefault: true,
	})
	if err != nil {
		t.Fatalf("create ideas: %v", err)
	}

	return s, ws, owner, member
}

func TestVisibleCollectionIDs_AllAccess(t *testing.T) {
	s, ws, _, member := setupPermissionTest(t)

	// Default: member has "all" access
	ids, err := s.VisibleCollectionIDs(ws.ID, member.ID)
	if err != nil {
		t.Fatalf("VisibleCollectionIDs: %v", err)
	}
	if ids != nil {
		t.Errorf("expected nil (all access), got %v", ids)
	}
}

func TestVisibleCollectionIDs_SpecificAccess(t *testing.T) {
	s, ws, _, member := setupPermissionTest(t)

	// Get collection IDs
	colls, _ := s.ListCollections(ws.ID)
	var tasksID, ideasID string
	for _, c := range colls {
		switch c.Slug {
		case "tasks":
			tasksID = c.ID
		case "ideas":
			ideasID = c.ID
		}
	}

	// Set member to specific access with only "tasks"
	err := s.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{tasksID})
	if err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	ids, err := s.VisibleCollectionIDs(ws.ID, member.ID)
	if err != nil {
		t.Fatalf("VisibleCollectionIDs: %v", err)
	}
	if ids == nil {
		t.Fatal("expected non-nil IDs for specific access")
	}

	// Should include tasks (granted) and conventions (system), but NOT ideas
	idSet := make(map[string]bool)
	for _, id := range ids {
		idSet[id] = true
	}

	if !idSet[tasksID] {
		t.Error("expected tasks collection to be visible")
	}
	if idSet[ideasID] {
		t.Error("expected ideas collection to NOT be visible")
	}
}

func TestVisibleCollectionIDs_SystemCollectionsAlwaysVisible(t *testing.T) {
	s, ws, _, member := setupPermissionTest(t)

	colls, _ := s.ListCollections(ws.ID)
	var tasksID, conventionsID string
	for _, c := range colls {
		switch c.Slug {
		case "tasks":
			tasksID = c.ID
		case "conventions":
			conventionsID = c.ID
		}
	}

	// Set member to specific access with only "tasks"
	_ = s.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{tasksID})

	ids, err := s.VisibleCollectionIDs(ws.ID, member.ID)
	if err != nil {
		t.Fatalf("VisibleCollectionIDs: %v", err)
	}

	idSet := make(map[string]bool)
	for _, id := range ids {
		idSet[id] = true
	}

	// Conventions is system → always visible even though not explicitly granted
	if !idSet[conventionsID] {
		t.Error("expected conventions (system) collection to always be visible")
	}
}

func TestVisibleCollectionIDs_NonMember(t *testing.T) {
	s, ws, _, _ := setupPermissionTest(t)

	// Create a user who is NOT a member
	outsider := createTestUser(t, s, "outsider@test.com", "Outsider", "password123")

	ids, err := s.VisibleCollectionIDs(ws.ID, outsider.ID)
	if err != nil {
		t.Fatalf("VisibleCollectionIDs: %v", err)
	}
	// Non-member should get empty list (not nil!)
	if ids == nil {
		t.Error("expected empty list for non-member, got nil (all access)")
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 visible collections for non-member, got %d", len(ids))
	}
}

func TestSetMemberCollectionAccess_ReplaceGrants(t *testing.T) {
	s, ws, _, member := setupPermissionTest(t)

	colls, _ := s.ListCollections(ws.ID)
	var tasksID, ideasID string
	for _, c := range colls {
		switch c.Slug {
		case "tasks":
			tasksID = c.ID
		case "ideas":
			ideasID = c.ID
		}
	}

	// Set to tasks only
	_ = s.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{tasksID})

	grants, _ := s.GetMemberCollectionAccess(ws.ID, member.ID)
	if len(grants) != 1 || grants[0] != tasksID {
		t.Errorf("expected [tasks], got %v", grants)
	}

	// Replace with ideas only
	_ = s.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{ideasID})

	grants, _ = s.GetMemberCollectionAccess(ws.ID, member.ID)
	if len(grants) != 1 || grants[0] != ideasID {
		t.Errorf("expected [ideas], got %v", grants)
	}
}

func TestSetMemberCollectionAccess_BackToAll(t *testing.T) {
	s, ws, _, member := setupPermissionTest(t)

	colls, _ := s.ListCollections(ws.ID)
	var tasksID string
	for _, c := range colls {
		if c.Slug == "tasks" {
			tasksID = c.ID
		}
	}

	// Set to specific
	_ = s.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{tasksID})

	// Verify specific
	m, _ := s.GetWorkspaceMember(ws.ID, member.ID)
	if m.CollectionAccess != "specific" {
		t.Errorf("expected 'specific', got %q", m.CollectionAccess)
	}

	// Set back to all
	_ = s.SetMemberCollectionAccess(ws.ID, member.ID, "all", nil)

	m, _ = s.GetWorkspaceMember(ws.ID, member.ID)
	if m.CollectionAccess != "all" {
		t.Errorf("expected 'all', got %q", m.CollectionAccess)
	}

	// Grants should be cleared
	grants, _ := s.GetMemberCollectionAccess(ws.ID, member.ID)
	if len(grants) != 0 {
		t.Errorf("expected 0 grants after switching to 'all', got %d", len(grants))
	}

	// VisibleCollectionIDs should return nil (all access)
	ids, _ := s.VisibleCollectionIDs(ws.ID, member.ID)
	if ids != nil {
		t.Error("expected nil (all access) after switching back to 'all'")
	}
}

func TestListItems_FilteredByCollectionIDs(t *testing.T) {
	s, ws, _, _ := setupPermissionTest(t)

	colls, _ := s.ListCollections(ws.ID)
	var tasksID, ideasID string
	for _, c := range colls {
		switch c.Slug {
		case "tasks":
			tasksID = c.ID
		case "ideas":
			ideasID = c.ID
		}
	}

	// Create items in different collections
	_, _ = s.CreateItem(ws.ID, tasksID, models.ItemCreate{Title: "Task 1"})
	_, _ = s.CreateItem(ws.ID, tasksID, models.ItemCreate{Title: "Task 2"})
	_, _ = s.CreateItem(ws.ID, ideasID, models.ItemCreate{Title: "Idea 1"})

	// No filter: all items
	all, _ := s.ListItems(ws.ID, models.ItemListParams{})
	if len(all) != 3 {
		t.Errorf("expected 3 items with no filter, got %d", len(all))
	}

	// Filter to tasks only
	tasksOnly, _ := s.ListItems(ws.ID, models.ItemListParams{
		CollectionIDs: []string{tasksID},
	})
	if len(tasksOnly) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasksOnly))
	}

	// Filter to ideas only
	ideasOnly, _ := s.ListItems(ws.ID, models.ItemListParams{
		CollectionIDs: []string{ideasID},
	})
	if len(ideasOnly) != 1 {
		t.Errorf("expected 1 idea, got %d", len(ideasOnly))
	}

	// Filter to empty set
	empty, _ := s.ListItems(ws.ID, models.ItemListParams{
		CollectionIDs: []string{"nonexistent-id"},
	})
	if len(empty) != 0 {
		t.Errorf("expected 0 items for nonexistent collection, got %d", len(empty))
	}
}

func TestCollectionAccess_DefaultIsAll(t *testing.T) {
	s, ws, _, member := setupPermissionTest(t)

	m, err := s.GetWorkspaceMember(ws.ID, member.ID)
	if err != nil {
		t.Fatalf("GetWorkspaceMember: %v", err)
	}
	if m.CollectionAccess != "all" {
		t.Errorf("D7: default collection_access should be 'all', got %q", m.CollectionAccess)
	}
}

func TestIsSystemCollection(t *testing.T) {
	s, ws, _, _ := setupPermissionTest(t)

	colls, _ := s.ListCollections(ws.ID)
	for _, c := range colls {
		switch c.Slug {
		case "conventions":
			if !c.IsSystem {
				t.Error("conventions should be a system collection")
			}
		case "tasks":
			if c.IsSystem {
				t.Error("tasks should NOT be a system collection")
			}
		case "ideas":
			if c.IsSystem {
				t.Error("ideas should NOT be a system collection")
			}
		}
	}
}
