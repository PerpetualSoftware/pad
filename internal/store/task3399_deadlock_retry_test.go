package store

import (
	"testing"
)

// TASK-3399 (lead ruling (a) on codex U5b-1 r5): a deadlocked transaction is
// run again from the start, at most three times.

func retryRecorder(s *Store) *[]string {
	var sites []string
	s.SetDeadlockRetryObserver(func(site string) { sites = append(sites, site) })
	return &sites
}

// Attempt 1 does all its writes, then fails as a deadlock just before its
// commit; attempt 2 commits the whole effect, once.
func TestTask3399_ADeadlockedAttemptIsRunAgainFromTheStart(t *testing.T) {
	cases := map[string]func(t *testing.T, f task3399Fix) (run func() error, check func(t *testing.T)){
		"remove_workspace_member": func(t *testing.T, f task3399Fix) (func() error, func(t *testing.T)) {
			return func() error { return f.s.RemoveWorkspaceMember(f.ws.ID, f.person.ID) }, func(t *testing.T) {
				if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM workspace_members WHERE user_id = ? AND workspace_id = ?`, f.person.ID, f.ws.ID); n != 0 {
					t.Error("the membership survived")
				}
				if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM app_token_bindings WHERE request_id = 'req-dl' AND revoked_at IS NOT NULL`); n != 1 {
					t.Error("the grant was not revoked")
				}
			}
		},
		"disable_user": func(t *testing.T, f task3399Fix) (func() error, func(t *testing.T)) {
			before := task3394Count(t, f.s, `SELECT credential_epoch FROM users WHERE id = ?`, f.person.ID)
			return func() error { return f.s.DisableUserAndRevokeAccess(f.person.ID) }, func(t *testing.T) {
				if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM users WHERE id = ? AND disabled_at IS NOT NULL`, f.person.ID); n != 1 {
					t.Error("the person is not disabled")
				}
				if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM oauth_refresh_tokens WHERE request_id = 'req-dl' AND active = ?`, true); n != 0 {
					t.Error("the grant's refresh token is still active")
				}
				if n := task3394Count(t, f.s, `SELECT credential_epoch FROM users WHERE id = ?`, f.person.ID); n != before+1 {
					t.Errorf("credential_epoch = %d, want %d: one committed bump, not one per attempt", n, before+1)
				}
			}
		},
		"revoke_app_grant": func(t *testing.T, f task3399Fix) (func() error, func(t *testing.T)) {
			return func() error { _, err := f.s.RevokeUserAppGrant(f.person.ID, "req-dl"); return err }, func(t *testing.T) {
				if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM app_token_bindings WHERE request_id = 'req-dl' AND revoked_at IS NOT NULL`); n != 1 {
					t.Error("the grant was not revoked")
				}
			}
		},
	}
	for site, build := range cases {
		t.Run(site, func(t *testing.T) {
			f := task3399Fixture(t, "inst-dl-"+site, "write")
			task3399Delegated(t, f, "req-dl")
			sites := retryRecorder(f.s)
			attempts := 0
			f.s.deadlockInjector = func(got string, attempt int) bool {
				if got != site {
					return false
				}
				attempts = attempt
				return attempt == 1
			}
			run, check := build(t, f)
			if err := run(); err != nil {
				t.Fatalf("%s after one injected deadlock: %v", site, err)
			}
			if attempts != 2 {
				t.Errorf("%s ran %d attempts, want 2", site, attempts)
			}
			if len(*sites) != 1 || (*sites)[0] != site {
				t.Errorf("retry observer saw %v, want [%s]", *sites, site)
			}
			check(t)
		})
	}
}

// Three deadlocks in a row: the last error is returned unchanged, and
// nothing any attempt wrote is committed.
func TestTask3399_DeadlockRetriesAreBounded(t *testing.T) {
	f := task3399Fixture(t, "inst-dl-bound", "write")
	task3399Delegated(t, f, "req-dlb")
	sites := retryRecorder(f.s)
	calls := 0
	f.s.deadlockInjector = func(string, int) bool { calls++; return true }
	err := f.s.RemoveWorkspaceMember(f.ws.ID, f.person.ID)
	if err != errInjectedDeadlock {
		t.Fatalf("err = %v, want the last attempt's deadlock error unchanged", err)
	}
	if calls != deadlockRetryAttempts || len(*sites) != deadlockRetryAttempts-1 {
		t.Errorf("%d attempts and %d retries, want %d and %d", calls, len(*sites), deadlockRetryAttempts, deadlockRetryAttempts-1)
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM workspace_members WHERE user_id = ? AND workspace_id = ?`, f.person.ID, f.ws.ID); n != 1 {
		t.Error("a failed attempt committed the removal")
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM app_token_bindings WHERE request_id = 'req-dlb' AND revoked_at IS NOT NULL`); n != 0 {
		t.Error("a failed attempt committed the revoke")
	}
	// Any other error is returned at once, never retried.
	f.s.deadlockInjector = nil
	*sites = nil
	if err := f.s.RemoveWorkspaceMember(f.ws.ID, "no-such-user"); err == nil || len(*sites) != 0 {
		t.Errorf("a non-deadlock error: err %v, %d retries; want an error and none", err, len(*sites))
	}
}
