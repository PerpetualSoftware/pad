package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3352: the invitation EMAIL carries a mailbox-only proof next to the
// join code. Spending it on register or accept verifies the address; the
// code, which the inviter also sees, never does (BUG-3348). The proof never
// appears anywhere the inviter can read it.

var proofRe = regexp.MustCompile(`#proof=([0-9a-f]{32})`)

// inviteByEmail invites addr through the HTTP door as the admin and returns
// the response body, the join code and the proof from the captured email.
func inviteByEmail(t *testing.T, srv *Server, mails chan capturedMail, adminTok, wsSlug, addr string) (respBody, code, proof string) {
	t.Helper()
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/members/invite",
		map[string]string{"email": addr, "role": "editor"}, adminTok)
	if rr.Code != http.StatusCreated {
		t.Fatalf("invite: %d %s", rr.Code, rr.Body.String())
	}
	m := waitMail(t, mails)
	if m.to != addr {
		t.Fatalf("invitation mail went to %q", m.to)
	}
	got := proofRe.FindStringSubmatch(m.body)
	if got == nil {
		t.Fatalf("the invitation email carries no proof: %q", m.body)
	}
	codeRe := regexp.MustCompile(`/join/([0-9a-f]{32})`)
	c := codeRe.FindStringSubmatch(m.body)
	if c == nil {
		t.Fatalf("the invitation email carries no join code: %q", m.body)
	}
	return rr.Body.String(), c[1], got[1]
}

func newProofFixture(t *testing.T) (*Server, chan capturedMail, string, *models.Workspace) {
	t.Helper()
	srv := testServer(t)
	adminTok := bootstrapFirstUser(t, srv, "admin@pad.test", "Admin")
	sender, mails := newMailSink(t)
	srv.baseURL = "https://app.getpad.dev"
	srv.SetEmailSender(sender)
	srv.cloudMode = true
	admin, err := srv.store.GetUserByEmail("admin@pad.test")
	if err != nil || admin == nil {
		t.Fatalf("admin lookup: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Proof WS", OwnerID: admin.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, admin.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	return srv, mails, adminTok, ws
}

func registerInvited(t *testing.T, srv *Server, addr, code, proof string) {
	t.Helper()
	body := map[string]string{
		"email": addr, "name": "Invitee", "password": "correct-horse-battery-staple",
		"invitation_code": code,
	}
	if proof != "" {
		body["invitation_proof"] = proof
	}
	rr := doRequest(srv, "POST", "/api/v1/auth/register", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rr.Code, rr.Body.String())
	}
}

func verified(t *testing.T, srv *Server, addr string) bool {
	t.Helper()
	u, err := srv.store.GetUserByEmail(addr)
	if err != nil || u == nil {
		t.Fatalf("lookup %s: %v", addr, err)
	}
	return u.IsEmailVerified()
}

// The proof is in the email and nowhere the inviter reads: not the invite
// response, the members/settings list, nor the admin invitation list; not
// even as its hash.
func TestTASK3352_ProofReachesOnlyTheEmail(t *testing.T) {
	srv, mails, adminTok, ws := newProofFixture(t)
	resp, code, proof := inviteByEmail(t, srv, mails, adminTok, ws.Slug, "new@example.com")
	sum := sha256.Sum256([]byte(proof))
	hash := hex.EncodeToString(sum[:])

	if !strings.Contains(resp, code) {
		t.Fatalf("control: the invite response should carry the shareable code: %s", resp)
	}
	surfaces := map[string]string{"invite response": resp}
	for name, path := range map[string]string{
		"members list":      "/api/v1/workspaces/" + ws.Slug + "/members",
		"admin invitations": "/api/v1/admin/invitations",
	} {
		rr := doRequestWithCookie(srv, "GET", path, nil, adminTok)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
		surfaces[name] = rr.Body.String()
	}
	for name, body := range surfaces {
		if strings.Contains(body, proof) || strings.Contains(body, hash) {
			t.Errorf("%s exposes the mailbox proof", name)
		}
	}
}

func TestTASK3352_RegisterWithTheEmailedProofVerifies(t *testing.T) {
	t.Run("proof verifies, no separate verification email", func(t *testing.T) {
		srv, mails, adminTok, ws := newProofFixture(t)
		_, code, proof := inviteByEmail(t, srv, mails, adminTok, ws.Slug, "new@example.com")
		registerInvited(t, srv, "new@example.com", code, proof)
		if !verified(t, srv, "new@example.com") {
			t.Fatal("registering through the emailed link left the account unverified")
		}
		select {
		case m := <-mails:
			t.Errorf("a verification email was still sent: %q", m.subject)
		case <-time.After(300 * time.Millisecond):
		}
	})

	for _, tc := range []struct {
		name  string
		proof func(real string) string
	}{
		{"the code alone does not verify", func(string) string { return "" }},
		{"a wrong proof does not verify", func(string) string { return strings.Repeat("0", 32) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mails, adminTok, ws := newProofFixture(t)
			_, code, proof := inviteByEmail(t, srv, mails, adminTok, ws.Slug, "new@example.com")
			registerInvited(t, srv, "new@example.com", code, tc.proof(proof))
			if verified(t, srv, "new@example.com") {
				t.Fatal("the account was verified without the emailed proof")
			}
			extractVerifyToken(t, waitMail(t, mails)) // falls back to the verification link
		})
	}
}

func TestTASK3352_AcceptWithTheEmailedProofVerifies(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sendProof  func(real string) string
		wantVerify bool
	}{
		{"the emailed proof verifies", func(p string) string { return p }, true},
		{"the code alone does not", func(string) string { return "" }, false},
		{"a wrong proof does not", func(string) string { return strings.Repeat("a", 32) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mails, adminTok, ws := newProofFixture(t)
			u, err := srv.store.CreateUser(models.UserCreate{
				Email: "existing@example.com", Name: "Existing", Password: "correct-horse-battery-staple",
				Role: "member", Unverified: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			sess, err := srv.store.CreateSession(u.ID, "web", "192.0.2.1", testSessionUA, webSessionTTL)
			if err != nil {
				t.Fatal(err)
			}
			_, code, proof := inviteByEmail(t, srv, mails, adminTok, ws.Slug, "existing@example.com")
			var body any
			if p := tc.sendProof(proof); p != "" {
				body = map[string]string{"proof": p}
			}
			rr := doRequestWithCookie(srv, "POST", "/api/v1/invitations/"+code+"/accept", body, sess)
			if rr.Code != http.StatusOK {
				t.Fatalf("accept: %d %s", rr.Code, rr.Body.String())
			}
			if member, err := srv.store.IsWorkspaceMember(ws.ID, u.ID); err != nil || !member {
				t.Fatalf("accept did not grant membership: %v %v", member, err)
			}
			if got := verified(t, srv, "existing@example.com"); got != tc.wantVerify {
				t.Errorf("verified = %v, want %v", got, tc.wantVerify)
			}
		})
	}
}

// The proof is single-use and bound to its invitation, and a disabled
// account is never verified by it.
func TestTASK3352_ConsumeInvitationProofRules(t *testing.T) {
	srv, _, _, ws := newProofFixture(t)
	admin, _ := srv.store.GetUserByEmail("admin@pad.test")
	mk := func(addr string) *models.User {
		t.Helper()
		u, err := srv.store.CreateUser(models.UserCreate{Email: addr, Name: addr, Password: "correct-horse-battery-staple", Role: "member", Unverified: true})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	a, b := mk("a@example.com"), mk("b@example.com")
	invA, err := srv.store.CreateInvitation(ws.ID, "a@example.com", "editor", admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	invB, err := srv.store.CreateInvitation(ws.ID, "b@example.com", "editor", admin.ID)
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := srv.store.ConsumeInvitationProof(invB.ID, b.ID, invA.Proof); err != nil || ok {
		t.Errorf("another invitation's proof was accepted: %v %v", ok, err)
	}
	if ok, err := srv.store.ConsumeInvitationProof(invA.ID, a.ID, invA.Proof); err != nil || !ok {
		t.Fatalf("the right proof was refused: %v %v", ok, err)
	}
	if ok, err := srv.store.ConsumeInvitationProof(invA.ID, a.ID, invA.Proof); err != nil || ok {
		t.Errorf("a spent proof was accepted again: %v %v", ok, err)
	}

	// An invitation past its expiry: its proof verifies nothing, however
	// the request got here.
	c := mk("c@example.com")
	invC, err := srv.store.CreateInvitation(ws.ID, "c@example.com", "editor", admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(`UPDATE workspace_invitations SET expires_at = ? WHERE id = ?`, past, invC.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := srv.store.ConsumeInvitationProof(invC.ID, c.ID, invC.Proof); err != nil || ok {
		t.Errorf("an expired invitation's proof verified: %v %v", ok, err)
	}
	// An empty expiry reads as expired (IsExpired), so it verifies nothing.
	if _, err := srv.store.DB().Exec(`UPDATE workspace_invitations SET expires_at = '' WHERE id = ?`, invC.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := srv.store.ConsumeInvitationProof(invC.ID, c.ID, invC.Proof); err != nil || ok {
		t.Errorf("an invitation with an empty expiry verified: %v %v", ok, err)
	}

	if err := srv.store.DisableUser(b.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := srv.store.ConsumeInvitationProof(invB.ID, b.ID, invB.Proof); err != nil || ok {
		t.Errorf("a disabled account was verified: %v %v", ok, err)
	}
	if got, _ := srv.store.GetUser(b.ID); got.IsEmailVerified() {
		t.Error("the disabled account reads verified")
	}
}
