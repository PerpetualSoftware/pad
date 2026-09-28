package store

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// An account deletion racing an ordinary writer that touches rows the
// deletion also writes (BUG-3288). The advisory lock in DeleteAccountAtomic
// serialises deletions against each other only; every writer here is outside
// it. One subtest per writer, so a red names the writer. Only Postgres
// discriminates (40P01); SQLite serialises every writer.
//
// The deleting user as a share-link VIEWER is not here: that race fails the
// deletion on a foreign key rather than deadlocking, and is BUG-3289.
//
// The deleting user is a. u is another user, a guest or member of a's
// workspace wa, with a tab on it; wu is a workspace u owns, where a is a
// member.
func TestDeleteAccountAtomic_ConcurrentWriters(t *testing.T) {
	t.Parallel()

	type world struct {
		a, u   *models.User
		wa, wu string
		ca, cu string
		ia, iu string // ia is authored by a
		gCollA string // collection grant on wa issued by a to u
		gItemA string // item grant on wa issued by a to u
		gCollU string // collection grant on wu issued by u to a
		linkA  string // share link on ia created by a
		linkU  string // share link on iu created by u
	}

	writers := []struct {
		name string
		// access is how u reaches wa. A revoke prunes u's tab on wa only
		// when it removes u's LAST access there, so a grant writer's u holds
		// that one grant and nothing else.
		access string
		run    func(s *Store, w world) error
	}{
		{"collection grant revoke (issued by the deleting user)", accessCollGrant, func(s *Store, w world) error {
			return s.DeleteCollectionGrant(w.gCollA, w.wa)
		}},
		{"item grant revoke (issued by the deleting user)", accessItemGrant, func(s *Store, w world) error {
			return s.DeleteItemGrant(w.gItemA, w.wa)
		}},
		{"collection grant revoke (the deleting user is the grantee)", accessMember, func(s *Store, w world) error {
			return s.DeleteCollectionGrant(w.gCollU, w.wu)
		}},
		{"member removal from the deleting user's workspace", accessMember, func(s *Store, w world) error {
			return s.RemoveWorkspaceMember(w.wa, w.u.ID)
		}},
		{"share link view of the deleting user's link", accessMember, func(s *Store, w world) error {
			_, err := s.RecordShareLinkView(w.linkA, "fp-u", w.u.ID, nil)
			return err
		}},
		{"assign the deleting user's item to another user", accessMember, func(s *Store, w world) error {
			uid := w.u.ID
			_, err := s.UpdateItem(w.ia, models.ItemUpdate{AssignedUserID: &uid})
			return err
		}},
		{"assign an item to the deleting user", accessMember, func(s *Store, w world) error {
			aid := w.a.ID
			_, err := s.UpdateItem(w.iu, models.ItemUpdate{AssignedUserID: &aid})
			return err
		}},
	}

	seed := func(t *testing.T, s *Store, tag string, access string) world {
		t.Helper()
		a := tabsUser(t, s, tag+"a")
		u := tabsUser(t, s, tag+"u")
		wa := tabsWorkspace(t, s, a, tag+"WA")
		wu := tabsWorkspace(t, s, u, tag+"WU")
		if access == accessMember {
			if err := s.AddWorkspaceMember(wa.ID, u.ID, "editor"); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.AddWorkspaceMember(wu.ID, a.ID, "editor"); err != nil {
			t.Fatal(err)
		}
		ca := createTestCollection(t, s, wa.ID, tag+"CA")
		cu := createTestCollection(t, s, wu.ID, tag+"CU")
		ia := createTestItem(t, s, wa.ID, ca.ID, tag+" item a", "")
		iu := createTestItem(t, s, wu.ID, cu.ID, tag+" item u", "")
		if _, err := s.db.Exec(s.q(`UPDATE items SET created_by_user_id = ? WHERE id = ?`), a.ID, ia.ID); err != nil {
			t.Fatal(err)
		}
		w := world{a: a, u: u, wa: wa.ID, wu: wu.ID, ca: ca.ID, cu: cu.ID, ia: ia.ID, iu: iu.ID}
		if access != accessItemGrant {
			gc, err := s.CreateCollectionGrant(wa.ID, ca.ID, u.ID, "view", a.ID)
			if err != nil {
				t.Fatal(err)
			}
			w.gCollA = gc.ID
		}
		if access != accessCollGrant {
			gi, err := s.CreateItemGrant(wa.ID, ia.ID, u.ID, "view", a.ID)
			if err != nil {
				t.Fatal(err)
			}
			w.gItemA = gi.ID
		}
		gcu, err := s.CreateCollectionGrant(wu.ID, cu.ID, a.ID, "view", u.ID)
		if err != nil {
			t.Fatal(err)
		}
		w.gCollU = gcu.ID
		la, err := s.CreateShareLink(wa.ID, "item", ia.ID, "view", a.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		w.linkA = la.ID
		lu, err := s.CreateShareLink(wu.ID, "item", iu.ID, "view", u.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		w.linkU = lu.ID
		for _, tab := range [][2]string{{u.ID, wa.ID}, {u.ID, wu.ID}, {a.ID, wa.ID}, {a.ID, wu.ID}} {
			if _, err := s.OpenWorkspaceTab(tab[0], tab[1], false); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.CreateSession(a.ID, "", "", "", time.Hour); err != nil {
			t.Fatal(err)
		}
		return w
	}

	check := func(t *testing.T, s *Store, what string, w world, delErr, wrErr error) {
		t.Helper()
		failOnDeadlockOrError(t, what+": account deletion", delErr)
		// The writer may lose to the deletion honestly (its row or its user
		// is gone); only a deadlock is this probe's defect on that side.
		if wrErr != nil && (strings.Contains(wrErr.Error(), "deadlock") || strings.Contains(wrErr.Error(), "40P01")) {
			t.Fatalf("%s: writer deadlocked: %v", what, wrErr)
		}
		if got, err := s.GetUser(w.a.ID); err == nil && got != nil {
			t.Fatalf("%s: user survived its account deletion", what)
		}
	}

	// A free race: both start together. It rarely lands the writer inside
	// the deletion's transaction, so on its own it is weak evidence; the
	// parked modes below construct those interleavings instead.
	t.Run("race", func(t *testing.T) {
		t.Parallel()
		for i, wr := range writers {
			i, wr := i, wr
			t.Run(wr.name, func(t *testing.T) {
				t.Parallel()
				s := testStore(t)
				for round := 0; round < 16; round++ {
					w := seed(t, s, fmt.Sprintf("Wr%dr%d", i, round), wr.access)
					var wg sync.WaitGroup
					start := make(chan struct{})
					var delErr, wrErr error
					wg.Add(2)
					go func() {
						defer wg.Done()
						<-start
						delErr = s.DeleteAccountAtomic(w.a.ID)
					}()
					go func() {
						defer wg.Done()
						<-start
						wrErr = wr.run(s, w)
					}()
					close(start)
					wg.Wait()
					check(t, s, fmt.Sprintf("%s round %d", wr.name, round), w, delErr, wrErr)
				}
			})
		}
	})

	// Parked: a third transaction holds a row the deletion needs next, so
	// the deletion stops mid-transaction holding everything it has written
	// so far. The writer then runs, and the holder lets go. Postgres only:
	// SQLite's BEGIN IMMEDIATE would block the deletion before its first
	// statement.
	parks := []struct {
		name   string
		needle string // the deletion's statement that waits on the holder
		hold   string // the holder's lock, bound to one id from the world
		id     func(w world) string
	}{
		// Waits in step 2, after step 1 deleted the tabs of a's workspaces.
		{"parked after tabs", "SET created_by_user_id = NULL", `SELECT id FROM items WHERE id = $1 FOR UPDATE`, func(w world) string { return w.ia }},
		// Waits in step 3, after step 2 de-identified a's rows.
		{"parked after de-identify", "DELETE FROM sessions WHERE user_id", `SELECT id FROM sessions WHERE user_id = $1 FOR UPDATE`, func(w world) string { return w.a.ID }},
	}
	for pi, park := range parks {
		pi, park := pi, park
		t.Run(park.name, func(t *testing.T) {
			t.Parallel()
			for i, wr := range writers {
				i, wr := i, wr
				t.Run(wr.name, func(t *testing.T) {
					t.Parallel()
					s := testStore(t)
					if s.dialect.Driver() != DriverPostgres {
						t.Skip("Postgres only: the park needs row locks")
					}
					w := seed(t, s, fmt.Sprintf("Pk%dw%d", pi, i), wr.access)
					holder, err := s.db.Begin()
					if err != nil {
						t.Fatal(err)
					}
					defer holder.Rollback()
					if _, err := holder.Exec(park.hold, park.id(w)); err != nil {
						t.Fatal(err)
					}
					delDone := make(chan error, 1)
					go func() { delDone <- s.DeleteAccountAtomic(w.a.ID) }()
					waitForLockWait(t, s, park.needle, delDone)

					wrDone := make(chan error, 1)
					go func() { wrDone <- wr.run(s, w) }()
					// The writer either finishes against the parked deletion,
					// or queues behind something the deletion (or the holder)
					// holds.
					var wrErr error
					wrFinished := false
					deadline := time.Now().Add(10 * time.Second)
					for !wrFinished && lockWaiters(t, s) < 2 {
						select {
						case wrErr = <-wrDone:
							wrFinished = true
						case <-time.After(20 * time.Millisecond):
						}
						if time.Now().After(deadline) {
							t.Fatal("the writer neither finished nor waited on a lock")
						}
					}
					if err := holder.Rollback(); err != nil {
						t.Fatal(err)
					}
					delErr := <-delDone
					if !wrFinished {
						wrErr = <-wrDone
					}
					check(t, s, fmt.Sprintf("%s, %s", park.name, wr.name), w, delErr, wrErr)
				})
			}
		})
	}
}

// How the other user reaches the deleting user's workspace in
// TestDeleteAccountAtomic_ConcurrentWriters.
const (
	accessMember    = "member" // plus one grant of each kind
	accessCollGrant = "collection grant"
	accessItemGrant = "item grant"
)

// lockWaiters counts this test database's backends waiting on a lock.
func lockWaiters(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM pg_stat_activity
		WHERE pid <> pg_backend_pid()
		  AND datname = current_database()
		  AND state = 'active'
		  AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
		t.Fatalf("poll pg_stat_activity: %v", err)
	}
	return n
}
