package store

import (
	"sync"
	"testing"
)

// Tests for the per-user tabs revision (BUG-3285). Like the other tab tests
// they run on whichever dialect testStore gives.

func tabsRevision(t *testing.T, s *Store, userID string) int64 {
	t.Helper()
	list, err := s.ListWorkspaceTabs(userID)
	if err != nil {
		t.Fatalf("list tabs: %v", err)
	}
	return list.Revision
}

func listHas(list WorkspaceTabList, wsID string) bool {
	for _, r := range list.Rows {
		if r.WorkspaceID == wsID {
			return true
		}
	}
	return false
}

// The bug's measured instance, as the store sees it: a close of C and a
// route write to A cross, and the server processes the route write FIRST.
// Its answer still holds C; the close's answer does not. The revision is what
// tells the client which of the two to keep: the later-PROCESSED one is
// higher, whatever order they were sent or answered in.
func TestWorkspaceTabs_RevisionOrdersWritesByProcessing(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := tabsUser(t, s, "Revorder")
	a := tabsWorkspace(t, s, u, "RA")
	c := tabsWorkspace(t, s, u, "RC")
	for _, id := range []string{a.ID, c.ID} {
		if _, err := s.OpenWorkspaceTab(u.ID, id, false); err != nil {
			t.Fatal(err)
		}
	}

	route := "/revorder/ra/tasks"
	patched, err := s.UpdateWorkspaceTab(u.ID, a.ID, WorkspaceTabUpdate{LastRoute: &route})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := s.CloseWorkspaceTab(u.ID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !listHas(patched, c.ID) || listHas(closed, c.ID) {
		t.Fatalf("precondition: the earlier-processed answer holds C and the later one does not: %+v / %+v", patched, closed)
	}
	if closed.Revision <= patched.Revision {
		t.Fatalf("the later-processed write answered revision %d, not above the earlier one's %d", closed.Revision, patched.Revision)
	}
	if got := tabsRevision(t, s, u.ID); got != closed.Revision {
		t.Fatalf("a read after both answered revision %d; want the last write's %d", got, closed.Revision)
	}
}

// Every write bumps, including one that changes nothing, so its answer is
// ordered after every write processed before it. A read does not bump.
func TestWorkspaceTabs_EveryWriteBumpsTheRevision(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := tabsUser(t, s, "Revbump")
	a := tabsWorkspace(t, s, u, "BA")
	b := tabsWorkspace(t, s, u, "BB")

	last := tabsRevision(t, s, u.ID)
	if again := tabsRevision(t, s, u.ID); again != last {
		t.Fatalf("a read moved the revision: %d then %d", last, again)
	}
	route := "/revbump/ba"
	steps := []struct {
		name string
		do   func() (WorkspaceTabList, error)
	}{
		{"open", func() (WorkspaceTabList, error) { return s.OpenWorkspaceTab(u.ID, a.ID, false) }},
		{"open ephemeral", func() (WorkspaceTabList, error) { return s.OpenWorkspaceTab(u.ID, b.ID, true) }},
		{"ephemeral open of an open tab (no change)", func() (WorkspaceTabList, error) { return s.OpenWorkspaceTab(u.ID, a.ID, true) }},
		{"reorder", func() (WorkspaceTabList, error) { return s.ReorderWorkspaceTabs(u.ID, []string{b.ID, a.ID}) }},
		{"update", func() (WorkspaceTabList, error) {
			return s.UpdateWorkspaceTab(u.ID, a.ID, WorkspaceTabUpdate{LastRoute: &route})
		}},
		{"pin", func() (WorkspaceTabList, error) {
			return s.UpdateWorkspaceTab(u.ID, b.ID, WorkspaceTabUpdate{Pin: true})
		}},
		{"close", func() (WorkspaceTabList, error) { return s.CloseWorkspaceTab(u.ID, b.ID) }},
		{"close of a closed tab (no change)", func() (WorkspaceTabList, error) { return s.CloseWorkspaceTab(u.ID, b.ID) }},
	}
	for _, step := range steps {
		list, err := step.do()
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if list.Revision != last+1 {
			t.Fatalf("%s: revision %d, want %d", step.name, list.Revision, last+1)
		}
		last = list.Revision
	}

	// A refused write (no such tab) rolls back and does not bump.
	if _, err := s.UpdateWorkspaceTab(u.ID, b.ID, WorkspaceTabUpdate{Pin: true}); err == nil {
		t.Fatal("update of a closed tab succeeded")
	}
	if got := tabsRevision(t, s, u.ID); got != last {
		t.Fatalf("a refused update moved the revision to %d from %d", got, last)
	}
}

// A write's answer is the set AS THAT WRITE LEFT IT: its rows and its
// revision are one instant. Concurrent durable opens of distinct workspaces
// each add exactly one row and bump exactly once, so every answer must hold
// exactly (revision - base) rows. An answer read after the commit, or rows
// and revision read in two statements, can pair one write's revision with a
// later write's rows.
func TestWorkspaceTabs_WriteAnswerIsOneInstant(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := tabsUser(t, s, "Revinstant")
	const n = 12
	ids := make([]string, n)
	for i := range ids {
		ids[i] = tabsWorkspace(t, s, u, "I"+string(rune('a'+i))).ID
	}
	if _, err := s.db.Exec(s.q(`DELETE FROM user_workspace_tabs WHERE user_id = ?`), u.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	base := tabsRevision(t, s, u.ID)

	var wg sync.WaitGroup
	start := make(chan struct{})
	lists := make(chan WorkspaceTabList, n)
	errs := make(chan error, n)
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			list, err := s.OpenWorkspaceTab(u.ID, id, false)
			if err != nil {
				errs <- err
				return
			}
			lists <- list
		}(id)
	}
	close(start)
	wg.Wait()
	close(errs)
	close(lists)
	for err := range errs {
		t.Fatalf("concurrent open: %v", err)
	}
	seen := map[int64]bool{}
	for list := range lists {
		if want := int(list.Revision - base); len(list.Rows) != want {
			t.Fatalf("answer at revision %d holds %d rows; want %d", list.Revision, len(list.Rows), want)
		}
		if seen[list.Revision] {
			t.Fatalf("two writes answered revision %d", list.Revision)
		}
		seen[list.Revision] = true
	}
	if got := tabsRevision(t, s, u.ID); got != base+n {
		t.Fatalf("final revision %d, want %d", got, base+n)
	}
}

// The loss paths that delete a row bump the revision of the user who lost
// it, and nobody else's; a prune that deletes nothing bumps nothing.
func TestWorkspaceTabs_LossPathsBumpTheRevision(t *testing.T) {
	t.Parallel()

	t.Run("member removal", func(t *testing.T) {
		t.Parallel()
		s := testStore(t)
		owner := tabsUser(t, s, "Lbowner")
		member := tabsUser(t, s, "Lbmember")
		bystander := tabsUser(t, s, "Lbbystander")
		ws := tabsWorkspace(t, s, owner, "LB")
		if err := s.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
			t.Fatal(err)
		}
		for _, uid := range []string{owner.ID, member.ID} {
			if _, err := s.OpenWorkspaceTab(uid, ws.ID, false); err != nil {
				t.Fatal(err)
			}
		}
		ownerRev, memberRev, byRev := tabsRevision(t, s, owner.ID), tabsRevision(t, s, member.ID), tabsRevision(t, s, bystander.ID)
		if err := s.RemoveWorkspaceMember(ws.ID, member.ID); err != nil {
			t.Fatal(err)
		}
		if got := tabsRevision(t, s, member.ID); got != memberRev+1 {
			t.Fatalf("removed member's revision %d, want %d", got, memberRev+1)
		}
		if got := tabsRevision(t, s, owner.ID); got != ownerRev {
			t.Fatalf("owner's revision moved: %d -> %d", ownerRev, got)
		}
		if got := tabsRevision(t, s, bystander.ID); got != byRev {
			t.Fatalf("bystander's revision moved: %d -> %d", byRev, got)
		}
	})

	t.Run("member removal with no tab does not bump", func(t *testing.T) {
		t.Parallel()
		s := testStore(t)
		owner := tabsUser(t, s, "Lnowner")
		member := tabsUser(t, s, "Lnmember")
		ws := tabsWorkspace(t, s, owner, "LN")
		if err := s.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(s.q(`DELETE FROM user_workspace_tabs WHERE user_id = ?`), member.ID); err != nil {
			t.Fatal(err)
		}
		before := tabsRevision(t, s, member.ID)
		if err := s.RemoveWorkspaceMember(ws.ID, member.ID); err != nil {
			t.Fatal(err)
		}
		if got := tabsRevision(t, s, member.ID); got != before {
			t.Fatalf("a prune that deleted nothing moved the revision: %d -> %d", before, got)
		}
	})

	t.Run("account deletion bumps the other holders of the owner's workspaces", func(t *testing.T) {
		t.Parallel()
		s := testStore(t)
		owner := tabsUser(t, s, "Laowner")
		member := tabsUser(t, s, "Lamember")
		ws := tabsWorkspace(t, s, owner, "LA")
		if err := s.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.OpenWorkspaceTab(member.ID, ws.ID, false); err != nil {
			t.Fatal(err)
		}
		before := tabsRevision(t, s, member.ID)
		if err := s.DeleteAccountAtomic(owner.ID); err != nil {
			t.Fatal(err)
		}
		list, err := s.ListWorkspaceTabs(member.ID)
		if err != nil {
			t.Fatal(err)
		}
		if list.Revision != before+1 || listHas(list, ws.ID) {
			t.Fatalf("member after the owner's account deletion: %+v (revision was %d)", list, before)
		}
	})

	t.Run("soft delete bumps every holder", func(t *testing.T) {
		t.Parallel()
		s := testStore(t)
		owner := tabsUser(t, s, "Lsowner")
		member := tabsUser(t, s, "Lsmember")
		ws := tabsWorkspace(t, s, owner, "LS")
		other := tabsWorkspace(t, s, member, "LSother")
		if err := s.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
			t.Fatal(err)
		}
		for _, uid := range []string{owner.ID, member.ID} {
			if _, err := s.OpenWorkspaceTab(uid, ws.ID, false); err != nil {
				t.Fatal(err)
			}
		}
		// The member also holds a tab elsewhere; only the lost one goes.
		if _, err := s.OpenWorkspaceTab(member.ID, other.ID, false); err != nil {
			t.Fatal(err)
		}
		ownerRev, memberRev := tabsRevision(t, s, owner.ID), tabsRevision(t, s, member.ID)
		if err := s.DeleteWorkspace(ws.Slug); err != nil {
			t.Fatal(err)
		}
		if got := tabsRevision(t, s, owner.ID); got != ownerRev+1 {
			t.Fatalf("owner's revision %d, want %d", got, ownerRev+1)
		}
		list, err := s.ListWorkspaceTabs(member.ID)
		if err != nil {
			t.Fatal(err)
		}
		if list.Revision != memberRev+1 || listHas(list, ws.ID) || !listHas(list, other.ID) {
			t.Fatalf("member after soft delete: %+v (revision was %d)", list, memberRev)
		}
	})
}

// A user with no tabs still has a revision, and an unknown user reads as
// empty rather than failing: the LEFT JOIN's shapes.
func TestWorkspaceTabs_ListShapes(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := tabsUser(t, s, "Revempty")
	if _, err := s.db.Exec(s.q(`DELETE FROM user_workspace_tabs WHERE user_id = ?`), u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE users SET workspace_tabs_revision = 7 WHERE id = ?`), u.ID); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListWorkspaceTabs(u.ID)
	if err != nil || list.Revision != 7 || len(list.Rows) != 0 {
		t.Fatalf("no-tabs user: %+v %v", list, err)
	}
	list, err = s.ListWorkspaceTabs("no-such-user")
	if err != nil || list.Revision != 0 || len(list.Rows) != 0 {
		t.Fatalf("unknown user: %+v %v", list, err)
	}
}
