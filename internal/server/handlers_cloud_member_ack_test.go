package server

import (
	"bytes"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-3026: the cloud auto-create path soft-deleted the workspace it had just
// created successfully, whenever AddWorkspaceMember's commit landed and reported an
// error anyway (the Postgres ack-loss shape).
//
// AddWorkspaceMember is a plain INSERT against PRIMARY KEY (workspace_id, user_id)
// returning the raw tx.Commit() error. So a lost acknowledgement puts the row on
// disk and reports failure; the handler's retry then hits the primary key and fails
// deterministically; and the handler concluded the owner could not be added and
// deleted the workspace. The deletion fired PRECISELY in the case where the write
// had succeeded — and on exactly the "dropped Postgres connection" the retry was
// written for.
//
// Found by the CONVE-18 class sweep for BUG-2994, which is the same defect (a store
// write retried after an error whose outcome is ambiguous) with a worse consequence.
//
// The ABSENT arm — a membership that genuinely is not there, where deleting remains
// correct — is covered by the older ghost-user FK test in
// handlers_cloud_autocreate_workspace_test.go, which must keep passing.

var errSimMemberAckLoss = errors.New("simulated member-add commit ack loss")

func ackLossCloudServer(t *testing.T, driver store.DriverType) *Server {
	t.Helper()
	var s *store.Store
	if driver == store.DriverPostgres {
		s = storetest.NewPostgres(t) // skips if PAD_TEST_POSTGRES_URL is unset
	} else {
		s = storetest.NewSQLite(t)
	}
	if got := s.D().Driver(); got != driver {
		t.Fatalf("wanted a %s store, got %s — this leg would have measured the wrong backend", driver, got)
	}
	srv := New(s)
	t.Cleanup(func() { srv.Stop() })
	srv.cloudMode = true
	return srv
}

// realUser inserts a user so workspace_members' FK is satisfied and the INSERT can
// actually succeed — the opposite setup from the ghost-user test, and the
// precondition for measuring a commit that LANDS.
func realUser(t *testing.T, srv *Server, email string) *models.User {
	t.Helper()
	u, err := srv.store.CreateUser(models.UserCreate{
		Email:    email,
		Name:     "Ack Loss",
		Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

func TestAutoCreateWorkspace_MemberAddAckLossKeepsTheWorkspace_SQLite(t *testing.T) {
	assertAckLossKeepsTheWorkspace(t, ackLossCloudServer(t, store.DriverSQLite))
}

func TestAutoCreateWorkspace_MemberAddAckLossKeepsTheWorkspace_Postgres(t *testing.T) {
	assertAckLossKeepsTheWorkspace(t, ackLossCloudServer(t, store.DriverPostgres))
}

func assertAckLossKeepsTheWorkspace(t *testing.T, srv *Server) {
	t.Helper()
	user := realUser(t, srv, "ackloss@example.com")

	before, err := srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces before: %v", err)
	}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	// Fail the FIRST commit only, and commit it for real first: the membership row
	// is durable and the caller is told it is not. The retry then hits the primary
	// key on its own, with no help from the seam — which is the mechanism under
	// test, not something the test simulates.
	var commits int32
	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		n := atomic.AddInt32(&commits, 1)
		if cerr := tx.Commit(); cerr != nil {
			return cerr
		}
		if n == 1 {
			return errSimMemberAckLoss
		}
		return nil
	})
	t.Cleanup(restore)

	srv.autoCreateWorkspace(user)

	// EXACTLY ONE commit — and the one is the mechanism, not an accident. The retry
	// does run, but its INSERT dies on PRIMARY KEY (workspace_id, user_id) before it
	// ever reaches tx.Commit, BECAUSE the first attempt's row is already on disk.
	// So a commit count of 1 here is positive evidence that the first write landed.
	//
	// (Measured, after a first draft asserted 2 on the assumption that a retry
	// reaches COMMIT — codex round 1 P3 asked for the retry to be pinned, and the
	// obvious way to pin it encodes a mechanism that does not hold.) The retry's
	// existence is pinned by its log line below instead.
	if n := atomic.LoadInt32(&commits); n != 1 {
		t.Fatalf("the member add reached COMMIT %d time(s), want 1: the retry must die on the primary key "+
			"the first attempt's committed row created", n)
	}
	// BOUNDARY, stated because the line reads stronger than it is: this log is
	// emitted immediately BEFORE the retry call, so it pins that the retry BRANCH
	// was entered, not that the second INSERT executed. Nothing between the two can
	// skip it, so the distinction is theoretical today — but a reader should not
	// take this as proof the retry ran (codex round 2).
	if logged := logBuf.String(); !strings.Contains(logged, "add owner member failed, retrying") {
		t.Errorf("the retry branch was not entered; without it this test does not describe the reported "+
			"path. log: %s", logged)
	}

	after, err := srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces after: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("workspace count %d -> %d, want exactly one more: the workspace was deleted even though "+
			"the owner-membership row had committed", len(before), len(after))
	}

	// (ListWorkspaces filters deleted_at IS NULL, so the COUNT above IS the
	// not-deleted assertion; a separate DeletedAt check on a returned row would be
	// vacuous by construction.)
	//
	// Found BY OWNER, not by position: ListWorkspaces is ORDER BY name ASC, so
	// after[len(after)-1] is only this workspace by accident of today's empty
	// fixture (codex round 1).
	var ws *models.Workspace
	for i := range after {
		if after[i].OwnerID == user.ID {
			ws = &after[i]
			break
		}
	}
	if ws == nil {
		t.Fatal("no workspace owned by the new user; autoCreateWorkspace did not create one")
	}

	// ANTI-VACUITY CONTROL. The test is meaningless unless the first commit
	// genuinely landed, so assert the row is really there AND carries the role the
	// write attempted. Without this the test would also pass against a seam that
	// merely failed the insert — the case the ghost-user FK test already covers and
	// which was never broken.
	member, cerr := srv.store.GetWorkspaceMember(ws.ID, user.ID)
	if cerr != nil {
		t.Fatalf("GetWorkspaceMember: %v", cerr)
	}
	if member == nil {
		t.Fatal("the owner-membership row is absent, so the first commit did not land and this test " +
			"measured the ordinary insert-failure path instead of an ack loss")
	}
	if member.Role != "owner" {
		t.Fatalf("membership role = %q, want owner", member.Role)
	}
}

// TestAutoCreateWorkspace_UnreadableMembershipKeepsTheWorkspace pins the third
// outcome, and it is the arm most likely to be lost to a later simplification:
// collapsing the read ERROR back into "absent" restores the deletion silently, and
// nothing else in the suite would notice.
//
// Destroying a workspace on a state nobody could read is the worst of the three
// outcomes. The orphan it would avoid is recoverable, and three sibling call sites
// accept exactly that orphan deliberately (BUG-2715).
func TestAutoCreateWorkspace_UnreadableMembershipKeepsTheWorkspace(t *testing.T) {
	srv := ackLossCloudServer(t, store.DriverSQLite)
	user := realUser(t, srv, "unreadable@example.com")

	before, err := srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces before: %v", err)
	}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	// Both attempts fail outright — nothing is committed, so on the evidence the
	// handler has, the membership may or may not exist.
	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimMemberAckLoss
	})
	t.Cleanup(restore)

	var checks int32
	srv.membershipCheck = func(string, string) (*models.WorkspaceMember, error) {
		atomic.AddInt32(&checks, 1)
		return nil, errors.New("simulated membership read failure")
	}
	t.Cleanup(func() { srv.membershipCheck = nil })

	srv.autoCreateWorkspace(user)

	if atomic.LoadInt32(&checks) == 0 {
		t.Fatal("the membership check never ran; the reconcile was not reached")
	}
	after, err := srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces after: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Errorf("workspace count %d -> %d, want exactly one more: the workspace was destroyed on a "+
			"membership state that could not be read", len(before), len(after))
	}

	// The count alone does NOT identify this arm — the owner-success arm keeps the
	// workspace too, so collapsing UNREADABLE into it would pass the check above
	// (codex round 2). The log is what separates them.
	logged := logBuf.String()
	if !strings.Contains(logged, "KEEPING the workspace because its state is unknown") {
		t.Errorf("the unreadable-state arm did not run; some other arm kept the workspace. log: %s", logged)
	}
	if strings.Contains(logged, "reconciled to success") {
		t.Errorf("an unreadable membership state was reported as success. log: %s", logged)
	}
}

// TestAutoCreateWorkspaceReconcileArmsAreDistinguishable is the control for the two
// tests above: it proves the ABSENT arm still deletes, so a fix that simply stopped
// deleting altogether — which would pass both tests above — fails here.
func TestAutoCreateWorkspaceReconcileArmsAreDistinguishable(t *testing.T) {
	srv := ackLossCloudServer(t, store.DriverSQLite)
	user := realUser(t, srv, "absent@example.com")

	before, err := srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces before: %v", err)
	}

	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimMemberAckLoss
	})
	t.Cleanup(restore)
	// No membershipCheck seam: the REAL read runs and correctly reports absence.

	srv.autoCreateWorkspace(user)

	after, err := srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces after: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("workspace count %d -> %d, want unchanged: a genuinely absent membership leaves the "+
			"workspace unreachable, so the cleanup must still delete it", len(before), len(after))
	}
}

// TestAutoCreateWorkspace_WrongRoleMembershipKeepsTheWorkspace pins the fourth arm.
//
// It exists because the first version of this reconcile asked the wrong question
// (codex round 1 P2): it used an EXISTENCE check, so a row carrying any role at all
// reconciled to success. What was attempted was an OWNER row, so a row with another
// role means the write did NOT land — and the user cannot administer their own
// auto-created workspace, since owner > editor > viewer.
//
// Neither success nor deletion is honest there, so the workspace is kept and the
// failure is logged loudly. Collapsing this arm back into the LANDED one restores a
// silent success, and nothing else in the suite would notice.
func TestAutoCreateWorkspace_WrongRoleMembershipKeepsTheWorkspace(t *testing.T) {
	srv := ackLossCloudServer(t, store.DriverSQLite)
	user := realUser(t, srv, "wrongrole@example.com")

	before, err := srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces before: %v", err)
	}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimMemberAckLoss
	})
	t.Cleanup(restore)

	var checks int32
	srv.membershipCheck = func(workspaceID, userID string) (*models.WorkspaceMember, error) {
		atomic.AddInt32(&checks, 1)
		return &models.WorkspaceMember{WorkspaceID: workspaceID, UserID: userID, Role: "viewer"}, nil
	}
	t.Cleanup(func() { srv.membershipCheck = nil })

	srv.autoCreateWorkspace(user)

	if atomic.LoadInt32(&checks) == 0 {
		t.Fatal("the membership check never ran; the reconcile was not reached")
	}
	after, err := srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces after: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Errorf("workspace count %d -> %d, want exactly one more: a membership row exists, so somebody "+
			"has access and the workspace must not be destroyed", len(before), len(after))
	}

	// THE POINT OF THIS TEST, and the count above cannot carry it: the owner arm
	// keeps the workspace too, so folding wrong-role back into "a row exists means
	// success" — the exact defect round 1 found — would pass the count check
	// (codex round 2). What must be true is that this is reported as a FAILURE
	// needing a human, not as a success.
	logged := logBuf.String()
	if !strings.Contains(logged, "carries a "+"different role") {
		t.Errorf("the wrong-role arm did not run. log: %s", logged)
	}
	if strings.Contains(logged, "reconciled to success") {
		t.Errorf("a non-owner membership row was reported as success; the user cannot administer their "+
			"own auto-created workspace. log: %s", logged)
	}
	if !strings.Contains(logged, "found_role=viewer") {
		t.Errorf("the log does not name the role actually found, so on-call cannot see what happened. log: %s", logged)
	}
}
