package server

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3811: the instance keeps at least one active human admin, and every
// door that would take the last one answers 409 last_active_admin.

// The sole admin's account deletion is refused BEFORE the Stripe cancel,
// which cannot be undone, and the account is untouched.
func TestBUG3811_SoleAdminCannotDeleteTheirAccount(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "only-admin@test.com", "Only Admin")
	u, err := srv.store.GetUserByEmail("only-admin@test.com")
	if err != nil || u == nil {
		t.Fatal(err)
	}
	if err := srv.store.SetUserStripeCustomerID(u.ID, "cus_sole_admin"); err != nil {
		t.Fatal(err)
	}
	fake := &fakeSidecar{}
	srv.SetCloudSidecar(fake)

	rr := deleteAccountReq(srv, map[string]any{"password": "correct-horse-battery-staple"}, token)
	if rr.Code != http.StatusConflict || errorCode(t, rr) != "last_active_admin" {
		t.Fatalf("sole admin delete-account: %d %s, want 409 last_active_admin", rr.Code, rr.Body.String())
	}
	if n := fake.callCount(); n != 0 {
		t.Errorf("CancelCustomer called %d times for a refused deletion, want 0", n)
	}
	stillThere(t, srv, u.ID)

	// CONTROL: with a second active admin the same request deletes.
	if _, err := srv.store.CreateUser(models.UserCreate{Email: "second-admin@test.com", Name: "Second", Password: "pw-second-12345", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	if rr := deleteAccountReq(srv, map[string]any{"password": "correct-horse-battery-staple"}, token); rr.Code != http.StatusOK {
		t.Fatalf("delete-account beside a second admin: %d %s, want 200", rr.Code, rr.Body.String())
	}
}

// Demote and disable can only reach the refusal concurrently: the caller is
// an active admin and cannot target themselves. The interleaving is held
// open by hand on Postgres: a transaction takes the guard's lock and demotes
// admin A, the HTTP demote or disable of admin B (by A's own session,
// admitted before A's demotion) waits on it, and answers 409 once A's
// demotion commits.
func TestBUG3811_DemoteAndDisableAnswer409(t *testing.T) {
	for _, door := range []struct{ name, method, path string }{
		{"demote", "PATCH", "/api/v1/admin/users/%s"},
		{"disable", "POST", "/api/v1/admin/users/%s/disable"},
	} {
		t.Run(door.name, func(t *testing.T) {
			// Postgres only: the refusal is reachable only through a concurrent
			// removal, and SQLite serializes writers. Skips without
			// PAD_TEST_POSTGRES_URL (make test-pg).
			srv := accountDeleteServer(t, store.DriverPostgres)
			bootstrapFirstUser(t, srv, "boot@test.com", "Boot")
			mk := func(email string) (*models.User, string) {
				u, err := srv.store.CreateUser(models.UserCreate{Email: email, Name: email, Password: "pw-test-12345", Role: "admin"})
				if err != nil {
					t.Fatal(err)
				}
				tok, err := srv.store.CreateSession(u.ID, "web", "192.0.2.9", "", time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				return u, tok
			}
			a, aTok := mk("a-3811@test.com")
			b, _ := mk("b-3811@test.com")
			boot, _ := srv.store.GetUserByEmail("boot@test.com")
			if err := srv.store.SetUserRole(boot.ID, "member"); err != nil {
				t.Fatal(err)
			}

			tx, err := srv.store.DB().Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext('pad:active-admins'))"); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec("UPDATE users SET role = 'member' WHERE id = $1", a.ID); err != nil {
				t.Fatal(err)
			}

			var body any
			if door.name == "demote" {
				body = map[string]any{"role": "member"}
			}
			done := make(chan int, 1)
			var rrBody string
			var code string
			go func() {
				rr := doRequestWithCookieFrom(srv, door.method, fmt.Sprintf(door.path, b.ID), body, aTok, "192.0.2.9:1")
				rrBody = rr.Body.String()
				code = errorCode(t, rr)
				done <- rr.Code
			}()
			// Commit only once the request is seen WAITING on the guard's
			// lock, not after a fixed sleep (codex r1 on #2178).
			waited := false
			for deadline := time.Now().Add(10 * time.Second); !waited && time.Now().Before(deadline); {
				var n int
				if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
					AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
					AND objsubid = 1
					AND classid = (((hashtext('pad:active-admins')::bigint) >> 32) & 4294967295)::oid
					AND objid = ((hashtext('pad:active-admins')::bigint) & 4294967295)::oid`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if waited = n > 0; !waited {
					time.Sleep(20 * time.Millisecond)
				}
			}
			if !waited {
				t.Fatalf("the %s request never waited on the active-admin lock", door.name)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			var status int
			select {
			case status = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the request never returned")
			}
			if status != http.StatusConflict || code != "last_active_admin" {
				t.Fatalf("%s of the last active admin: %d %s, want 409 last_active_admin", door.name, status, rrBody)
			}
			if got, _ := srv.store.GetUser(b.ID); got == nil || got.Role != "admin" || got.IsDisabled() {
				t.Errorf("B was changed by a refused %s: %+v", door.name, got)
			}
		})
	}
}
