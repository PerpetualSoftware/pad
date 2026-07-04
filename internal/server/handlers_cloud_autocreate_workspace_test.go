package server

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestAutoCreateWorkspace_MemberAddFailure_CleansUpAndLogsLoudly covers the
// B6 hardening from TASK-1932: if AddWorkspaceMember fails after the
// workspace row is created, autoCreateWorkspace must not leave a
// permanently orphaned, completely inaccessible workspace behind, and must
// log loudly so on-call can see it happened.
//
// The failure is forced with a real DB-level error rather than a mock: the
// user's ID is never inserted into the users table, so workspace_members'
// FK on user_id rejects the INSERT (workspaces.owner_id has no such FK, so
// CreateWorkspace succeeds first, matching the exact failure shape B6
// describes).
func TestAutoCreateWorkspace_MemberAddFailure_CleansUpAndLogsLoudly(t *testing.T) {
	srv := testServer(t)
	srv.cloudMode = true

	before, err := srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces before: %v", err)
	}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	ghost := &models.User{
		ID:    "ghost-user-not-in-db",
		Name:  "Ghost",
		Email: "ghost@example.com",
	}
	srv.autoCreateWorkspace(ghost)

	after, err := srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces after: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("expected no net-new workspace after a failed member-add (orphan must be cleaned up), before=%d after=%d",
			len(before), len(after))
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "level=ERROR") {
		t.Errorf("expected a loud (ERROR-level) log for the failed member-add, got: %s", logged)
	}
	if !strings.Contains(logged, "add owner member failed after retry") {
		t.Errorf("expected the failure log to describe the retry-then-cleanup path, got: %s", logged)
	}
}

// TestAutoCreateWorkspace_DeleteWorkspace_NoRowsIsAReportableError pins the
// store-layer contract that autoCreateWorkspace's compensating cleanup
// depends on: Store.DeleteWorkspace returns a non-nil error (rather than a
// silent no-op) when the slug doesn't exist or is already soft-deleted.
// That's what makes the "cleanup also failed" branch in autoCreateWorkspace
// (the one that logs "manual intervention required" instead of the plain
// success message) reachable and distinguishable from the happy-cleanup
// case exercised above. End-to-end coverage of that double-failure branch
// (member-add fails on retry AND the compensating delete fails) would
// require racing a delete against autoCreateWorkspace's own delete call, or
// a production test-seam — deliberately not added per the TASK-1932 plan —
// so this test instead pins the store-level building block the branch
// relies on.
func TestAutoCreateWorkspace_DeleteWorkspace_NoRowsIsAReportableError(t *testing.T) {
	srv := testServer(t)

	if err := srv.store.DeleteWorkspace("does-not-exist-slug"); err == nil {
		t.Fatal("expected DeleteWorkspace to return an error for a nonexistent slug, got nil")
	}
}
