package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestWorkspaceGroupsCRUDAndOwnerScope(t *testing.T) {
	s := testStore(t)
	a := tabsUser(t, s, "Groupa")
	b := tabsUser(t, s, "Groupb")
	w1 := tabsWorkspace(t, s, a, "One")
	w2 := tabsWorkspace(t, s, a, "Two")
	g, err := s.CreateWorkspaceGroup(context.Background(), a.ID, "Saved", []string{w1.ID, w1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.WorkspaceIDs) != 1 {
		t.Fatalf("duplicates: %+v", g)
	}
	name := "Renamed"
	ids := []string{w2.ID}
	for _, op := range []func() error{
		func() error { _, err := s.GetWorkspaceGroup(context.Background(), b.ID, g.ID); return err },
		func() error {
			_, err := s.UpdateWorkspaceGroup(context.Background(), b.ID, g.ID, &name, &ids, nil)
			return err
		},
		func() error { return s.DeleteWorkspaceGroup(context.Background(), b.ID, g.ID) },
	} {
		if err := op(); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("wrong owner: %v", err)
		}
	}
	got, err := s.GetWorkspaceGroup(context.Background(), a.ID, g.ID)
	if err != nil || got.Name != "Saved" || len(got.WorkspaceIDs) != 1 || got.WorkspaceIDs[0] != w1.ID {
		t.Fatalf("changed by other user: %+v %v", got, err)
	}
	got, err = s.UpdateWorkspaceGroup(context.Background(), a.ID, g.ID, &name, &ids, nil)
	if err != nil || got.Name != name || len(got.WorkspaceIDs) != 1 || got.WorkspaceIDs[0] != w2.ID {
		t.Fatalf("replace: %+v %v", got, err)
	}
	empty := []string{}
	got, err = s.UpdateWorkspaceGroup(context.Background(), a.ID, g.ID, nil, &empty, nil)
	if err != nil || got.WorkspaceIDs == nil || len(got.WorkspaceIDs) != 0 {
		t.Fatalf("clear: %+v %v", got, err)
	}
	if rows, err := s.ListWorkspaceGroups(context.Background(), b.ID); err != nil || len(rows) != 0 {
		t.Fatalf("other list: %+v %v", rows, err)
	}
	if err := s.DeleteWorkspaceGroup(context.Background(), a.ID, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWorkspaceGroup(context.Background(), a.ID, g.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted lookup: %v", err)
	}
}

func TestWorkspaceGroupsCascades(t *testing.T) {
	s := testStore(t)
	owner := tabsUser(t, s, "Cascadeowner")
	u := tabsUser(t, s, "Cascadegroup")
	ws := tabsWorkspace(t, s, owner, "Cascade workspace")
	g, err := s.CreateWorkspaceGroup(context.Background(), u.ID, "Group", []string{ws.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeWorkspaceData(ws.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWorkspaceGroup(context.Background(), u.ID, g.ID)
	if err != nil || len(got.WorkspaceIDs) != 0 {
		t.Fatalf("workspace cascade: %+v %v", got, err)
	}
	ws = tabsWorkspace(t, s, owner, "Another workspace")
	ids := []string{ws.ID}
	if _, err := s.UpdateWorkspaceGroup(context.Background(), u.ID, g.ID, nil, &ids, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(`DELETE FROM users WHERE id = ?`), u.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"user_workspace_groups", "user_workspace_group_members"} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s retains %d rows", table, count)
		}
	}
}

func TestWorkspaceGroupsUniqueNames(t *testing.T) {
	s := testStore(t)
	a := tabsUser(t, s, "Namea")
	b := tabsUser(t, s, "Nameb")
	g, err := s.CreateWorkspaceGroup(context.Background(), a.ID, "Mixed", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWorkspaceGroup(context.Background(), a.ID, "mIXED", nil); !errors.Is(err, ErrWorkspaceGroupNameTaken) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := s.CreateWorkspaceGroup(context.Background(), b.ID, "mixed", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWorkspaceGroup(context.Background(), a.ID, "Äpfel", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWorkspaceGroup(context.Background(), a.ID, "äPFEL", nil); !errors.Is(err, ErrWorkspaceGroupNameTaken) {
		t.Fatalf("Unicode duplicate: %v", err)
	}
	name := "MIXED"
	if _, err := s.UpdateWorkspaceGroup(context.Background(), a.ID, g.ID, &name, nil, nil); err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateWorkspaceGroup(context.Background(), a.ID, "Other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateWorkspaceGroup(context.Background(), a.ID, other.ID, &name, nil, nil); !errors.Is(err, ErrWorkspaceGroupNameTaken) {
		t.Fatalf("rename conflict: %v", err)
	}
	if _, err := s.db.Exec(s.q(`INSERT INTO user_workspace_groups (id,user_id,name,created_at,updated_at) VALUES (?,?,?,'2031-01-01','2031-01-01')`), "duplicate", a.ID, "mixed"); err == nil {
		t.Fatal("unique index missing")
	}
}

func TestWorkspaceGroupsConcurrentUserCap(t *testing.T) {
	s := testStore(t)
	a := tabsUser(t, s, "Capa")
	b := tabsUser(t, s, "Capb")
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 64)
	for _, u := range []string{a.ID, b.ID} {
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func(u string, i int) {
				defer wg.Done()
				<-start
				_, err := s.CreateWorkspaceGroup(context.Background(), u, fmt.Sprintf("Group %d", i), nil)
				errs <- err
			}(u, i)
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrWorkspaceGroupLimit) {
			t.Errorf("create: %v", err)
		}
	}
	if success != 40 {
		t.Fatalf("successes = %d", success)
	}
	for _, u := range []string{a.ID, b.ID} {
		rows, err := s.ListWorkspaceGroups(context.Background(), u)
		if err != nil || len(rows) != 20 {
			t.Fatalf("cap: %d %v", len(rows), err)
		}
	}
}

// The members to keep are read inside the write transaction, from the set of
// workspaces the caller can see, so a concurrent edit cannot be undone by a
// stale snapshot taken before it.
func TestWorkspaceGroupsUpdateKeepsMembersOutsideVisible(t *testing.T) {
	s := testStore(t)
	u := tabsUser(t, s, "Keepvisible")
	w1 := tabsWorkspace(t, s, u, "Keep one")
	w2 := tabsWorkspace(t, s, u, "Keep two")
	w3 := tabsWorkspace(t, s, u, "Keep three")
	g, err := s.CreateWorkspaceGroup(context.Background(), u.ID, "Saved", []string{w1.ID, w2.ID})
	if err != nil {
		t.Fatal(err)
	}
	visible := map[string]bool{w1.ID: true, w3.ID: true}
	ids := []string{w3.ID}
	got, err := s.UpdateWorkspaceGroup(context.Background(), u.ID, g.ID, nil, &ids, visible)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, id := range got.WorkspaceIDs {
		have[id] = true
	}
	// w1 was visible and left out of the new list: gone. w2 was not visible: kept.
	if len(have) != 2 || !have[w2.ID] || !have[w3.ID] {
		t.Fatalf("members after replace: %+v", got.WorkspaceIDs)
	}
}
