package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3098: members_per_workspace was checked only when an invitation was
// SENT, and pending invitations are not members, so accepting them took a
// workspace past its cap one request at a time. Both accept doors must now
// refuse at the cap and leave the invitation pending so it can be accepted
// once there is room. Each refused leg has an under-cap control (CONVE-12).

type acceptLimitEnv struct {
	*planLimitEnv
}

func newAcceptLimitEnv(t *testing.T) *acceptLimitEnv {
	t.Helper()
	return &acceptLimitEnv{planLimitEnv: newPlanLimitEnv(t)}
}

func (e *acceptLimitEnv) members(t *testing.T) int {
	t.Helper()
	return e.countIn(t, `SELECT COUNT(*) FROM workspace_members WHERE workspace_id = ?`)
}

func (e *acceptLimitEnv) setMemberCap(t *testing.T, limit int) {
	t.Helper()
	if err := e.srv.store.SetUserPlanOverrides(e.user.ID, fmt.Sprintf(`{"members_per_workspace":%d}`, limit)); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
}

func (e *acceptLimitEnv) invite(t *testing.T, email string) *models.WorkspaceInvitation {
	t.Helper()
	inv, err := e.srv.store.CreateInvitation(e.home.ID, email, "editor", e.user.ID)
	if err != nil {
		t.Fatalf("CreateInvitation(%s): %v", email, err)
	}
	return inv
}

func (e *acceptLimitEnv) stillPending(t *testing.T, inv *models.WorkspaceInvitation) bool {
	t.Helper()
	got, err := e.srv.store.GetInvitationByCode(inv.Code)
	if err != nil {
		t.Fatalf("GetInvitationByCode: %v", err)
	}
	return got != nil
}

// acceptAs creates an existing account for email, signs it in, and accepts
// inv through the authenticated door (A1).
func (e *acceptLimitEnv) acceptAs(t *testing.T, email string, inv *models.WorkspaceInvitation) *httptest.ResponseRecorder {
	t.Helper()
	u, err := e.srv.store.CreateUser(models.UserCreate{Email: email, Name: "Invitee", Password: "pw-invitee-12345"})
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", email, err)
	}
	tok, err := e.srv.store.CreateSession(u.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return doRequestWithCookie(e.srv, "POST", "/api/v1/invitations/"+inv.Code+"/accept", nil, tok)
}

// register signs up email with inv's code (A2).
func (e *acceptLimitEnv) register(email string, inv *models.WorkspaceInvitation) *httptest.ResponseRecorder {
	return doRequest(e.srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":           email,
		"name":            "New Invitee",
		"password":        "correct-horse-battery-staple",
		"invitation_code": inv.Code,
	})
}

func (e *acceptLimitEnv) accountExists(t *testing.T, email string) bool {
	t.Helper()
	u, err := e.srv.store.GetUserByEmail(email)
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	return u != nil
}

// --- A1: authenticated accept ---

func TestAcceptLimit_Existing_AtCap_RefusedAndPending(t *testing.T) {
	e := newAcceptLimitEnv(t)
	inv := e.invite(t, "existing@example.com")
	limit := e.members(t)
	e.setMemberCap(t, limit)

	rr := e.acceptAs(t, "existing@example.com", inv)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := e.members(t); got != limit {
		t.Errorf("members = %d under a cap of %d, want the cap", got, limit)
	}
	if !e.stillPending(t, inv) {
		t.Error("the refused invitation was consumed; it must stay pending")
	}
}

func TestAcceptLimit_Existing_UnderCap_Admitted(t *testing.T) {
	e := newAcceptLimitEnv(t)
	inv := e.invite(t, "existing@example.com")
	limit := e.members(t) + 1
	e.setMemberCap(t, limit)

	rr := e.acceptAs(t, "existing@example.com", inv)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := e.members(t); got != limit {
		t.Errorf("members = %d, want exactly the cap %d", got, limit)
	}
	if e.stillPending(t, inv) {
		t.Error("an accepted invitation is still pending")
	}
}

// --- A2: register with an invitation ---

func TestAcceptLimit_Register_AtCap_RefusedNoAccountPending(t *testing.T) {
	e := newAcceptLimitEnv(t)
	inv := e.invite(t, "newcomer@example.com")
	limit := e.members(t)
	e.setMemberCap(t, limit)

	rr := e.register("newcomer@example.com", inv)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := e.members(t); got != limit {
		t.Errorf("members = %d under a cap of %d, want the cap", got, limit)
	}
	if !e.stillPending(t, inv) {
		t.Error("the refused invitation was consumed; it must stay pending")
	}
	if e.accountExists(t, "newcomer@example.com") {
		t.Error("a refused registration left an account behind; the retry would meet a duplicate email")
	}
}

func TestAcceptLimit_Register_UnderCap_Admitted(t *testing.T) {
	e := newAcceptLimitEnv(t)
	inv := e.invite(t, "newcomer@example.com")
	limit := e.members(t) + 1
	e.setMemberCap(t, limit)

	rr := e.register("newcomer@example.com", inv)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := e.members(t); got != limit {
		t.Errorf("members = %d, want exactly the cap %d", got, limit)
	}
	if e.stillPending(t, inv) {
		t.Error("an accepted invitation is still pending")
	}
}

// --- self-hosted: no cap applies at either door ---

func TestAcceptLimit_SelfHosted_NotEnforced(t *testing.T) {
	for _, door := range []string{"existing", "register"} {
		t.Run(door, func(t *testing.T) {
			e := newAcceptLimitEnv(t)
			e.srv.cloudMode = false
			inv := e.invite(t, door+"@example.com")
			current := e.members(t)
			e.setMemberCap(t, current)

			var rr *httptest.ResponseRecorder
			want := http.StatusOK
			if door == "existing" {
				rr = e.acceptAs(t, door+"@example.com", inv)
			} else {
				rr = e.register(door+"@example.com", inv)
				want = http.StatusCreated
			}
			if rr.Code != want {
				t.Fatalf("status = %d, want %d (body=%s)", rr.Code, want, rr.Body.String())
			}
			if got := e.members(t); got != current+1 {
				t.Errorf("members = %d, want %d", got, current+1)
			}
		})
	}
}

// --- the invitee's refusal: its own code, the owner named, no upgrade link ---

// mustBeMemberLimitRefusal asserts the invitee-facing refusal (BUG-3098
// ruling 1): 403 workspace_member_limit, a message naming the owner by
// display name only, and details without upgrade_url.
func (e *acceptLimitEnv) mustBeMemberLimitRefusal(t *testing.T, rr *httptest.ResponseRecorder, limit int, ownerPhrase string) {
	t.Helper()
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	if env.Error.Code != "workspace_member_limit" {
		t.Errorf("code = %q, want workspace_member_limit (body=%s)", env.Error.Code, rr.Body.String())
	}
	if !strings.Contains(env.Error.Message, ownerPhrase) {
		t.Errorf("message %q does not contain %q", env.Error.Message, ownerPhrase)
	}
	if strings.Contains(env.Error.Message, e.user.Email) {
		t.Errorf("message %q discloses the owner's email", env.Error.Message)
	}
	if strings.Contains(env.Error.Message, "free plan") || strings.Contains(env.Error.Message, "You've reached") {
		t.Errorf("message %q is the owner-addressed plan-limit text", env.Error.Message)
	}
	d := env.Error.Details
	if d["feature"] != "members_per_workspace" || d["limit"] != float64(limit) || d["current"] != float64(limit) {
		t.Errorf("details = %v, want members_per_workspace at %d of %d", d, limit, limit)
	}
	if _, has := d["upgrade_url"]; has {
		t.Errorf("details carry upgrade_url, which the invitee cannot use: %v", d)
	}
}

func (e *acceptLimitEnv) setOwnerName(t *testing.T, name string) {
	t.Helper()
	if _, err := e.srv.store.DB().Exec(e.srv.store.D().Rebind(`UPDATE users SET name = ? WHERE id = ?`), name, e.user.ID); err != nil {
		t.Fatalf("set owner name: %v", err)
	}
}

func TestAcceptLimit_Refusal_NamesOwnerByDisplayName(t *testing.T) {
	for _, door := range []string{"existing", "register"} {
		t.Run(door, func(t *testing.T) {
			e := newAcceptLimitEnv(t)
			e.setOwnerName(t, "Olive Owner")
			inv := e.invite(t, door+"@example.com")
			limit := e.members(t)
			e.setMemberCap(t, limit)

			var rr *httptest.ResponseRecorder
			if door == "existing" {
				rr = e.acceptAs(t, door+"@example.com", inv)
			} else {
				rr = e.register(door+"@example.com", inv)
			}
			e.mustBeMemberLimitRefusal(t, rr, limit, "the workspace owner, Olive Owner,")
		})
	}
}

func TestAcceptLimit_Refusal_OwnerWithoutName(t *testing.T) {
	e := newAcceptLimitEnv(t)
	e.setOwnerName(t, "   ")
	inv := e.invite(t, "existing@example.com")
	limit := e.members(t)
	e.setMemberCap(t, limit)

	rr := e.acceptAs(t, "existing@example.com", inv)
	e.mustBeMemberLimitRefusal(t, rr, limit, "Ask the workspace owner to make room")
}

// A2's store refusal on its own: a member lands between the pre-check and the
// membership write. The account the request created must be rolled back and
// the invitation left pending, and the answer is the invitee refusal, not 500.
func TestAcceptLimit_Register_CompetingAddInWindow_RolledBack(t *testing.T) {
	e := newAcceptLimitEnv(t)
	inv := e.invite(t, "racer@example.com")
	limit := e.members(t) + 1
	e.setMemberCap(t, limit)
	rival, err := e.srv.store.CreateUser(models.UserCreate{Email: "rival@example.com", Name: "Rival", Password: "pw-rival-12345"})
	if err != nil {
		t.Fatalf("CreateUser(rival): %v", err)
	}

	ran := false
	e.srv.planLimitAdmittedHook = func(feature, scope string) {
		if feature != "members_per_workspace" || scope != e.home.ID {
			return
		}
		ran = true
		if err := e.srv.store.AddWorkspaceMember(e.home.ID, rival.ID, "editor"); err != nil {
			t.Errorf("competing AddWorkspaceMember: %v", err)
		}
	}

	rr := e.register("racer@example.com", inv)
	if !ran {
		t.Fatal("planLimitAdmittedHook never ran on the register door; the leg measured nothing")
	}
	e.mustBeMemberLimitRefusal(t, rr, limit, "Ask the workspace owner")
	if got := e.members(t); got != limit {
		t.Errorf("members = %d under a cap of %d, want the cap", got, limit)
	}
	if e.accountExists(t, "racer@example.com") {
		t.Error("the refused registration's account was not rolled back")
	}
	if !e.stillPending(t, inv) {
		t.Error("the refused invitation was consumed; it must stay pending")
	}
}
