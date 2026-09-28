package store

import (
	"fmt"
	"sync"
	"testing"
)

// Two account deletions at the same moment, by users whose rows reach into
// each other (BUG-3286). Each deletion writes some rows through a predicate on
// ITS user (tabs of its owned workspaces, grants it issued, share links it
// created, items it authored) that the OTHER deletion then reaches through
// its own user's FK cascade or SET NULL. With a mirror-image pair of such
// rows the two transactions wait on each other. Only Postgres discriminates
// (40P01); SQLite serialises every writer.
//
// One subtest per ingredient, so a red names the table that carries it.
func TestDeleteAccountAtomic_ConcurrentMutualDeletions(t *testing.T) {
	t.Parallel()

	type pair struct {
		a, d   string // user ids
		wa, wd string // workspaces owned by a and d, each a member of the other's
		ca, cd string // one collection in each workspace
		ia, id string // one item in each
	}

	ingredients := []struct {
		name string
		seed func(t *testing.T, s *Store, p pair)
	}{
		{"tabs", func(t *testing.T, s *Store, p pair) {
			for _, tab := range [][2]string{{p.a, p.wa}, {p.a, p.wd}, {p.d, p.wa}, {p.d, p.wd}} {
				if _, err := s.OpenWorkspaceTab(tab[0], tab[1], false); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"collection grants", func(t *testing.T, s *Store, p pair) {
			if _, err := s.CreateCollectionGrant(p.wa, p.ca, p.d, "view", p.a); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateCollectionGrant(p.wd, p.cd, p.a, "view", p.d); err != nil {
				t.Fatal(err)
			}
		}},
		{"item grants", func(t *testing.T, s *Store, p pair) {
			if _, err := s.CreateItemGrant(p.wa, p.ia, p.d, "view", p.a); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateItemGrant(p.wd, p.id, p.a, "view", p.d); err != nil {
				t.Fatal(err)
			}
		}},
		{"share link views", func(t *testing.T, s *Store, p pair) {
			la, err := s.CreateShareLink(p.wa, "item", p.ia, "view", p.a, nil)
			if err != nil {
				t.Fatal(err)
			}
			ld, err := s.CreateShareLink(p.wd, "item", p.id, "view", p.d, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.RecordShareLinkView(la.ID, "fp-d", p.d, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RecordShareLinkView(ld.ID, "fp-a", p.a, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"authored and assigned items", func(t *testing.T, s *Store, p pair) {
			for _, it := range [][3]string{{p.ia, p.a, p.d}, {p.id, p.d, p.a}} {
				if _, err := s.db.Exec(s.q(`UPDATE items SET created_by_user_id = ?, assigned_user_id = ? WHERE id = ?`), it[1], it[2], it[0]); err != nil {
					t.Fatal(err)
				}
			}
		}},
	}

	for i, ing := range ingredients {
		i, ing := i, ing
		t.Run(ing.name, func(t *testing.T) {
			t.Parallel()
			s := testStore(t)
			for round := 0; round < 8; round++ {
				tag := fmt.Sprintf("Mu%dr%d", i, round)
				a := tabsUser(t, s, tag+"a")
				d := tabsUser(t, s, tag+"d")
				wa := tabsWorkspace(t, s, a, tag+"WA")
				wd := tabsWorkspace(t, s, d, tag+"WD")
				if err := s.AddWorkspaceMember(wa.ID, d.ID, "editor"); err != nil {
					t.Fatal(err)
				}
				if err := s.AddWorkspaceMember(wd.ID, a.ID, "editor"); err != nil {
					t.Fatal(err)
				}
				ca := createTestCollection(t, s, wa.ID, tag+"CA")
				cd := createTestCollection(t, s, wd.ID, tag+"CD")
				ia := createTestItem(t, s, wa.ID, ca.ID, tag+" item a", "")
				id := createTestItem(t, s, wd.ID, cd.ID, tag+" item d", "")
				ing.seed(t, s, pair{a: a.ID, d: d.ID, wa: wa.ID, wd: wd.ID, ca: ca.ID, cd: cd.ID, ia: ia.ID, id: id.ID})

				var wg sync.WaitGroup
				start := make(chan struct{})
				errs := make(chan error, 2)
				for _, uid := range []string{a.ID, d.ID} {
					uid := uid
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						if err := s.DeleteAccountAtomic(uid); err != nil {
							errs <- err
						}
					}()
				}
				close(start)
				wg.Wait()
				close(errs)
				for err := range errs {
					failOnDeadlockOrError(t, fmt.Sprintf("%s round %d", ing.name, round), err)
				}
				for _, uid := range []string{a.ID, d.ID} {
					if u, err := s.GetUser(uid); err == nil && u != nil {
						t.Fatalf("%s round %d: user %s survived its account deletion", ing.name, round, uid)
					}
				}
			}
		})
	}
}
