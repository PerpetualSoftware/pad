package server

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3479: removeUnusableWorkspace returned nil when its own soft-delete
// FAILED, and its contract says nil means "the workspace is gone". Every
// caller then reported a removal that had not happened, while a live,
// ownerless workspace kept its slug. A trigger refuses the soft-delete so
// each caller can be driven into exactly that state (SQLite).

func refuseSoftDelete(t *testing.T, srv *Server) {
	t.Helper()
	if _, err := srv.store.DB().Exec(`CREATE TRIGGER t3479_no_soft_delete BEFORE UPDATE OF deleted_at ON workspaces
		BEGIN SELECT RAISE(ABORT, 'soft-delete refused (BUG-3479 test)'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
}

func liveBySlug(t *testing.T, srv *Server, slug string) *models.Workspace {
	t.Helper()
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug(%q): %v", slug, err)
	}
	return ws
}

// The helper itself: a failed delete is an ERROR, and the workspace is still
// there; the control (no trigger) is nil and gone.
func TestRemoveUnusableWorkspace_BUG3479_FailedDeleteIsAnError(t *testing.T) {
	srv := attachmentsServerOn(t, store.DriverSQLite)

	ctl, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Control 3479"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.removeUnusableWorkspace("test", ctl.ID, ctl.Slug, "", errSimSeedFailure); err != nil {
		t.Fatalf("control: a removal that succeeded returned %v", err)
	}
	if liveBySlug(t, srv, ctl.Slug) != nil {
		t.Fatal("control: the workspace is still live after a successful removal")
	}

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Stuck 3479"})
	if err != nil {
		t.Fatal(err)
	}
	refuseSoftDelete(t, srv)
	err = srv.removeUnusableWorkspace("test", ws.ID, ws.Slug, "", errSimSeedFailure)
	if err == nil {
		t.Fatal("a removal whose delete failed returned nil, which callers read as 'gone'")
	}
	if !strings.Contains(err.Error(), "could not be removed") {
		t.Errorf("the error should say the workspace could not be removed: %v", err)
	}
	if liveBySlug(t, srv, ws.Slug) == nil {
		t.Fatal("precondition: the trigger should have kept the workspace live")
	}
}

// addOwnerOrCompensate's ABSENT arm: the owner row is genuinely absent, the
// removal's delete fails, and the error must be the KEPT one, not the
// "removed" one its callers read as gone.
func TestAddOwnerOrCompensate_BUG3479_FailedRemovalIsReportedAsKept(t *testing.T) {
	srv := ackLossCloudServer(t, store.DriverSQLite)
	user := realUser(t, srv, "absent3479@example.com")
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Absent 3479"})
	if err != nil {
		t.Fatal(err)
	}
	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimMemberAckLoss
	})
	t.Cleanup(restore)
	refuseSoftDelete(t, srv)

	err = srv.addOwnerOrCompensate("test", ws.ID, ws.Slug, user.ID)
	if err == nil || !strings.Contains(err.Error(), "could not be removed") {
		t.Fatalf("want the kept error naming the failed removal, got %v", err)
	}
	if liveBySlug(t, srv, ws.Slug) == nil {
		t.Fatal("precondition: the workspace should still be live")
	}
}

// Cloud auto-create, both arms: the log must not end on "being removed" when
// the workspace survived.
func TestAutoCreateWorkspace_BUG3479_FailedRemovalIsLogged(t *testing.T) {
	t.Run("seed failure", func(t *testing.T) {
		srv := ackLossCloudServer(t, store.DriverSQLite)
		user := realUser(t, srv, "seed3479@example.com")
		logs := captureLogs(t)
		restore := srv.store.SetSeedCollectionsFailureHookForTesting(func(string, string) error { return errSimSeedFailure })
		t.Cleanup(restore)
		refuseSoftDelete(t, srv)

		srv.autoCreateWorkspace(user)

		if !strings.Contains(logs.String(), "could NOT be removed and is still live") {
			t.Errorf("a failed removal was not logged as one: %s", logs.String())
		}
	})
	t.Run("owner never written", func(t *testing.T) {
		srv := ackLossCloudServer(t, store.DriverSQLite)
		user := realUser(t, srv, "absent3479b@example.com")
		logs := captureLogs(t)
		restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
			_ = tx.Rollback()
			return errSimMemberAckLoss
		})
		t.Cleanup(restore)
		refuseSoftDelete(t, srv)

		srv.autoCreateWorkspace(user)

		if !strings.Contains(logs.String(), "could NOT be removed and is still live") {
			t.Errorf("a failed removal was not logged as one: %s", logs.String())
		}
	})
}

// Create workspace, seed failure: the 500 says the half-made workspace
// survived and holds its name, instead of reading as "nothing was created".
func TestCreateWorkspace_BUG3479_SeedFailureThatCannotBeRemovedSaysSo(t *testing.T) {
	srv := attachmentsServerOn(t, store.DriverSQLite)
	_, tok := memberImporter(t, srv)
	restore := srv.store.SetSeedCollectionsFailureHookForTesting(func(string, string) error { return errSimSeedFailure })
	t.Cleanup(restore)
	refuseSoftDelete(t, srv)

	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]any{"name": "Seedless", "template": "startup"}, tok)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "could not be removed and still holds its name") {
		t.Errorf("the 500 must say the workspace survived: %s", rr.Body.String())
	}
	if liveBySlug(t, srv, "seedless") == nil {
		t.Fatal("precondition: the trigger should have kept the workspace live")
	}
}
