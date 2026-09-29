package store

import (
	"database/sql"
	"errors"
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
		// loses reports the one error the writer may return in a parked
		// leg, because the deletion removed something it needed first
		// (measured on Postgres). nil means it must succeed. Any other
		// error fails the leg, so a writer that failed before reaching the
		// contested rows cannot pass.
		loses func(err error) bool
		run   func(s *Store, w world) error
	}{
		{"collection grant revoke (issued by the deleting user)", accessCollGrant, nil, func(s *Store, w world) error {
			return s.DeleteCollectionGrant(w.gCollA, w.wa)
		}},
		{"item grant revoke (issued by the deleting user)", accessItemGrant, nil, func(s *Store, w world) error {
			return s.DeleteItemGrant(w.gItemA, w.wa)
		}},
		{"collection grant revoke (the deleting user is the grantee)", accessMember, userGone, func(s *Store, w world) error {
			return s.DeleteCollectionGrant(w.gCollU, w.wu)
		}},
		{"member removal from the deleting user's workspace", accessMember, nil, func(s *Store, w world) error {
			return s.RemoveWorkspaceMember(w.wa, w.u.ID)
		}},
		{"share link view of the deleting user's link", accessMember, nil, func(s *Store, w world) error {
			_, err := s.RecordShareLinkView(w.linkA, "fp-u", w.u.ID, nil)
			return err
		}},
		// Writers that insert a row referencing the deleting user through a
		// foreign key with no ON DELETE action (BUG-3289). Landing after the
		// cleanup for that table, such a row used to fail the deletion's
		// DELETE FROM users with 23503.
		{"share link view by the deleting user", accessMember, refGone, func(s *Store, w world) error {
			_, err := s.RecordShareLinkView(w.linkU, "fp-a", w.a.ID, nil)
			return err
		}},
		{"share link view by the deleting user of its own link", accessMember, refGone, func(s *Store, w world) error {
			_, err := s.RecordShareLinkView(w.linkA, "fp-a", w.a.ID, nil)
			return err
		}},
		{"session minted for the deleting user", accessMember, refGone, func(s *Store, w world) error {
			_, err := s.CreateSession(w.a.ID, "", "", "", time.Hour)
			return err
		}},
		{"password reset for the deleting user", accessMember, refGone, func(s *Store, w world) error {
			_, err := s.CreatePasswordReset(w.a.ID)
			return err
		}},
		{"api token minted for the deleting user", accessMember, refGone, func(s *Store, w world) error {
			_, err := s.CreateAPIToken(w.a.ID, models.APITokenCreate{Name: "probe"}, 30, 365)
			return err
		}},
		{"invitation sent by the deleting user", accessMember, refGone, func(s *Store, w world) error {
			_, err := s.CreateInvitation(w.wu, "invitee-"+w.a.ID[:8]+"@example.com", "viewer", w.a.ID)
			return err
		}},
		{"share link created by the deleting user", accessMember, refGone, func(s *Store, w world) error {
			_, err := s.CreateShareLink(w.wu, "item", w.iu, "view", w.a.ID, nil)
			return err
		}},
		{"collection grant issued by the deleting user", accessMember, refGone, func(s *Store, w world) error {
			_, err := s.CreateCollectionGrant(w.wu, w.cu, w.u.ID, "edit", w.a.ID)
			return err
		}},
		{"assign the deleting user's item to another user", accessMember, nil, func(s *Store, w world) error {
			uid := w.u.ID
			_, err := s.UpdateItem(w.ia, models.ItemUpdate{AssignedUserID: &uid})
			return err
		}},
		{"assign an item to the deleting user", accessMember, assigneeGone, func(s *Store, w world) error {
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

	check := func(t *testing.T, s *Store, what string, loses func(error) bool, w world, delErr, wrErr error) {
		t.Helper()
		failOnDeadlockOrError(t, what+": account deletion", delErr)
		// Not failOnDeadlockOrError: it forgives sql.ErrNoRows, which is
		// what a writer that never reached the contested rows returns.
		if wrErr != nil && (loses == nil || !loses(wrErr)) {
			t.Fatalf("%s: writer: %v", what, wrErr)
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
					// In a free race the whole deletion can also commit
					// before the writer reads its row (always, on SQLite),
					// which is an honest loss too.
					loses := func(err error) bool {
						return userGone(err) || (wr.loses != nil && wr.loses(err))
					}
					check(t, s, fmt.Sprintf("%s round %d", wr.name, round), loses, w, delErr, wrErr)
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
		// Waits at DELETE FROM users, after every cleanup statement: KEY SHARE
		// on the users row is what an inserted reference takes too, so it
		// conflicts only with the delete (BUG-3289).
		{"parked before the users delete", "DELETE FROM users WHERE id", `SELECT id FROM users WHERE id = $1 FOR KEY SHARE`, func(w world) string { return w.a.ID }},
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
					// holds. The count names the writer without its pid: this
					// database is the test's own, the holder is idle in its
					// transaction, and the deletion is the one waiter already.
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
					check(t, s, fmt.Sprintf("%s, %s", park.name, wr.name), wr.loses, w, delErr, wrErr)
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

// userGone is a writer's loss when a row it reads first (its user, its
// grant) was deleted by the account deletion. Matched on the sentinel, not on
// message text: a deadlock taking the same lock carries the same wrapper.
func userGone(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// refGone is a write referencing the deleting user (or a row it owned) after
// the deletion removed it: the foreign key refuses it, or the row it updates
// first is gone.
func refGone(err error) bool {
	msg := err.Error()
	return userGone(err) || strings.Contains(msg, "SQLSTATE 23503") || strings.Contains(msg, "FOREIGN KEY constraint failed")
}

// assigneeGone is an assignment naming a user the deletion removed: the
// store's membership check refuses it when the deletion committed first, and
// the foreign key refuses it when the deletion commits in between.
func assigneeGone(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "assigned user is not a member of this workspace") ||
		strings.Contains(msg, "SQLSTATE 23503") || strings.Contains(msg, "FOREIGN KEY constraint failed")
}

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
