package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// Lock-order tests for the tabs revision (BUG-3285). They only discriminate
// on Postgres (PAD_TEST_POSTGRES_URL, `make test-pg`), where the loss paths
// take users row locks and a wrong order surfaces as SQLSTATE 40P01
// "deadlock detected". SQLite serialises every writer, so there they only
// prove the paths run concurrently without error.

func failOnDeadlockOrError(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return
	}
	if strings.Contains(err.Error(), "deadlock") || strings.Contains(err.Error(), "40P01") {
		t.Fatalf("%s deadlocked: %v", what, err)
	}
	t.Fatalf("%s: %v", what, err)
}

// Two soft deletes sharing every holder, against tab writes by those holders
// on the rows being deleted, and against a member removal's prune. Each
// soft delete locks many users rows; the ordered lock is what keeps it from
// crossing another soft delete, and taking users before tab rows is what
// keeps it from crossing a tab write.
func TestWorkspaceTabs_SoftDeleteLockOrder(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	owner := tabsUser(t, s, "Loowner")
	const holders = 6
	users := make([]string, holders)
	for i := range users {
		users[i] = tabsUser(t, s, fmt.Sprintf("Loholder%d", i)).ID
	}

	for round := 0; round < 5; round++ {
		w1 := tabsWorkspace(t, s, owner, fmt.Sprintf("LO1r%d", round))
		w2 := tabsWorkspace(t, s, owner, fmt.Sprintf("LO2r%d", round))
		for _, uid := range users {
			for _, ws := range []string{w1.ID, w2.ID} {
				if err := s.AddWorkspaceMember(ws, uid, "editor"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.OpenWorkspaceTab(uid, ws, false); err != nil {
					t.Fatal(err)
				}
			}
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, 2+3*holders)
		run := func(what string, fn func() error) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := fn(); err != nil {
					errs <- fmt.Errorf("%s: %w", what, err)
				}
			}()
		}
		run("soft delete w1", func() error { return s.DeleteWorkspace(w1.Slug) })
		run("soft delete w2", func() error { return s.DeleteWorkspace(w2.Slug) })
		for i, uid := range users {
			uid := uid
			route := fmt.Sprintf("/loowner/%s", w1.Slug)
			run("route write", func() error {
				_, err := s.UpdateWorkspaceTab(uid, w1.ID, WorkspaceTabUpdate{LastRoute: &route})
				return err
			})
			run("reorder", func() error {
				_, err := s.ReorderWorkspaceTabs(uid, []string{w2.ID, w1.ID})
				return err
			})
			if i%2 == 0 {
				run("member removal", func() error { return s.RemoveWorkspaceMember(w2.ID, uid) })
			} else {
				run("close", func() error {
					_, err := s.CloseWorkspaceTab(uid, w2.ID)
					return err
				})
			}
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			failOnDeadlockOrError(t, fmt.Sprintf("round %d", round), err)
		}
		for _, uid := range users {
			list, err := s.ListWorkspaceTabs(uid)
			if err != nil {
				t.Fatal(err)
			}
			if listHas(list, w1.ID) || listHas(list, w2.ID) {
				t.Fatalf("round %d: a holder still has a tab on a soft-deleted workspace: %+v", round, list)
			}
		}
	}
}

// A member removal racing that member's own account deletion. Account
// deletion locks users(U) first and then deletes U's memberships; the
// removal deletes the membership and then, in its prune, needs users(U) to
// bump the revision. Unless the removal takes users(U) BEFORE it touches the
// membership, the two wait on each other.
func TestWorkspaceTabs_MemberRemovalVsAccountDeletion(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	owner := tabsUser(t, s, "Rdowner")
	for round := 0; round < 8; round++ {
		member := tabsUser(t, s, fmt.Sprintf("Rdmember%d", round))
		ws := tabsWorkspace(t, s, owner, fmt.Sprintf("RD%d", round))
		if err := s.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.OpenWorkspaceTab(member.ID, ws.ID, false); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, 2)
		run := func(what string, fn func() error) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := fn(); err != nil {
					errs <- fmt.Errorf("%s: %w", what, err)
				}
			}()
		}
		run("member removal", func() error { return s.RemoveWorkspaceMember(ws.ID, member.ID) })
		run("account deletion", func() error { return s.DeleteAccountAtomic(member.ID) })
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			failOnDeadlockOrError(t, fmt.Sprintf("round %d", round), err)
		}
	}
}
