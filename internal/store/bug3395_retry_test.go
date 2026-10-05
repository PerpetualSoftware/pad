package store

import (
	"testing"
)

// BUG-3395: account deletion and the workspace purge retry a deadlocked
// attempt from the start (the two cycles their root-first comment lock
// cannot remove). Attempt 1 does all its writes and fails as a deadlock just
// before its commit; attempt 2 commits the whole effect.
func TestBug3395_DeadlockedAttemptIsRunAgain(t *testing.T) {
	for _, site := range []string{"delete_account", "purge_workspace"} {
		t.Run(site, func(t *testing.T) {
			s := testStore(t)
			a := tabsUser(t, s, "B3395r"+site)
			ws := tabsWorkspace(t, s, a, "B3395r"+site+"WS")
			sites := retryRecorder(s)
			attempts := 0
			s.deadlockInjector = func(got string, attempt int) bool {
				if got != site {
					return false
				}
				attempts = attempt
				return attempt == 1
			}
			var err error
			if site == "delete_account" {
				err = s.DeleteAccountAtomic(a.ID)
			} else {
				if _, xerr := s.db.Exec(s.q(`UPDATE workspaces SET deleted_at = ? WHERE id = ?`), now(), ws.ID); xerr != nil {
					t.Fatal(xerr)
				}
				err = s.PurgeWorkspaceData(ws.ID)
			}
			if err != nil {
				t.Fatalf("%s after one injected deadlock: %v", site, err)
			}
			if attempts != 2 || len(*sites) != 1 || (*sites)[0] != site {
				t.Errorf("%s: %d attempts, retries %v; want 2 and [%s]", site, attempts, *sites, site)
			}
			if site == "delete_account" {
				if n := task3394Count(t, s, `SELECT COUNT(*) FROM users WHERE id = ?`, a.ID); n != 0 {
					t.Error("the account survived")
				}
			} else if n := task3394Count(t, s, `SELECT COUNT(*) FROM workspaces WHERE id = ?`, ws.ID); n != 0 {
				t.Error("the workspace survived")
			}
		})
	}
}
