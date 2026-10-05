package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3395: account deletion and the workspace purge retry a deadlocked
// transaction inside the store call. Every effect outside that transaction
// (the access-changed publishes, kicks, invalidations, audit event, blob
// reclaim) runs in the caller, so it must happen ONCE however many attempts
// the store took. Each case fails attempt 1 as a deadlock just before its
// commit and counts a member's notification.
func TestBug3395_CallerEffectsRunOnceAcrossARetry(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		count := func(lines []string, want string) int {
			n := 0
			for _, l := range lines {
				if l == want {
					n++
				}
			}
			return n
		}
		inject := func(t *testing.T, f *accessFixture, site string) *int {
			attempts := 0
			restore := f.srv.store.SetDeadlockInjectorForTesting(func(got string, attempt int) bool {
				if got != site {
					return false
				}
				attempts = attempt
				return attempt == 1
			})
			t.Cleanup(restore)
			return &attempts
		}

		t.Run("account deletion", func(t *testing.T) {
			f := newAccessFixture(t, d)
			f.member("member@example.com", "editor")
			sess, err := f.srv.store.CreateSession(f.owner.ID, "cli-browser-auth", "192.0.2.1", "", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			attempts := inject(t, f, "delete_account")
			since := f.mark()
			f.must(f.do("POST", "/api/v1/auth/delete-account", sess,
				map[string]any{"password": "correct-horse-battery-staple"}), http.StatusOK, "delete account")
			if *attempts != 2 {
				t.Fatalf("the store took %d attempts, want 2: the injected deadlock did not fire", *attempts)
			}
			if n := count(f.accessSince(since), f.line("deleted", "member@example.com")); n != 1 {
				t.Errorf("the member was told %d times, want once", n)
			}
			var audits int
			if err := f.srv.store.DB().QueryRow(`SELECT COUNT(*) FROM activities WHERE action = '` + models.ActionAccountDeleted + `'`).Scan(&audits); err != nil {
				t.Fatal(err)
			}
			if audits != 1 {
				t.Errorf("%d account_deleted audit events, want one", audits)
			}
		})

		t.Run("workspace purge", func(t *testing.T) {
			f := newAccessFixture(t, d)
			f.member("member@example.com", "viewer")
			f.must(f.do("DELETE", "/api/v1/workspaces/"+f.wsSlug, f.ownerTok, nil), http.StatusNoContent, "soft delete")
			attempts := inject(t, f, "purge_workspace")
			since := f.mark()
			res, err := f.srv.runWorkspacePurgeSweep(context.Background(), time.Now().UTC().Add(time.Minute))
			if err != nil || res.Purged != 1 {
				t.Fatalf("purge sweep: res=%+v err=%v", res, err)
			}
			if *attempts != 2 {
				t.Fatalf("the store took %d attempts, want 2: the injected deadlock did not fire", *attempts)
			}
			if n := count(f.accessSince(since), f.line("purged", "member@example.com")); n != 1 {
				t.Errorf("the member was told %d times, want once", n)
			}
		})
	})
}
