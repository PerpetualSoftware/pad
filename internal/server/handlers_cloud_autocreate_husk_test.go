package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3087: two ways the cloud auto-create door left a workspace behind that
// the product then misdescribed.
//
// SEED FAILURE was logged and skipped, and the signup reported success. The user
// got a workspace the dashboard and the agent bootstrap both flagged
// needs_onboarding, whose onboard playbook the seed never wrote (measured on the
// item trail: `playbooks/onboard` answered 404). Nothing re-runs the seed —
// autoCreateWorkspace has exactly three callers, all signup doors, and the
// store's zero-collection rescue has no callers at all — so the state was
// permanent. The door now removes the workspace and the signup still succeeds:
// the user row, the session and the verification email already exist by then,
// so failing the request would 409 on retry for an error that was never about
// the account, and the console's empty state is the honest picture.
//
// The ABSENT arm soft-deleted only. ListDeletedWorkspaces is scoped by
// workspaces.owner_id, not by membership, so the husk sat in the user's
// deleted-workspaces list for 30 days, and restoring it returned a workspace
// with no member row: after the restore the owner got 403 on every read AND on
// the delete (measured on the trail). removeUnusableWorkspace — the guarded
// soft-delete-plus-purge BUG-2715 wrote for the other three doors — replaces
// the bare DeleteWorkspace. BUG-3026's four arms are untouched: the helper
// changes what the removal IS, not when it fires.
//
// THE ASSERTION THE OLDER TESTS CANNOT MAKE. ListWorkspaces filters
// `deleted_at IS NULL`, so a "workspace count unchanged" check passes against a
// soft delete and against a purge alike. The contract here is
// ListDeletedWorkspaces(owner) EMPTY afterwards — the query the console's
// deleted-workspaces page runs — and the ABSENT leg was run against the
// pre-fix code and went red on exactly that line before the fix went in.
//
// None of these call t.Parallel(): they capture the process-global slog
// default, which CONVE-2086 names as a reason not to.

var errSimSeedFailure = errors.New("simulated template seed failure")

// restorableHusks is the question the console asks: what would this user see
// under "Deleted workspaces"? cutoff mirrors the purge retention the handler
// derives.
func restorableHusks(t *testing.T, srv *Server, ownerID string) []models.Workspace {
	t.Helper()
	rows, err := srv.store.ListDeletedWorkspaces(ownerID, time.Now().UTC().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("ListDeletedWorkspaces: %v", err)
	}
	return rows
}

// captureLogs lives in workspace_owner_compensation_test.go.

// --- seed failure ---------------------------------------------------------

// TestCloudRegister_SeedFailure_RemovesTheWorkspaceAndSignupStillSucceeds drives
// the real register door, because "the signup response is unchanged" is the
// claim disposition 1 makes and only the door can answer it.
func TestCloudRegister_SeedFailure_RemovesTheWorkspaceAndSignupStillSucceeds(t *testing.T) {
	srv, _ := newCloudEmailServer(t)
	logs := captureLogs(t)

	before, err := srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces before: %v", err)
	}

	var seedCalls int
	restore := srv.store.SetSeedCollectionsFailureHookForTesting(func(workspaceID, templateName string) error {
		seedCalls++
		if templateName != "startup" {
			t.Errorf("auto-create seeded template %q, want startup", templateName)
		}
		return errSimSeedFailure
	})
	t.Cleanup(restore)

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "seedfail@pad.test",
		"name":     "Seed Fail",
		"password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("register with a failing seed: expected 201 (the signup is not the thing that failed), got %d: %s", rr.Code, rr.Body.String())
	}
	var reg struct {
		Token string `json:"token"`
		User  struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"user"`
	}
	parseJSON(t, rr, &reg)
	if reg.Token == "" || reg.User.ID == "" || reg.User.Email != "seedfail@pad.test" || reg.User.Name != "Seed Fail" {
		t.Fatalf("register body changed shape on the seed-failure path: %s", rr.Body.String())
	}
	if seedCalls != 1 {
		t.Fatalf("seed hook fired %d times, want exactly 1 (no seed retry, by ruling)", seedCalls)
	}

	// The user exists and can sign in; the workspace does not exist in any
	// list the user can reach — live, deleted, or restorable.
	u, err := srv.store.GetUserByEmail("seedfail@pad.test")
	if err != nil || u == nil {
		t.Fatalf("GetUserByEmail: user=%v err=%v", u, err)
	}
	mine, err := srv.store.GetUserWorkspaces(u.ID)
	if err != nil {
		t.Fatalf("GetUserWorkspaces: %v", err)
	}
	if len(mine) != 0 {
		t.Errorf("user still has %d live workspace(s) after a failed seed; want 0 (a usable workspace or nothing)", len(mine))
	}
	if husks := restorableHusks(t, srv, u.ID); len(husks) != 0 {
		t.Errorf("failed-seed workspace is RESTORABLE by its owner (%d row(s) in ListDeletedWorkspaces); want none — a restored husk has no member row", len(husks))
	}
	after, err := srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces after: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("live workspace count %d -> %d, want unchanged", len(before), len(after))
	}

	logged := logs.String()
	if !strings.Contains(logged, "level=ERROR") {
		t.Errorf("a seed failure now causes a removal and must be logged at ERROR; got: %s", logged)
	}
	if !strings.Contains(logged, "cloud auto-create") || !strings.Contains(logged, "removing the workspace") {
		t.Errorf("expected the door-labelled removal log line, got: %s", logged)
	}
}

// TestAutoCreateWorkspace_SeedFailure_LeavesNoRestorableHusk is the same
// property on the function itself, on both dialects: the purge is what makes
// the husk go away, and the purge is a real SQL cascade.
func TestAutoCreateWorkspace_SeedFailure_LeavesNoRestorableHusk_SQLite(t *testing.T) {
	seedFailureLeavesNoRestorableHusk(t, store.DriverSQLite)
}

func TestAutoCreateWorkspace_SeedFailure_LeavesNoRestorableHusk_Postgres(t *testing.T) {
	seedFailureLeavesNoRestorableHusk(t, store.DriverPostgres)
}

func seedFailureLeavesNoRestorableHusk(t *testing.T, driver store.DriverType) {
	srv := ackLossCloudServer(t, driver)
	user := realUser(t, srv, "seedfail-"+string(driver)+"@example.com")

	var seededWorkspace string
	restore := srv.store.SetSeedCollectionsFailureHookForTesting(func(workspaceID, _ string) error {
		seededWorkspace = workspaceID
		return errSimSeedFailure
	})
	t.Cleanup(restore)

	srv.autoCreateWorkspace(user)

	if seededWorkspace == "" {
		t.Fatal("seed hook never fired; the test measured nothing")
	}
	// The hook fires AFTER the template's collections exist, so the removal
	// here purges a workspace that has child rows — the realistic shape.
	if husks := restorableHusks(t, srv, user.ID); len(husks) != 0 {
		t.Errorf("failed-seed workspace is restorable by its owner (%d row(s)); want none", len(husks))
	}
	if ws, _ := srv.store.GetWorkspaceByID(seededWorkspace); ws != nil {
		t.Errorf("failed-seed workspace %s is still live", seededWorkspace)
	}
	if mine, _ := srv.store.GetUserWorkspaces(user.ID); len(mine) != 0 {
		t.Errorf("user has %d live workspace(s) after a failed seed; want 0", len(mine))
	}
}

// --- ABSENT arm -----------------------------------------------------------

// The pre-fix code passes every assertion here EXCEPT the restorable-husk one:
// it soft-deletes, so the count is unchanged and the pinned log line is
// present, and ListDeletedWorkspaces(owner) returns the row.
func TestAutoCreateWorkspace_AbsentArm_LeavesNoRestorableHusk_SQLite(t *testing.T) {
	absentArmLeavesNoRestorableHusk(t, store.DriverSQLite)
}

func TestAutoCreateWorkspace_AbsentArm_LeavesNoRestorableHusk_Postgres(t *testing.T) {
	absentArmLeavesNoRestorableHusk(t, store.DriverPostgres)
}

func absentArmLeavesNoRestorableHusk(t *testing.T, driver store.DriverType) {
	srv := ackLossCloudServer(t, driver)
	user := realUser(t, srv, "absent-"+string(driver)+"@example.com")
	logs := captureLogs(t)

	before, err := srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces before: %v", err)
	}

	// The INSERT is rolled back and reported failed, twice; the real
	// membership read then correctly answers ABSENT (the arm under test, the
	// same setup TestAutoCreateWorkspaceReconcileArmsAreDistinguishable uses).
	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimMemberAckLoss
	})
	t.Cleanup(restore)

	srv.autoCreateWorkspace(user)

	after, err := srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces after: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("live workspace count %d -> %d, want unchanged", len(before), len(after))
	}
	if husks := restorableHusks(t, srv, user.ID); len(husks) != 0 {
		t.Errorf("ABSENT-arm workspace is RESTORABLE by its owner (%d row(s) in ListDeletedWorkspaces); "+
			"want none — restoring it hands back a workspace with no member row", len(husks))
	}
	logged := logs.String()
	if !strings.Contains(logged, "add owner member failed after retry") {
		t.Errorf("the pinned retry-then-cleanup log line must survive the change to the removal, got: %s", logged)
	}
	if !strings.Contains(logged, "cloud auto-create") || !strings.Contains(logged, "removing the workspace") {
		t.Errorf("expected the door-labelled removal log line, got: %s", logged)
	}
}
