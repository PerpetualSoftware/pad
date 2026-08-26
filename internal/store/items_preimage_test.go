package store

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// assertSameFields compares two `fields` blobs by VALUE, not by bytes.
// The column is TEXT on SQLite — which hands back exactly what was written —
// and JSONB on Postgres, which re-serialises it (`{"status": "done"}` with a
// space). A byte comparison therefore passes on one dialect and fails on the
// other while saying nothing about the property under test, which is WHICH
// snapshot the blob came from. Found by the Postgres leg after the SQLite
// suite was green (BUG-2776).
func assertSameFields(t *testing.T, label, got, want, why string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("%s is not JSON: %q (%v)", label, got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want value is not JSON: %q (%v)", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		if why != "" {
			t.Errorf("%s = %s, want %s — %s", label, got, want, why)
		} else {
			t.Errorf("%s = %s, want %s", label, got, want)
		}
	}
}

// BUG-2776. UpdateItemWithParentLink hands back the pre-write view it read
// under its own locks, so a caller can describe what THIS call changed rather
// than what changed since the caller last looked.
//
// These are the store half of the contract; the handler half (a concurrent
// writer's change must not reach this request's change list) lives in
// internal/server. Wiring is a claim, so both halves are tested — a pre-image
// that is correct but never consumed fixes nothing (CONVE-19).

func TestUpdateItem_PreUpdateIsTheLockedPreWriteView(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "PreImage")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, coll.ID, "Subject", "")

	first := `{"status":"open"}`
	if _, err := s.UpdateItemWithParentLink(item.ID, models.ItemUpdate{Fields: &first}, nil, nil); err != nil {
		t.Fatalf("seed update: %v", err)
	}

	// A second writer commits BEFORE our call — the sequential stand-in for
	// the concurrent case: what matters is that the pre-image is read at call
	// time under the write lock, not carried in by the caller.
	second := `{"status":"done"}`
	if _, err := s.UpdateItemWithParentLink(item.ID, models.ItemUpdate{Fields: &second}, nil, nil); err != nil {
		t.Fatalf("rival update: %v", err)
	}

	third := `{"status":"done","priority":"high"}`
	updated, err := s.UpdateItemWithParentLink(item.ID, models.ItemUpdate{Fields: &third}, nil, nil)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.PreUpdate == nil {
		t.Fatal("PreUpdate is nil on a successful update; every caller diffing against it would silently fall back to its own stale read")
	}
	assertSameFields(t, "PreUpdate.Fields", updated.PreUpdate.Fields, second,
		"the pre-image must be the row as the WRITE LOCK saw it, not an earlier snapshot")
	assertSameFields(t, "post-write fields", updated.Fields, third, "")
}

// TestUpdateItem_PreUpdateCarriesJoinedDisplayNames pins the part a narrower
// pre-image would have broken: the handler's change list compares the assigned
// user's NAME and the role's slug/name, not their ids. A pre-image read
// without the users/agent_roles joins would leave those empty and invent
// "assigned:  → Someone" on every update — a louder version of the bug this
// field exists to fix.
func TestUpdateItem_PreUpdateCarriesJoinedDisplayNames(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "PreImageJoins")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, coll.ID, "Subject", "")

	role, err := s.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Implementer", Icon: "🔨"})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, err := s.UpdateItemWithParentLink(item.ID, models.ItemUpdate{AgentRoleID: &role.ID}, nil, nil); err != nil {
		t.Fatalf("assign role: %v", err)
	}

	title := "Subject renamed"
	updated, err := s.UpdateItemWithParentLink(item.ID, models.ItemUpdate{Title: &title}, nil, nil)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if updated.PreUpdate == nil {
		t.Fatal("PreUpdate is nil")
	}
	if updated.PreUpdate.AgentRoleName != role.Name || updated.PreUpdate.AgentRoleSlug != role.Slug {
		t.Errorf("PreUpdate role = %q/%q, want %q/%q — the pre-image lost the joined display fields the change list compares",
			updated.PreUpdate.AgentRoleName, updated.PreUpdate.AgentRoleSlug, role.Name, role.Slug)
	}
	if updated.PreUpdate.Title != "Subject" {
		t.Errorf("PreUpdate.Title = %q, want the pre-rename title", updated.PreUpdate.Title)
	}
}

// TestUpdateItem_PreUpdateComesFromTheLockedReRead is what makes the locked
// re-read load-bearing rather than incidental. A rival commits inside the
// window between this call's PRE-LOCK read and its transaction; the pre-image
// must reflect the rival's committed value, because that is the row this
// transaction is about to write over.
//
// Without this test a mutation that takes the pre-image from the pre-lock
// snapshot SURVIVES the rest of the suite: every other test's rival write
// lands before the store call begins, so both reads see the same thing and
// the two implementations are indistinguishable. That is why the seam exists.
func TestUpdateItem_PreUpdateComesFromTheLockedReRead(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "PreImageLock")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, coll.ID, "Subject", "")

	seed := `{"status":"open"}`
	if _, err := s.UpdateItemWithParentLink(item.ID, models.ItemUpdate{Fields: &seed}, nil, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rival := `{"status":"done"}`
	fired := 0
	var hook func(string)
	hook = func(string) {
		if fired > 0 {
			return
		}
		fired++
		s.afterItemPreLockRead = nil
		defer func() { s.afterItemPreLockRead = hook }()
		if _, err := s.UpdateItemWithParentLink(item.ID, models.ItemUpdate{Fields: &rival}, nil, nil); err != nil {
			t.Errorf("rival update: %v", err)
		}
	}
	s.afterItemPreLockRead = hook

	mine := `{"status":"done","priority":"high"}`
	updated, err := s.UpdateItemWithParentLink(item.ID, models.ItemUpdate{Fields: &mine}, nil, nil)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	s.afterItemPreLockRead = nil

	if fired != 1 {
		t.Fatalf("seam fired %d times, want 1 — the pre-lock window was not exercised", fired)
	}
	if updated.PreUpdate == nil {
		t.Fatal("PreUpdate is nil")
	}
	assertSameFields(t, "PreUpdate.Fields", updated.PreUpdate.Fields, rival,
		"the pre-image was taken before the lock, so it predates a write this transaction is about to overwrite")
}

// TestUpdateItem_PreUpdateCarriesTheAssignedUser is the users-join half of the
// display-name contract. Without it, an implementation that dropped the users
// join while keeping the agent_roles one passes every other test here (codex
// round 3) — and would make the handler report "assigned:  → Dave" on any
// update to an already-assigned item.
func TestUpdateItem_PreUpdateCarriesTheAssignedUser(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "PreImageAssignee")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, coll.ID, "Subject", "")
	user := createTestUser(t, s, "assignee@test.com", "Assignee", "hunter2hunter2")
	if err := s.AddWorkspaceMember(ws.ID, user.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	if _, err := s.UpdateItemWithParentLink(item.ID, models.ItemUpdate{AssignedUserID: &user.ID}, nil, nil); err != nil {
		t.Fatalf("assign: %v", err)
	}

	title := "Subject renamed"
	updated, err := s.UpdateItemWithParentLink(item.ID, models.ItemUpdate{Title: &title}, nil, nil)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if updated.PreUpdate == nil {
		t.Fatal("PreUpdate is nil")
	}
	if updated.PreUpdate.AssignedUserName != user.Name {
		t.Errorf("PreUpdate.AssignedUserName = %q, want %q — a pre-image without the users join makes every update on an assigned item report a spurious assignment change",
			updated.PreUpdate.AssignedUserName, user.Name)
	}
}
