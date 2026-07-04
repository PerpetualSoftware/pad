package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/email"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-1933 Wave 3b (TASK-1938) — cloud email self-registration + the
// verify-email / resend-verification endpoints + invite-accept verification.
//
// These tests exercise the full behavioral contract from DR-6 (register
// relaxation), DR-5 (verify/resend endpoints, enumeration-safe), DR-11
// (duplicate-email 409), and DR-1 (invite-accept verifies), plus the
// load-bearing session-freshness invariant: a user who verifies in a live
// session is immediately unblocked under RequireVerifiedEmail (Wave 3a).

// capturedMail is one email the fake Maileroo endpoint received.
type capturedMail struct {
	to      string
	subject string
	body    string // html + plain concatenated, for token/URL extraction
}

// newMailSink stands up a fake Maileroo v2 endpoint that records every
// outgoing email onto a channel, and returns a Sender wired to it. Mirrors
// the httptest pattern in internal/email/sender_test.go.
func newMailSink(t *testing.T) (*email.Sender, chan capturedMail) {
	t.Helper()
	ch := make(chan capturedMail, 8)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			To []struct {
				Address string `json:"address"`
			} `json:"to"`
			Subject string `json:"subject"`
			HTML    string `json:"html"`
			Plain   string `json:"plain"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		to := ""
		if len(p.To) > 0 {
			to = p.To[0].Address
		}
		ch <- capturedMail{to: to, subject: p.Subject, body: p.HTML + "\n" + p.Plain}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"sent"}`))
	}))
	t.Cleanup(ts.Close)

	sender := email.NewSender("test-api-key", "noreply@pad.test", "Pad", "http://pad.test")
	sender.SetEndpoint(ts.URL)
	return sender, ch
}

// waitMail blocks for the next captured email or fails on timeout. Emails are
// sent from a goAsync goroutine, so the caller can't assume synchronous
// delivery.
func waitMail(t *testing.T, ch chan capturedMail) capturedMail {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for outgoing email")
		return capturedMail{}
	}
}

var verifyTokenRe = regexp.MustCompile(`padver_[0-9a-f]{64}`)

// extractVerifyToken pulls the padver_ token out of a verification email body.
func extractVerifyToken(t *testing.T, m capturedMail) string {
	t.Helper()
	tok := verifyTokenRe.FindString(m.body)
	if tok == "" {
		t.Fatalf("no padver_ token found in email body: %q", m.body)
	}
	return tok
}

// newCloudEmailServer returns a cloud-mode server with a usable base URL and a
// capturing email sink, bootstrapped with one admin. This is the configuration
// where self-serve signup is permitted (DR-6).
func newCloudEmailServer(t *testing.T) (*Server, chan capturedMail) {
	t.Helper()
	srv := testServer(t)
	// Bootstrap the admin BEFORE cloud mode (bootstrap is disabled in cloud).
	bootstrapFirstUser(t, srv, "admin@pad.test", "Admin")
	sender, mails := newMailSink(t)
	srv.baseURL = "https://app.getpad.dev"
	srv.SetEmailSender(sender)
	srv.cloudMode = true
	return srv, mails
}

// ---------------------------------------------------------------------
// DR-6 + DR-5 + Wave 3a: cloud signup → unverified → verify → unblocked.
// ---------------------------------------------------------------------

func TestCloudSignup_UnverifiedThenVerifyUnblocksSameSession(t *testing.T) {
	srv, mails := newCloudEmailServer(t)

	admin, err := srv.store.GetUserByEmail("admin@pad.test")
	if err != nil || admin == nil {
		t.Fatalf("GetUserByEmail admin: %v", err)
	}
	// A workspace + collection owned by the admin so the cloud-mode
	// item-create plan check can resolve the owner's plan (mirrors
	// middleware_verified_email_test.go).
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Signup WS", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	// Self-serve register (no invitation, no admin session, from a public IP).
	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "newbie@pad.test",
		"name":     "Newbie",
		"password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("self-serve register: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var reg struct {
		Token string `json:"token"`
		User  struct {
			EmailVerified bool `json:"email_verified"`
		} `json:"user"`
	}
	parseJSON(t, rr, &reg)
	if reg.Token == "" {
		t.Fatal("expected a session token from register")
	}
	if reg.User.EmailVerified {
		t.Fatal("a self-serve signup must be UNVERIFIED in the response payload")
	}

	// The persisted row is unverified.
	u, err := srv.store.GetUserByEmail("newbie@pad.test")
	if err != nil || u == nil {
		t.Fatalf("GetUserByEmail newbie: %v", err)
	}
	if u.IsEmailVerified() {
		t.Fatal("self-serve signup persisted as verified — DR-3 says only this path writes NULL")
	}

	// Make the user an editor so item-create passes workspace access.
	if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}

	// A verification email was sent to the right address; grab its token.
	mail := waitMail(t, mails)
	if mail.to != "newbie@pad.test" {
		t.Fatalf("verification email to=%q, want newbie@pad.test", mail.to)
	}
	token := extractVerifyToken(t, mail)

	itemsPath := "/api/v1/workspaces/" + ws.Slug + "/collections/" + coll.Slug + "/items"

	// Before verification: a content mutation is blocked (Wave 3a live).
	rr = doRequestWithCookie(srv, "POST", itemsPath, map[string]any{"title": "blocked"}, reg.Token)
	if rr.Code != http.StatusForbidden || veErrorCode(rr) != "email_not_verified" {
		t.Fatalf("pre-verify item-create: expected 403 email_not_verified, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify the email.
	rr = doRequest(srv, "POST", "/api/v1/auth/verify-email", map[string]string{"token": token})
	if rr.Code != http.StatusOK {
		t.Fatalf("verify-email: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var vr struct {
		User struct {
			EmailVerified bool `json:"email_verified"`
		} `json:"user"`
	}
	parseJSON(t, rr, &vr)
	if !vr.User.EmailVerified {
		t.Fatal("verify-email response should report email_verified=true")
	}

	// SAME session token now performs the mutation — the DB flip is
	// immediately visible because ValidateSession re-reads the user per
	// request (no session rewrite needed).
	rr = doRequestWithCookie(srv, "POST", itemsPath, map[string]any{"title": "unblocked"}, reg.Token)
	if rr.Code != http.StatusCreated {
		t.Fatalf("post-verify item-create (same session): expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------
// DR-11: duplicate email at open signup keeps the clear 409.
// ---------------------------------------------------------------------

func TestCloudSignup_DuplicateEmail_Returns409(t *testing.T) {
	srv, mails := newCloudEmailServer(t)

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email": "dup@pad.test", "name": "Dup", "password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("first register: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	_ = waitMail(t, mails) // drain the verification email

	rr = doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email": "dup@pad.test", "name": "Dup Again", "password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate register: expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------
// DR-5: resend is enumeration-safe (always 200) and invalidates the prior
// token.
// ---------------------------------------------------------------------

func TestResendVerification_InvalidatesPriorTokenAndIsEnumerationSafe(t *testing.T) {
	srv, mails := newCloudEmailServer(t)

	// The PasswordReset limiter (burst 3, shared by verify/resend) is keyed by
	// IP; give each of these calls its own source IP so the test's several
	// verify/resend hits don't trip the limiter.
	ipN := 0
	post := func(path string, body any) *httptest.ResponseRecorder {
		ipN++
		return doRequestFromRemoteAddr(srv, "POST", path, body, "203.0.113."+strconv.Itoa(ipN)+":1234")
	}

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email": "resend@pad.test", "name": "Resend", "password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	firstToken := extractVerifyToken(t, waitMail(t, mails))

	// Resend → always 200, and a NEW token lands in the inbox.
	rr = post("/api/v1/auth/resend-verification", map[string]string{"email": "resend@pad.test"})
	if rr.Code != http.StatusOK {
		t.Fatalf("resend: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	secondToken := extractVerifyToken(t, waitMail(t, mails))
	if secondToken == firstToken {
		t.Fatal("resend should mint a fresh token")
	}

	// The prior token is now invalidated.
	rr = post("/api/v1/auth/verify-email", map[string]string{"token": firstToken})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("stale first token: expected 400 (invalidated by resend), got %d: %s", rr.Code, rr.Body.String())
	}

	// The new token still verifies.
	rr = post("/api/v1/auth/verify-email", map[string]string{"token": secondToken})
	if rr.Code != http.StatusOK {
		t.Fatalf("new token verify: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Enumeration safety: unknown email, now-verified account, and the admin
	// (verified) all still return 200 with no distinguishing signal.
	for _, addr := range []string{"nobody@pad.test", "resend@pad.test", "admin@pad.test"} {
		rr = post("/api/v1/auth/resend-verification", map[string]string{"email": addr})
		if rr.Code != http.StatusOK {
			t.Fatalf("resend %s: expected 200 (enumeration-safe), got %d: %s", addr, rr.Code, rr.Body.String())
		}
	}
}

// ---------------------------------------------------------------------
// DR-6 negative: self-host register stays admin/invitation-only.
// ---------------------------------------------------------------------

func TestSelfHostRegister_StaysRestricted(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@pad.test", "Admin")
	// Email fully configured, but NOT cloud mode.
	sender, _ := newMailSink(t)
	srv.baseURL = "https://app.getpad.dev"
	srv.SetEmailSender(sender)

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email": "selfhost@pad.test", "name": "SelfHost", "password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("self-host self-serve register: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if u, _ := srv.store.GetUserByEmail("selfhost@pad.test"); u != nil {
		t.Fatal("self-host register must not create a user")
	}
}

// ---------------------------------------------------------------------
// DR-6 guard: cloud WITHOUT a usable emailed-link path stays CLOSED, so no
// user is ever created who could never verify.
// ---------------------------------------------------------------------

func TestCloudRegister_UnverifiableConfig_StaysClosed(t *testing.T) {
	// (a) Cloud, usable base URL, but NO email sender wired.
	t.Run("no email sender", func(t *testing.T) {
		srv := testServer(t)
		bootstrapFirstUser(t, srv, "admin@pad.test", "Admin")
		srv.cloudMode = true
		srv.baseURL = "https://app.getpad.dev" // usable, but s.email == nil
		rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
			"email": "noemail@pad.test", "name": "NoEmail", "password": "correct-horse-battery-staple",
		})
		if rr.Code != http.StatusForbidden {
			t.Fatalf("cloud no-email register: expected 403, got %d: %s", rr.Code, rr.Body.String())
		}
		if u, _ := srv.store.GetUserByEmail("noemail@pad.test"); u != nil {
			t.Fatal("no unverifiable user should be created when email is unconfigured")
		}
	})

	// (b) Cloud, email sender wired, but the base URL is not reachable by an
	// external recipient — an emailed verification link would be undeliverable.
	// emailConfigured() must be false across the whole unusable-host set so no
	// write-locked user is ever minted.
	t.Run("unusable base URLs disable emailConfigured", func(t *testing.T) {
		srv := testServer(t)
		sender, _ := newMailSink(t)
		srv.SetEmailSender(sender)
		srv.cloudMode = true
		for _, base := range []string{
			// Literal IPs are rejected WHOLESALE (a public verification
			// endpoint is a hostname) — loopback, private, reserved, AND even
			// an ordinary public IP like 8.8.8.8 all disqualify self-serve.
			"http://0.0.0.0:7777",    // unspecified bind-all
			"http://[::]:7777",       // unspecified IPv6
			"http://127.0.0.1:777",   // loopback IP
			"https://[::1]",          // loopback IPv6
			"http://10.1.2.3",        // RFC1918 private
			"http://192.168.0.5",     // RFC1918 private
			"http://169.254.1.1",     // link-local
			"http://100.64.0.1",      // CGNAT (RFC 6598)
			"http://192.0.2.1",       // TEST-NET-1
			"http://192.88.99.1",     // 6to4 relay anycast
			"http://0.1.2.3",         // 0.0.0.0/8 "this network"
			"http://255.255.255.255", // broadcast
			"http://[2001:2::1]",     // IPv6 benchmarking
			"http://8.8.8.8",         // ordinary public IP — still a literal, rejected
			// Non-public / malformed hostnames.
			"http://localhost:777",            // loopback name
			"http://pad.internal",             // special-use TLD (.internal)
			"http://svc.local",                // special-use TLD (.local)
			"https://example.invalid",         // special-use TLD (.invalid)
			"http://app.test",                 // special-use TLD (.test)
			"http://pad.home.arpa",            // reserved infrastructure TLD (.arpa)
			"https://pad.onion",               // Tor hidden service (RFC 7686)
			"https://service.alt",             // alternative namespace (RFC 9476)
			"http://intranet",                 // single-label, not a public FQDN
			"https://app.getpad.dev?x=1",      // query string breaks link concatenation
			"https://app.getpad.dev/#/verify", // fragment breaks link concatenation
			"https://.com",                    // empty leading label
			"https://foo..com",                // empty interior label
			"https://-bad.com",                // label starts with hyphen
			"https://a.123",                   // all-numeric TLD
			"https://app.getpad.dev:99999",    // port out of range
			"https://app.getpad.dev:0",        // port zero
			"ftp://example.com",               // non-http scheme
			"",                                // unset
		} {
			srv.baseURL = base
			if srv.emailConfigured() {
				t.Fatalf("emailConfigured() must be false for unusable base URL %q", base)
			}
		}
	})

	// (b2) Full register path stays closed for a loopback base URL.
	t.Run("register closed for loopback base URL", func(t *testing.T) {
		srv := testServer(t)
		bootstrapFirstUser(t, srv, "admin@pad.test", "Admin")
		sender, _ := newMailSink(t)
		srv.SetEmailSender(sender)
		srv.cloudMode = true
		srv.baseURL = "http://localhost:7777"
		rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
			"email": "unreachable@pad.test", "name": "Unreachable", "password": "correct-horse-battery-staple",
		})
		if rr.Code != http.StatusForbidden {
			t.Fatalf("cloud loopback-base-URL register: expected 403, got %d: %s", rr.Code, rr.Body.String())
		}
		if u, _ := srv.store.GetUserByEmail("unreachable@pad.test"); u != nil {
			t.Fatal("no unverifiable user should be created when the base URL is unreachable")
		}
	})

	// (b3) A real public base URL enables it (sanity: the guard isn't
	// rejecting everything).
	t.Run("public base URL enables emailConfigured", func(t *testing.T) {
		srv := testServer(t)
		sender, _ := newMailSink(t)
		srv.SetEmailSender(sender)
		srv.cloudMode = true
		srv.baseURL = "https://app.getpad.dev"
		if !srv.emailConfigured() {
			t.Fatal("emailConfigured() must be true for a usable public base URL")
		}
	})
}

// ---------------------------------------------------------------------
// DR-1: accepting an email-bound invitation verifies an unverified account,
// and the invite-accept carve-out lets an unverified cloud user reach it.
// ---------------------------------------------------------------------

func TestInviteAccept_VerifiesUnverifiedUser(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@pad.test", "Admin")
	srv.cloudMode = true // so RequireVerifiedEmail is live and the carve-out matters

	admin, err := srv.store.GetUserByEmail("admin@pad.test")
	if err != nil || admin == nil {
		t.Fatalf("GetUserByEmail admin: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Invite WS", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	// An existing UNVERIFIED self-signup (created directly via the store's
	// explicit control).
	u, err := srv.store.CreateUser(models.UserCreate{
		Email: "invitee@pad.test", Name: "Invitee", Password: "correct-horse-battery-staple",
		Role: "member", Unverified: true,
	})
	if err != nil {
		t.Fatalf("CreateUser invitee: %v", err)
	}
	if u.IsEmailVerified() {
		t.Fatal("precondition: invitee should start unverified")
	}
	sess, err := srv.store.CreateSession(u.ID, "web", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// An invitation bound to the invitee's email.
	inv, err := srv.store.CreateInvitation(ws.ID, "invitee@pad.test", "editor", admin.ID)
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}

	rr := doRequestWithCookie(srv, "POST", "/api/v1/invitations/"+inv.Code+"/accept", nil, sess)
	if rr.Code != http.StatusOK {
		t.Fatalf("accept invitation (unverified user, cloud carve-out): expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// The account is now verified — proving "invited = verified" for later
	// accepts, not just register-with-code.
	u2, err := srv.store.GetUser(u.ID)
	if err != nil || u2 == nil {
		t.Fatalf("GetUser after accept: %v", err)
	}
	if !u2.IsEmailVerified() {
		t.Fatal("accepting an email-bound invitation should verify the account (DR-1)")
	}
}
