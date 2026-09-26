package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-3099. The delete-account handler read the list of workspaces to delete
// BEFORE the deletion's transaction, and DeleteAccountAtomic soft-deleted
// only the slugs it was handed. A workspace the same account minted in
// between survived as a LIVE workspace owned by a deleted user.

func accountDeleteServer(t *testing.T, driver store.DriverType) *Server {
	t.Helper()
	var s *store.Store
	if driver == store.DriverPostgres {
		s = storetest.NewPostgres(t) // skips when PAD_TEST_POSTGRES_URL is unset
	} else {
		s = storetest.NewSQLite(t)
	}
	if got := s.D().Driver(); got != driver {
		t.Fatalf("wanted a %s store, got %s", driver, got)
	}
	srv := New(s)
	t.Cleanup(func() { srv.Stop() })
	return srv
}

func liveWorkspace(t *testing.T, srv *Server, id string) bool {
	t.Helper()
	var n int
	if err := srv.store.DB().QueryRow(srv.store.D().Rebind(
		`SELECT COUNT(*) FROM workspaces WHERE id = ? AND deleted_at IS NULL`), id).Scan(&n); err != nil {
		t.Fatalf("read workspace %s: %v", id, err)
	}
	return n == 1
}

// TestDeleteAccount_WorkspaceMintedInTheWindowIsDeleted mints the workspace
// from inside the billing-cancel call, which the handler makes AFTER it used
// to read the owned list and BEFORE the deletion transaction: the exact
// window the filing names, reached deterministically on the real door. The
// control workspace, owned before the request, was deleted before the fix
// too; if it survives, the harness is wrong, not the fix.
func TestDeleteAccount_WorkspaceMintedInTheWindowIsDeleted(t *testing.T) {
	for _, driver := range []store.DriverType{store.DriverSQLite, store.DriverPostgres} {
		t.Run(string(driver), func(t *testing.T) {
			srv := accountDeleteServer(t, driver)
			userID, token := bootstrapAccountDeleteUser(t, srv, "cus_window")
			before, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Before", Slug: "before-3099", OwnerID: userID})
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.store.AddWorkspaceMember(before.ID, userID, "owner"); err != nil {
				t.Fatal(err)
			}

			var minted *models.Workspace
			fake := &fakeSidecar{}
			fake.hook = func(string) {
				ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "In window", Slug: "window-3099", OwnerID: userID})
				if err != nil {
					t.Errorf("mint in window: %v", err)
					return
				}
				if err := srv.store.AddWorkspaceMember(ws.ID, userID, "owner"); err != nil {
					t.Errorf("join in window: %v", err)
				}
				minted = ws
			}
			srv.SetCloudSidecar(fake)

			rr := deleteAccountReq(srv, map[string]interface{}{"password": "correct-horse-battery-staple"}, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("delete-account: got %d: %s", rr.Code, rr.Body.String())
			}
			if minted == nil {
				t.Fatal("the window hook never minted; the test measured nothing")
			}
			if liveWorkspace(t, srv, before.ID) {
				t.Fatal("control: a workspace owned before the request survived the deletion")
			}
			if liveWorkspace(t, srv, minted.ID) {
				t.Fatal("a workspace minted between the owned-list read and the deletion survived as a live workspace of a deleted user")
			}
		})
	}
}

// TestDeleteAccount_OwnedWorkspaceWithoutMembershipIsDeleted covers the
// second window the premise check found: the handler's list came from a
// MEMBERSHIP read, and CreateWorkspace commits the workspace before the
// owner's membership row is added, outside that transaction. A workspace
// that is owned but not joined was therefore invisible to the list with no
// race on the read at all.
func TestDeleteAccount_OwnedWorkspaceWithoutMembershipIsDeleted(t *testing.T) {
	for _, driver := range []store.DriverType{store.DriverSQLite, store.DriverPostgres} {
		t.Run(string(driver), func(t *testing.T) {
			srv := accountDeleteServer(t, driver)
			userID, token := bootstrapAccountDeleteUser(t, srv, "")
			unjoined, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Unjoined", Slug: "unjoined-3099", OwnerID: userID})
			if err != nil {
				t.Fatal(err)
			}
			rr := deleteAccountReq(srv, map[string]interface{}{"password": "correct-horse-battery-staple"}, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("delete-account: got %d: %s", rr.Code, rr.Body.String())
			}
			if liveWorkspace(t, srv, unjoined.ID) {
				t.Fatal("an owned workspace the owner had not joined survived the deletion")
			}
		})
	}
}
