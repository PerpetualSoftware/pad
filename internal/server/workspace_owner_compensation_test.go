package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-2715 — the owner membership is FATAL to a workspace create, and the
// compensation reconciles before it destroys anything.
//
// Four doors created a workspace and then discarded AddWorkspaceMember's error,
// leaving a workspace that exists, is seeded, and that nobody can administer —
// reported to the caller as a 201. These legs drive the create door with a
// store fault injected on the member write, one leg per arm of the
// compensation, because the arms differ in what they do to the workspace and
// getting that wrong in either direction is its own defect (BUG-3026 destroyed
// a workspace whose membership had actually committed).
//
// The fault seam is BUG-3026's: SetAddWorkspaceMemberCommitHookForTesting
// routes the COMMIT, so a leg can make the write fail, or make it LAND and
// report failure, which is the case the naive "delete on error" gets wrong.

var errSimOwnerAddFailed = errors.New("simulated owner-add failure")
var errSimMembershipUnreadable = errors.New("simulated membership read failure")

func compensationServer(t *testing.T) (*Server, string) {
	t.Helper()
	srv := New(storetest.NewSQLite(t))
	t.Cleanup(func() { srv.Stop() })
	u, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// A real session, so these legs go through the whole middleware chain —
	// auth, CSRF and all — rather than past it. A leg that reaches the handler
	// by a route production never uses would not be evidence about the door.
	token, err := srv.store.CreateSession(u.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return srv, token
}

// createWorkspaceAs drives the real create door through the router, so these
// legs exercise the handler rather than the helper in isolation.
func createWorkspaceAs(t *testing.T, srv *Server, token, name string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": name}, token)
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func workspaceCount(t *testing.T, srv *Server) int {
	t.Helper()
	ws, err := srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	return len(ws)
}

// ABSENT — the member write genuinely did not land. The workspace really is
// unreachable, so deleting it is correct and the door must refuse.
func TestCreateWorkspace_OwnerAddFails_RefusesAndLeavesNoWorkspace(t *testing.T) {
	srv, token := compensationServer(t)
	before := workspaceCount(t, srv)

	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		// Roll back rather than commit: the row never lands, which is what
		// makes this the ABSENT arm rather than the ack-loss one.
		_ = tx.Rollback()
		return errSimOwnerAddFailed
	})
	t.Cleanup(restore)

	rr := createWorkspaceAs(t, srv, token, "Doomed")

	if rr.Code < 500 {
		t.Fatalf("status %d, want 5xx: a workspace nobody can administer must not be reported as created", rr.Code)
	}
	if got := workspaceCount(t, srv); got != before {
		t.Fatalf("workspace count %d -> %d, want unchanged: the compensating delete did not run", before, got)
	}
}

// ACK LOSS — the commit LANDED and reported an error anyway. Deleting here is
// the BUG-3026 defect; the workspace is usable, so the door must succeed.
func TestCreateWorkspace_OwnerAddAckLoss_ReconcilesToSuccess(t *testing.T) {
	srv, token := compensationServer(t)
	before := workspaceCount(t, srv)
	logs := captureLogs(t)

	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		if cerr := tx.Commit(); cerr != nil {
			return cerr
		}
		return errSimOwnerAddFailed // committed, then "failed"
	})
	t.Cleanup(restore)

	rr := createWorkspaceAs(t, srv, token, "Acklost")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201: the owner row committed, so the workspace is usable and deleting it "+
			"would be BUG-3026 all over again. body: %s", rr.Code, rr.Body.String())
	}
	if got := workspaceCount(t, srv); got != before+1 {
		t.Fatalf("workspace count %d -> %d, want one more: the workspace was destroyed despite a committed "+
			"owner row", before, got)
	}
	if !strings.Contains(logs.String(), "reconciled to success") {
		t.Errorf("the reconcile arm was not taken; without it this leg does not describe the ack-loss path. log: %s", logs)
	}
}

// UNREADABLE — the membership read failed too, so absence and ack-loss are
// indistinguishable. Keep the workspace (destroying on an unreadable state is
// the unrecoverable direction) and still refuse.
func TestCreateWorkspace_MembershipUnreadable_RefusesButKeepsTheWorkspace(t *testing.T) {
	srv, token := compensationServer(t)
	before := workspaceCount(t, srv)
	logs := captureLogs(t)

	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimOwnerAddFailed
	})
	t.Cleanup(restore)
	srv.membershipCheck = func(string, string) (*models.WorkspaceMember, error) {
		return nil, errSimMembershipUnreadable
	}
	t.Cleanup(func() { srv.membershipCheck = nil })

	rr := createWorkspaceAs(t, srv, token, "Unreadable")

	if rr.Code < 500 {
		t.Fatalf("status %d, want 5xx: the workspace cannot be claimed usable when its membership is unreadable", rr.Code)
	}
	if got := workspaceCount(t, srv); got != before+1 {
		t.Fatalf("workspace count %d -> %d, want one more: an unreadable membership must not authorise a "+
			"delete — an orphan is recoverable, a destroyed live workspace is not", before, got)
	}
	if !strings.Contains(logs.String(), "state is unknown") {
		t.Errorf("the unreadable arm was not taken. log: %s", logs)
	}
}

// WRONG ROLE — somebody has access but not as owner. Neither success nor
// deletion is honest.
func TestCreateWorkspace_MembershipWrongRole_RefusesButKeepsTheWorkspace(t *testing.T) {
	srv, token := compensationServer(t)
	before := workspaceCount(t, srv)
	logs := captureLogs(t)

	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimOwnerAddFailed
	})
	t.Cleanup(restore)
	srv.membershipCheck = func(string, string) (*models.WorkspaceMember, error) {
		return &models.WorkspaceMember{Role: "viewer"}, nil
	}
	t.Cleanup(func() { srv.membershipCheck = nil })

	rr := createWorkspaceAs(t, srv, token, "Wrongrole")

	if rr.Code < 500 {
		t.Fatalf("status %d, want 5xx: a viewer cannot administer the workspace they just created", rr.Code)
	}
	if got := workspaceCount(t, srv); got != before+1 {
		t.Fatalf("workspace count %d -> %d, want one more: somebody has access, so deleting is not safe", before, got)
	}
	if !strings.Contains(logs.String(), "different role") {
		t.Errorf("the wrong-role arm was not taken. log: %s", logs)
	}
}

// THE CONTROL LEG. With no fault injected the door behaves exactly as before:
// 201, one more workspace, and an owner row. Without this, every leg above
// could pass because creation was broken outright.
func TestCreateWorkspace_NoFault_StillCreatesWithAnOwner(t *testing.T) {
	srv, token := compensationServer(t)
	before := workspaceCount(t, srv)

	rr := createWorkspaceAs(t, srv, token, "Healthy")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	if got := workspaceCount(t, srv); got != before+1 {
		t.Fatalf("workspace count %d -> %d, want one more", before, got)
	}
	var ws models.Workspace
	if err := json.Unmarshal(rr.Body.Bytes(), &ws); err != nil {
		t.Fatalf("decode: %v", err)
	}
	user, err := srv.store.GetUserByEmail("owner@example.com")
	if err != nil || user == nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	member, err := srv.store.GetWorkspaceMember(ws.ID, user.ID)
	if err != nil {
		t.Fatalf("GetWorkspaceMember: %v", err)
	}
	if member == nil || member.Role != "owner" {
		t.Fatalf("owner row = %+v, want role owner", member)
	}
}

// BUG-2715, the signup door. The membership write's error was discarded and the
// invitation was consumed on the very next line, so a failure burned the code
// AND left the user with no access — unrecoverable without an admin, because an
// invitation cannot be redeemed twice.
//
// "Still redeemable" is measured with GetInvitationByCode, which filters
// `accepted_at IS NULL` — the same query the signup door uses to redeem one. So
// the assertion is not a proxy for redeemability; it is the thing itself.

func invitedSignupServer(t *testing.T) (*Server, *models.WorkspaceInvitation) {
	t.Helper()
	srv := New(storetest.NewSQLite(t))
	t.Cleanup(func() { srv.Stop() })

	inviter, err := srv.store.CreateUser(models.UserCreate{
		Email: "inviter@example.com", Name: "Inviter", Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatalf("CreateUser inviter: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Invited"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	inv, err := srv.store.CreateInvitation(ws.ID, "newcomer@example.com", "editor", inviter.ID)
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	return srv, inv
}

func TestInvitationSignup_MemberAddFails_RefusesAndLeavesTheInvitationRedeemable(t *testing.T) {
	srv, inv := invitedSignupServer(t)

	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimOwnerAddFailed
	})
	t.Cleanup(restore)

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":           "newcomer@example.com",
		"name":            "Newcomer",
		"password":        "correct-horse-battery-staple",
		"invitation_code": inv.Code,
	})

	if rr.Code < 500 {
		t.Fatalf("status %d, want 5xx: a signup that could not grant workspace access must not report success. body: %s",
			rr.Code, rr.Body.String())
	}

	still, err := srv.store.GetInvitationByCode(inv.Code)
	if err != nil {
		t.Fatalf("GetInvitationByCode: %v", err)
	}
	if still == nil {
		t.Fatal("the invitation was consumed by a signup that failed to create the membership — the code cannot " +
			"be redeemed twice, so this is the unrecoverable state the ordering exists to prevent")
	}
}

// CONTROL. Without a fault the same signup succeeds, the membership exists, and
// the invitation IS consumed. Without this leg the one above could pass because
// registration was broken outright and never reached the invitation at all.
func TestInvitationSignup_NoFault_GrantsMembershipAndConsumesTheInvitation(t *testing.T) {
	srv, inv := invitedSignupServer(t)

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":           "newcomer@example.com",
		"name":            "Newcomer",
		"password":        "correct-horse-battery-staple",
		"invitation_code": inv.Code,
	})
	if rr.Code >= 400 {
		t.Fatalf("status %d, want success. body: %s", rr.Code, rr.Body.String())
	}

	user, err := srv.store.GetUserByEmail("newcomer@example.com")
	if err != nil || user == nil {
		t.Fatalf("GetUserByEmail: %v (user=%v)", err, user)
	}
	member, err := srv.store.GetWorkspaceMember(inv.WorkspaceID, user.ID)
	if err != nil {
		t.Fatalf("GetWorkspaceMember: %v", err)
	}
	if member == nil || member.Role != "editor" {
		t.Fatalf("membership = %+v, want role editor", member)
	}
	consumed, err := srv.store.GetInvitationByCode(inv.Code)
	if err != nil {
		t.Fatalf("GetInvitationByCode: %v", err)
	}
	if consumed != nil {
		t.Fatal("the invitation is still redeemable after a SUCCESSFUL signup — it should have been accepted")
	}
}

// THE MaxOpenConns(1) QUESTION, SETTLED RATHER THAN DEFERRED (BUG-2715).
//
// The design discussion for this fix kept citing BUG-2778 and BUG-2409 — a pool
// read issued from inside a lock-holding transaction needs a SECOND connection,
// and when the pool is saturated by callers waiting on that very lock it never
// arrives: a deadlock no SQLSTATE names. That history was being used as a
// reason not to put seeding inside a transaction, which is fine as far as it
// goes; but it was also being carried, unexamined, as a hazard supposedly
// attaching to THIS change. So measure it rather than inherit it.
//
// What the compensation actually does: AddWorkspaceMember (its own
// transaction, opened and committed), then GetWorkspaceMember (a pool read),
// then possibly DeleteWorkspace (a pool exec). They are SEQUENTIAL — no read is
// issued while a transaction of this path holds a connection — so with
// MaxOpenConns(1) each takes the single connection in turn and gives it back.
// The prediction is therefore that this completes, and this leg is what turns
// that prediction into a result.
//
// IT CAN GO RED, which is what makes a green here worth anything: issuing the
// membership check on a connection held by AddWorkspaceMember's transaction —
// the shape BUG-2778 describes — hangs it until the timeout. That mutation is
// on the unit's trail with the matrix.
//
// WHAT THIS LEG DOES NOT COVER, stated so its green is not read wider than it
// is (codex round 2): it runs on SQLITE and drives the ABSENT arm only. It says
// nothing about Postgres connection behaviour, and nothing about the ack-loss
// arm, which reaches the same reads by a different route. The claim it supports
// is "this path's calls are sequential and complete under one connection",
// which is dialect-independent in the pool layer — not "the compensation is
// deadlock-free under every driver and every arm".
func TestOwnerCompensation_CompletesUnderASingleConnection(t *testing.T) {
	srv, token := compensationServer(t)

	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimOwnerAddFailed
	})
	t.Cleanup(restore)

	// From here the pool has exactly one connection. Anything in the
	// compensation that needs a second while holding the first deadlocks
	// deterministically instead of only under load.
	srv.store.DB().SetMaxOpenConns(1)

	before := workspaceCount(t, srv)
	logs := captureLogs(t)

	done := make(chan int, 1)
	go func() { done <- createWorkspaceAs(t, srv, token, "Single Conn").Code }()

	select {
	case code := <-done:
		// RETURNING is the point, but a 5xx alone would also be satisfied by a
		// failure BEFORE the compensation ever ran — validation, auth, the
		// create itself — which would make this leg green without exercising
		// the path it names (codex round 2). So pin that the compensation
		// actually happened: its own log line, and a workspace count back where
		// it started because the removal ran.
		if code < 500 {
			t.Fatalf("status %d, want 5xx: the door completed but stopped refusing", code)
		}
		if !strings.Contains(logs.String(), "removing the workspace nobody could administer") {
			t.Fatalf("the compensation did not run, so this leg says nothing about it under "+
				"MaxOpenConns(1). log: %s", logs)
		}
		if got := workspaceCount(t, srv); got != before {
			t.Fatalf("workspace count %d -> %d, want unchanged: the removal did not complete", before, got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the create door did not complete under MaxOpenConns(1): something in the owner compensation " +
			"issued a pool read while a transaction on this path held the only connection")
	}
}

// The three legs below cover behaviour added by codex round 1. Each exists
// because the round's finding named a state the original code reached and the
// tests could not see.

// ABSENT BUT NOT ALONE. Absence of THIS caller's owner row is not absence of
// every member — the two questions BUG-3026 separated. Deleting on the first
// would remove a workspace somebody else can reach.
func TestCreateWorkspace_OwnerAddFailsButOthersHaveAccess_KeepsTheWorkspace(t *testing.T) {
	srv, token := compensationServer(t)
	other, err := srv.store.CreateUser(models.UserCreate{
		Email: "other@example.com", Name: "Other", Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatalf("CreateUser other: %v", err)
	}
	logs := captureLogs(t)
	before := workspaceCount(t, srv)

	// Let the FIRST member write through (the unrelated user), then fail the
	// creator's. Ordering by call count, because both go through one seam.
	var n int
	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		n++
		if n == 1 {
			return tx.Commit()
		}
		_ = tx.Rollback()
		return errSimOwnerAddFailed
	})
	t.Cleanup(restore)

	// A workspace with a foreign member already on it, created outside the door.
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Shared"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, other.ID, "editor"); err != nil {
		t.Fatalf("seed foreign member: %v", err)
	}
	user, err := srv.store.GetUserByEmail("owner@example.com")
	if err != nil || user == nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	_ = token

	if err := srv.addOwnerOrCompensate("test", ws.ID, ws.Slug, user.ID); err == nil {
		t.Fatal("want an error: the owner row was never written, so the caller cannot administer this workspace")
	}
	if got := workspaceCount(t, srv); got != before+1 {
		t.Fatalf("workspace count %d -> %d, want the shared workspace still present: it was deleted despite "+
			"another member having access", before, got)
	}
	if !strings.Contains(logs.String(), "other members") {
		t.Errorf("the other-members arm was not taken. log: %s", logs)
	}
}

// NOT MERELY SOFT-DELETED. ListDeletedWorkspaces is scoped by owner_id, so a
// soft delete alone would surface the husk in the creator's restorable list —
// and restoring it hands back the ownerless workspace this refuses to create.
func TestCreateWorkspace_Compensation_LeavesNothingRestorable(t *testing.T) {
	srv, token := compensationServer(t)
	logs := captureLogs(t)
	before := workspaceCount(t, srv)
	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimOwnerAddFailed
	})
	t.Cleanup(restore)

	if rr := createWorkspaceAs(t, srv, token, "Gone"); rr.Code < 500 {
		t.Fatalf("status %d, want 5xx", rr.Code)
	}

	// PIN THAT COMPENSATION RAN (codex round 3). An empty deleted-list is also
	// what a 5xx BEFORE the workspace was ever created produces, and what a
	// failed DeleteWorkspace leaving the workspace LIVE produces — so the
	// absence below is only evidence once these two say the removal happened.
	if !strings.Contains(logs.String(), "removing the workspace nobody could administer") {
		t.Fatalf("the compensation never ran, so an empty deleted-list proves nothing. log: %s", logs)
	}
	if got := workspaceCount(t, srv); got != before {
		t.Fatalf("workspace count %d -> %d, want unchanged: the workspace is still LIVE, which also yields "+
			"an empty deleted-list", before, got)
	}

	user, err := srv.store.GetUserByEmail("owner@example.com")
	if err != nil || user == nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	deleted, err := srv.store.ListDeletedWorkspaces(user.ID, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("ListDeletedWorkspaces: %v", err)
	}
	for _, w := range deleted {
		if w.Name == "Gone" {
			t.Fatal("the compensated workspace is restorable: the creator sees a workspace they never " +
				"knowingly created, and restoring it returns the ownerless state the refusal prevented")
		}
	}
}

// THE ACCOUNT GOES BACK TOO. Leaving the invitation open is only half a
// recoverable state: the account persists holding the email, so the retry the
// refusal exists to permit meets a duplicate-email conflict instead.
func TestInvitationSignup_MemberAddFails_RollsTheAccountBack(t *testing.T) {
	srv, inv := invitedSignupServer(t)
	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimOwnerAddFailed
	})
	t.Cleanup(restore)

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":           "newcomer@example.com",
		"name":            "Newcomer",
		"password":        "correct-horse-battery-staple",
		"invitation_code": inv.Code,
	})
	if rr.Code < 500 {
		t.Fatalf("status %d, want 5xx. body: %s", rr.Code, rr.Body.String())
	}

	user, err := srv.store.GetUserByEmail("newcomer@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if user != nil {
		t.Fatal("the account survived a signup that granted no access — it holds the email, so the retry " +
			"with the still-open invitation meets a duplicate-email conflict and the user is stuck")
	}
}

// SIGNUP ACK LOSS — the membership committed and reported failure anyway.
// Deleting the account here destroys a signup that worked, which is the
// BUG-3026 shape arriving in the door forty lines from the helper written to
// prevent it. Added after codex round 2 found exactly that.
func TestInvitationSignup_MemberAddAckLoss_KeepsTheAccountAndSucceeds(t *testing.T) {
	srv, inv := invitedSignupServer(t)
	logs := captureLogs(t)

	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		if cerr := tx.Commit(); cerr != nil {
			return cerr
		}
		return errSimOwnerAddFailed // committed, then "failed"
	})
	t.Cleanup(restore)

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":           "newcomer@example.com",
		"name":            "Newcomer",
		"password":        "correct-horse-battery-staple",
		"invitation_code": inv.Code,
	})
	if rr.Code >= 400 {
		t.Fatalf("status %d, want success: the membership committed, so the signup worked. body: %s",
			rr.Code, rr.Body.String())
	}

	user, err := srv.store.GetUserByEmail("newcomer@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if user == nil {
		t.Fatal("the account was rolled back even though the membership row committed — a working signup destroyed")
	}
	member, err := srv.store.GetWorkspaceMember(inv.WorkspaceID, user.ID)
	if err != nil {
		t.Fatalf("GetWorkspaceMember: %v", err)
	}
	if member == nil {
		t.Fatal("no membership: the precondition for this leg did not hold, so it proves nothing")
	}
	if !strings.Contains(logs.String(), "reconciled to success") {
		t.Errorf("the reconcile arm was not taken. log: %s", logs)
	}
	// The signup SUCCEEDED, so the bookkeeping must have run too (codex round
	// 3). Without this, a regression that reconciles the membership and then
	// skips the accept passes here while leaving a redeemable code behind a
	// granted membership.
	consumed, err := srv.store.GetInvitationByCode(inv.Code)
	if err != nil {
		t.Fatalf("GetInvitationByCode: %v", err)
	}
	if consumed != nil {
		t.Fatal("the invitation is still redeemable after a signup that succeeded by reconciliation")
	}
}

// SIGNUP, MEMBERSHIP UNREADABLE — absence and ack-loss are indistinguishable,
// so the account must NOT be deleted on the ambiguity, and the door must still
// refuse.
func TestInvitationSignup_MembershipUnreadable_RefusesButKeepsTheAccount(t *testing.T) {
	srv, inv := invitedSignupServer(t)
	logs := captureLogs(t)

	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimOwnerAddFailed
	})
	t.Cleanup(restore)
	srv.membershipCheck = func(string, string) (*models.WorkspaceMember, error) {
		return nil, errSimMembershipUnreadable
	}
	t.Cleanup(func() { srv.membershipCheck = nil })

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":           "newcomer@example.com",
		"name":            "Newcomer",
		"password":        "correct-horse-battery-staple",
		"invitation_code": inv.Code,
	})
	if rr.Code < 500 {
		t.Fatalf("status %d, want 5xx", rr.Code)
	}

	user, err := srv.store.GetUserByEmail("newcomer@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if user == nil {
		t.Fatal("the account was deleted on an unreadable membership state — the membership may exist, and " +
			"deleting on an unknown state is the unrecoverable direction")
	}
	if !strings.Contains(logs.String(), "state is unknown") {
		t.Errorf("the unreadable arm was not taken. log: %s", logs)
	}
	// The refusal's whole promise is that the code survives it (codex round 3).
	// Asserting only that the account survives would still pass if the accept
	// were moved ahead of the reconcile, which is the one ordering this door
	// exists to forbid.
	still, err := srv.store.GetInvitationByCode(inv.Code)
	if err != nil {
		t.Fatalf("GetInvitationByCode: %v", err)
	}
	if still == nil {
		t.Fatal("the invitation was consumed by a signup that refused — the code cannot be redeemed twice, " +
			"so the refusal left nothing to retry with")
	}
}

// SEED FAILURE IS THE SAME HUSK, one step earlier (codex round 4). Compensating
// the owner step alone left the invariant true of one failure point and false of
// its neighbour: the old handler returned 500 saying "Workspace created but
// failed to seed collections" and kept a live, ownerless, unseeded workspace
// holding its slug.
func TestCreateWorkspace_SeedFails_RefusesAndLeavesNoWorkspace(t *testing.T) {
	srv, token := compensationServer(t)
	logs := captureLogs(t)
	before := workspaceCount(t, srv)

	// An unknown template is the seed step's own refusal, so this drives the
	// real failure path rather than a simulated one.
	body := map[string]string{"name": "Unseedable", "template": "no-such-template-exists"}
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", body, token)

	if rr.Code < 500 {
		t.Fatalf("status %d, want 5xx. body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(logs.String(), "removing the workspace nobody could administer") {
		t.Fatalf("the seed failure did not compensate, so the count below proves nothing. log: %s", logs)
	}
	if got := workspaceCount(t, srv); got != before {
		t.Fatalf("workspace count %d -> %d, want unchanged: a seed failure left a live ownerless workspace "+
			"holding its slug", before, got)
	}
}

// THE SLUG MUST COME BACK, on every compensated arm (BUG-2715, ruling (d)).
//
// The comments in the helper claim the removal frees the name — uniqueWorkspaceSlug
// filters `deleted_at IS NULL`, and the purge removes the row outright — so a
// caller's retry reclaims it. That was an ASSERTION, not a measurement, and it
// is the same claim BUG-2892 had to fix for imports when it turned out false:
// a husk that keeps its slug sends the retry to `name-2`, which is a worse
// outcome than the failure, because the user asked for a name and silently got
// a different one.
//
// One leg per compensated arm. The failure mode they guard is specific: a
// removal that purges the DATA but leaves the row holding the name would pass
// every other leg in this file.

func TestCreateWorkspace_OwnerAddCompensation_FreesTheSlugForARetry(t *testing.T) {
	srv, token := compensationServer(t)

	restore := srv.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimOwnerAddFailed
	})
	if rr := createWorkspaceAs(t, srv, token, "Reclaim Me"); rr.Code < 500 {
		restore()
		t.Fatalf("status %d, want 5xx: the precondition is a COMPENSATED create", rr.Code)
	}
	restore() // the retry must be allowed to succeed

	rr := createWorkspaceAs(t, srv, token, "Reclaim Me")
	if rr.Code != http.StatusCreated {
		t.Fatalf("retry status %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	if err := json.Unmarshal(rr.Body.Bytes(), &ws); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ws.Slug != "reclaim-me" {
		t.Fatalf("retry got slug %q, want %q: the compensated workspace is still holding the name, so the "+
			"user asked for one thing and silently received another", ws.Slug, "reclaim-me")
	}
}

func TestCreateWorkspace_SeedFailureCompensation_FreesTheSlugForARetry(t *testing.T) {
	srv, token := compensationServer(t)

	// The seed arm, driven by its own real refusal rather than a simulated one.
	bad := map[string]string{"name": "Seed Reclaim", "template": "no-such-template-exists"}
	if rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", bad, token); rr.Code < 500 {
		t.Fatalf("status %d, want 5xx: the precondition is a COMPENSATED seed failure", rr.Code)
	}

	rr := createWorkspaceAs(t, srv, token, "Seed Reclaim")
	if rr.Code != http.StatusCreated {
		t.Fatalf("retry status %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	if err := json.Unmarshal(rr.Body.Bytes(), &ws); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ws.Slug != "seed-reclaim" {
		t.Fatalf("retry got slug %q, want %q: the seed-compensated workspace is still holding the name",
			ws.Slug, "seed-reclaim")
	}
}
