package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestRoleArrival_CreateAppendsBelowLine(t *testing.T) {
	t.Parallel()
	s, ws, col, role, user, items := seedRoleParkFixture(t)
	// Existing items have role_sort_order 1024, 2048, 3072.

	// Unparked: new item joins bottom at 3072 + 1024 = 4096.
	created1, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Arrival Unparked", Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem unparked: %v", err)
	}
	if created1.RoleSortOrder != 4096 {
		t.Fatalf("unparked arrival role_sort_order = %d, want 4096", created1.RoleSortOrder)
	}

	// Park After(created1): P = 4096 + 1024 = 5120 (P > laneMax=4096).
	park, err := s.SetRolePark(ws.ID, role.ID, ParkAfter(created1.ID), nil, user.ID)
	if err != nil {
		t.Fatalf("SetRolePark After(last): %v", err)
	}
	if park.SortOrder == nil || *park.SortOrder != 5120 {
		t.Fatalf("park = %+v, want line @ 5120", park)
	}

	// New item must land at max(4096, 5120) + 1024 = 6144 (below the park line!).
	created2, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Arrival Below Line", Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem below line: %v", err)
	}
	if created2.RoleSortOrder != 6144 {
		t.Fatalf("parked arrival role_sort_order = %d, want 6144", created2.RoleSortOrder)
	}
	if !ItemParked(park, created2.RoleSortOrder) {
		t.Fatalf("new arrival (sort=%d) should be parked under line P=%d", created2.RoleSortOrder, *park.SortOrder)
	}

	// Also verify existing items were not disturbed.
	for i, want := range []int{1024, 2048, 3072} {
		got, err := s.GetItem(items[i].ID)
		if err != nil {
			t.Fatalf("GetItem %d: %v", i, err)
		}
		if got.RoleSortOrder != want {
			t.Errorf("existing item %d moved to %d, want %d", i, got.RoleSortOrder, want)
		}
	}
}

func TestRoleArrival_ReassignAppendsBelowLine(t *testing.T) {
	t.Parallel()
	s, ws, col, role, user, items := seedRoleParkFixture(t)
	otherRole, err := s.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Reviewer"})
	if err != nil {
		t.Fatalf("CreateAgentRole other: %v", err)
	}

	// Park After(items[2] @ 3072) -> P = 4096.
	park, err := s.SetRolePark(ws.ID, role.ID, ParkAfter(items[2].ID), nil, user.ID)
	if err != nil {
		t.Fatalf("SetRolePark: %v", err)
	}

	// 1. Reassign from no-role -> role: lands at max(3072, 4096) + 1024 = 5120.
	unassigned, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Was Unassigned", Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem unassigned: %v", err)
	}
	reassigned1, err := s.UpdateItem(unassigned.ID, models.ItemUpdate{AgentRoleID: &role.ID})
	if err != nil {
		t.Fatalf("UpdateItem assign role: %v", err)
	}
	if reassigned1.RoleSortOrder != 5120 {
		t.Fatalf("unassigned->role role_sort_order = %d, want 5120", reassigned1.RoleSortOrder)
	}
	if !ItemParked(park, reassigned1.RoleSortOrder) {
		t.Errorf("reassigned item should be parked below P=%d", *park.SortOrder)
	}

	// 2. Reassign from otherRole -> role: lands at max(5120, 4096) + 1024 = 6144.
	fromOther, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "From Reviewer", Fields: `{"status":"open"}`, AgentRoleID: &otherRole.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem otherRole: %v", err)
	}
	reassigned2, err := s.UpdateItem(fromOther.ID, models.ItemUpdate{AgentRoleID: &role.ID})
	if err != nil {
		t.Fatalf("UpdateItem role change: %v", err)
	}
	if reassigned2.RoleSortOrder != 6144 {
		t.Fatalf("otherRole->role role_sort_order = %d, want 6144", reassigned2.RoleSortOrder)
	}

	// 3. Updating an item while keeping the SAME role must NOT move its role_sort_order.
	newTitle := "Alpha Edited"
	sameRoleUpd, err := s.UpdateItem(items[0].ID, models.ItemUpdate{
		Title:       &newTitle,
		AgentRoleID: &role.ID,
	})
	if err != nil {
		t.Fatalf("UpdateItem same role: %v", err)
	}
	if sameRoleUpd.RoleSortOrder != 1024 {
		t.Fatalf("same-role update moved role_sort_order to %d, want 1024", sameRoleUpd.RoleSortOrder)
	}
}

func TestRoleArrival_NoRoleUnchanged(t *testing.T) {
	t.Parallel()
	s, ws, col, role, _, items := seedRoleParkFixture(t)

	// Create without role -> role_sort_order stays 0.
	noRole, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "No Role", Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem no role: %v", err)
	}
	if noRole.RoleSortOrder != 0 {
		t.Fatalf("no-role create role_sort_order = %d, want 0", noRole.RoleSortOrder)
	}

	// Update without setting a role -> role_sort_order unchanged.
	newTitle := "No Role Edited"
	upd, err := s.UpdateItem(noRole.ID, models.ItemUpdate{Title: &newTitle})
	if err != nil {
		t.Fatalf("UpdateItem no role: %v", err)
	}
	if upd.RoleSortOrder != 0 {
		t.Fatalf("no-role update role_sort_order = %d, want 0", upd.RoleSortOrder)
	}

	// Clearing a role leaves role_sort_order unchanged (not appended to any lane).
	cleared, err := s.UpdateItem(items[1].ID, models.ItemUpdate{ClearAgentRole: true})
	if err != nil {
		t.Fatalf("UpdateItem clear role: %v", err)
	}
	if cleared.AgentRoleID != nil {
		t.Fatalf("expected nil AgentRoleID after clear, got %v", *cleared.AgentRoleID)
	}
	if cleared.RoleSortOrder != 2048 {
		t.Fatalf("clear-role changed role_sort_order to %d, want 2048", cleared.RoleSortOrder)
	}
	_ = role
}

func TestRoleArrival_RespacesNearLimit(t *testing.T) {
	t.Parallel()
	s, ws, col, role, user, items := seedRoleParkFixture(t)

	// Position items near 2^30 and place a park line before items[1].
	nearLimit := (1 << 30) - 512
	if err := s.UpdateRoleSortOrder(ws.ID, []RoleSortUpdate{
		{ItemID: items[0].ID, RoleSortOrder: nearLimit - 2048},
		{ItemID: items[1].ID, RoleSortOrder: nearLimit - 1024},
		{ItemID: items[2].ID, RoleSortOrder: nearLimit},
	}); err != nil {
		t.Fatalf("UpdateRoleSortOrder near limit: %v", err)
	}
	if _, err := s.SetRolePark(ws.ID, role.ID, ParkBefore(items[1].ID), nil, user.ID); err != nil {
		t.Fatalf("SetRolePark near limit: %v", err)
	}

	// Creating a new item with this role would exceed 2^30, so the lane MUST
	// respace first (1024, 2048, 3072), remap P to 2048, and place the new item at 4096.
	arrived, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Near Limit Arrival", Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem near limit: %v", err)
	}
	if arrived.RoleSortOrder != 4096 {
		t.Fatalf("arrived role_sort_order = %d, want 4096", arrived.RoleSortOrder)
	}

	wantOrders := []int{1024, 2048, 3072}
	for i, it := range items {
		cur, err := s.GetItem(it.ID)
		if err != nil {
			t.Fatalf("GetItem %d: %v", i, err)
		}
		if cur.RoleSortOrder != wantOrders[i] {
			t.Errorf("respaced item %d role_sort_order = %d, want %d", i, cur.RoleSortOrder, wantOrders[i])
		}
	}

	// Verify park line was remapped cleanly: items[0] still above, items[1..2] and arrived still below.
	gotRole, err := s.GetAgentRole(ws.ID, role.ID)
	if err != nil || gotRole.Park == nil || gotRole.Park.SortOrder == nil {
		t.Fatalf("GetAgentRole after respace: %+v err=%v", gotRole, err)
	}
	if *gotRole.Park.SortOrder != 2048 {
		t.Fatalf("remapped park_sort_order = %d, want 2048", *gotRole.Park.SortOrder)
	}
	if ItemParked(gotRole.Park, 1024) {
		t.Error("items[0] should remain above park line after respace")
	}
	if !ItemParked(gotRole.Park, 2048) || !ItemParked(gotRole.Park, arrived.RoleSortOrder) {
		t.Error("items[1] and arrived item should be parked after respace")
	}
}
