package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/billing"
	"github.com/pquerna/otp/totp"
)

// fakeSidecar is a controllable CloudSidecar used to assert the cancel-before-
// delete cascade (TASK-690) without standing up a real HTTP server.
//
// When hook is set, it runs inside CancelCustomer before the call returns —
// giving tests a chokepoint to verify invariants that are true only at
// that exact moment (e.g. "the user row still exists at the moment cancel
// is called"). Without the hook, a regression that swapped cancel and
// delete could still pass a simple "cancel was called" + "user is gone"
// assertion because both would be true at the end.
type fakeSidecar struct {
	calls          int32 // atomic counter of CancelCustomer invocations
	lastCustomerID atomic.Pointer[string]
	err            error
	hook           func(customerID string) // runs under CancelCustomer; optional
}

func (f *fakeSidecar) CancelCustomer(customerID string) error {
	atomic.AddInt32(&f.calls, 1)
	id := customerID // copy so the atomic pointer doesn't alias the caller's stack
	f.lastCustomerID.Store(&id)
	if f.hook != nil {
		f.hook(customerID)
	}
	return f.err
}

// GetBillingMetrics is a stub so fakeSidecar satisfies the extended
// CloudSidecar interface (TASK-827). The account-delete tests do not
// exercise the billing dashboard path; the dedicated billing tests use
// fakeBillingSidecar in handlers_admin_billing_test.go.
func (f *fakeSidecar) GetBillingMetrics() (*billing.BillingMetricsResponse, error) {
	return &billing.BillingMetricsResponse{}, nil
}

func (f *fakeSidecar) callCount() int {
	return int(atomic.LoadInt32(&f.calls))
}

func (f *fakeSidecar) lastID() string {
	if p := f.lastCustomerID.Load(); p != nil {
		return *p
	}
	return ""
}

// bootstrapAccountDeleteUser creates the first admin user, sets a
// Stripe customer ID on them, and returns (userID, sessionToken). Used by
// the handleDeleteAccount cascade tests.
//
// The activities.user_id scrub this helper used to perform is gone:
// DeleteAccountAtomic now de-identifies activities via the
// activities.user_id ON DELETE SET NULL FK (migrations 072/050, TASK-1959),
// so the bootstrap activity row no longer blocks the final DELETE FROM
// users. Deleting the account exercises the real cascade end to end.
func bootstrapAccountDeleteUser(t *testing.T, srv *Server, customerID string) (string, string) {
	t.Helper()
	token := bootstrapFirstUser(t, srv, "delete-me@test.com", "Delete Me")
	u, err := srv.store.GetUserByEmail("delete-me@test.com")
	if err != nil || u == nil {
		t.Fatalf("failed to locate bootstrapped user: %v", err)
	}
	if customerID != "" {
		if err := srv.store.SetUserStripeCustomerID(u.ID, customerID); err != nil {
			t.Fatalf("set customer id: %v", err)
		}
	}
	return u.ID, token
}

// deleteAccountReq issues the delete-account request from a client IP that
// differs from the loopback address bootstrap ran on. The session-IP-change
// middleware (log-only mode, the default) therefore writes an
// ActionSessionIPChanged audit row into activities (user_id → users.id) at
// request time and lets the request through. The delete must still succeed:
// this is the regression guard for the audit-row FK gap TASK-1959 closed —
// previously the fresh audit row blocked the final DELETE FROM users and the
// handler 500'd.
func deleteAccountReq(srv *Server, body interface{}, token string) *httptest.ResponseRecorder {
	return doRequestWithCookieFrom(srv, "POST", "/api/v1/auth/delete-account", body, token, "198.51.100.7:5555")
}

// TestHandleDeleteAccount_CancelsStripeBeforeDelete is the happy-path
// cascade assertion: a paying user's CancelCustomer runs FIRST (while the
// user row is still present), then the local delete goes through, and the
// user row is gone afterwards.
//
// The inline hook closes the cancel-before-delete loophole: we check the
// user row directly during the CancelCustomer call, so a regression that
// reverses the order (delete first, then cancel) can't pass — by the time
// cancel would be called, the row would already be absent and the
// assertion would fire inside the hook.
func TestHandleDeleteAccount_CancelsStripeBeforeDelete(t *testing.T) {
	srv := testServer(t)

	userID, token := bootstrapAccountDeleteUser(t, srv, "cus_paying_user")

	fake := &fakeSidecar{}
	fake.hook = func(customerID string) {
		if customerID != "cus_paying_user" {
			t.Errorf("CancelCustomer called with %q during hook, want cus_paying_user", customerID)
		}
		// Invariant under test: the user row MUST still exist at the
		// moment cancel fires. A regression that flipped cancel/delete
		// order would blow up this check because the user would already
		// be gone by the time the sidecar was called.
		u, err := srv.store.GetUser(userID)
		if err != nil {
			t.Errorf("get user during cancel hook: %v", err)
			return
		}
		if u == nil {
			t.Error("user row must still be present when CancelCustomer is called (cancel must precede delete)")
		}
	}
	srv.SetCloudSidecar(fake)

	rr := deleteAccountReq(srv, map[string]interface{}{"password": "correct-horse-battery-staple"}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete-account: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if fake.callCount() != 1 {
		t.Fatalf("CancelCustomer: expected 1 call, got %d", fake.callCount())
	}
	if got := fake.lastID(); got != "cus_paying_user" {
		t.Errorf("CancelCustomer called with %q, want cus_paying_user", got)
	}

	// After the whole flow, user row must be gone.
	u, err := srv.store.GetUser(userID)
	if err != nil {
		t.Fatalf("get user after delete: %v", err)
	}
	if u != nil {
		t.Error("expected user to be deleted after successful cascade")
	}
}

// TestHandleDeleteAccount_SkipsWhenNoStripeCustomer verifies that users
// without a Stripe customer ID (free tier, OAuth-only, never paid) don't
// trigger a sidecar call. Otherwise we'd burn a sidecar RPC on every
// free-user delete and log-spam 400 "customer_id must start with 'cus_'".
func TestHandleDeleteAccount_SkipsWhenNoStripeCustomer(t *testing.T) {
	srv := testServer(t)
	fake := &fakeSidecar{}
	srv.SetCloudSidecar(fake)

	userID, token := bootstrapAccountDeleteUser(t, srv, "") // no customer ID

	rr := deleteAccountReq(srv, map[string]interface{}{"password": "correct-horse-battery-staple"}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete-account: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if fake.callCount() != 0 {
		t.Errorf("CancelCustomer: expected 0 calls for non-paying user, got %d", fake.callCount())
	}

	u, err := srv.store.GetUser(userID)
	if err != nil {
		t.Fatalf("get user after delete: %v", err)
	}
	if u != nil {
		t.Error("expected user to be deleted even without a Stripe customer")
	}
}

// TestHandleDeleteAccount_SkipsWhenNoSidecarConfigured verifies that a
// self-hosted deploy (no CloudSidecar wired) lets deletes complete for
// paying users without blowing up on nil dereference. In practice this
// arrangement shouldn't exist (if the user has a Stripe customer ID,
// cloud mode was configured at some point) but it's the graceful-fallback
// contract: missing sidecar ≠ broken deletes.
func TestHandleDeleteAccount_SkipsWhenNoSidecarConfigured(t *testing.T) {
	srv := testServer(t)
	// No SetCloudSidecar — the reverse hook is nil.

	userID, token := bootstrapAccountDeleteUser(t, srv, "cus_paying_user")

	rr := deleteAccountReq(srv, map[string]interface{}{"password": "correct-horse-battery-staple"}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete-account: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	u, err := srv.store.GetUser(userID)
	if err != nil {
		t.Fatalf("get user after delete: %v", err)
	}
	if u != nil {
		t.Error("expected user to be deleted when sidecar is absent")
	}
}

// TestHandleDeleteAccount_AbortsOnSidecarTransportFailure — a bare error
// (no SidecarError) means transport failure or timeout. We MUST NOT delete
// the user; a retry needs the data intact to re-drive the cancel.
func TestHandleDeleteAccount_AbortsOnSidecarTransportFailure(t *testing.T) {
	srv := testServer(t)
	fake := &fakeSidecar{err: errors.New("connect: connection refused")}
	srv.SetCloudSidecar(fake)

	userID, token := bootstrapAccountDeleteUser(t, srv, "cus_paying_user")

	rr := deleteAccountReq(srv, map[string]interface{}{"password": "correct-horse-battery-staple"}, token)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("delete-account: expected 500, got %d: %s", rr.Code, rr.Body.String())
	}

	// Critical: user is STILL present — aborted cleanly.
	u, err := srv.store.GetUser(userID)
	if err != nil {
		t.Fatalf("get user after failed delete: %v", err)
	}
	if u == nil {
		t.Fatal("expected user to still exist after sidecar transport failure")
	}
	if u.StripeCustomerID != "cus_paying_user" {
		t.Errorf("stripe_customer_id must be preserved for retry; got %q", u.StripeCustomerID)
	}
}

// TestHandleDeleteAccount_AbortsOnSidecar5xx — pad-cloud reported a 5xx.
// Treated identically to transport failure: user stays put, retry can
// re-drive the cancel.
func TestHandleDeleteAccount_AbortsOnSidecar5xx(t *testing.T) {
	srv := testServer(t)
	fake := &fakeSidecar{err: &billing.SidecarError{
		Status: http.StatusInternalServerError,
		Body:   `{"error":"Failed to cancel subscription"}`,
	}}
	srv.SetCloudSidecar(fake)

	userID, token := bootstrapAccountDeleteUser(t, srv, "cus_paying_user")

	rr := deleteAccountReq(srv, map[string]interface{}{"password": "correct-horse-battery-staple"}, token)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("delete-account: expected 500 on sidecar 5xx, got %d: %s", rr.Code, rr.Body.String())
	}

	u, err := srv.store.GetUser(userID)
	if err != nil {
		t.Fatalf("get user after 5xx: %v", err)
	}
	if u == nil {
		t.Fatal("expected user to still exist after sidecar 5xx")
	}
}

// TestHandleDeleteAccount_AbortsOnSidecar4xx ensures every 4xx from
// pad-cloud — 400 (malformed request), 403 (wrong cloud_secret) — aborts
// the delete. pad-cloud normalizes Stripe's "already gone" cases to a 200
// internally (see pad-cloud stripe.go isStripeAlreadyGone), so a real 4xx
// means an ops bug on our side, never "nothing to cancel, proceed".
// Continuing on 4xx would silently wipe the StripeCustomerID while the
// subscription kept billing — the exact regression TASK-690 exists to
// prevent.
func TestHandleDeleteAccount_AbortsOnSidecar4xx(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"400 bad_request", http.StatusBadRequest, `{"error":"customer_id must start with 'cus_'"}`},
		{"403 wrong_secret", http.StatusForbidden, `{"error":"Forbidden"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			fake := &fakeSidecar{err: &billing.SidecarError{Status: tc.status, Body: tc.body}}
			srv.SetCloudSidecar(fake)

			userID, token := bootstrapAccountDeleteUser(t, srv, "cus_paying_user")

			rr := deleteAccountReq(srv, map[string]interface{}{"password": "correct-horse-battery-staple"}, token)
			if rr.Code != http.StatusInternalServerError {
				t.Fatalf("expected 500 on sidecar %d, got %d: %s", tc.status, rr.Code, rr.Body.String())
			}

			// Critical: user MUST still exist and still own the customer ID
			// so ops can investigate / retry. If we had wiped the local row
			// here, the Stripe customer would keep billing with no way for
			// us to find who it belonged to.
			u, err := srv.store.GetUser(userID)
			if err != nil {
				t.Fatalf("get user after %d: %v", tc.status, err)
			}
			if u == nil {
				t.Fatalf("user row must still exist after sidecar %d", tc.status)
			}
			if u.StripeCustomerID != "cus_paying_user" {
				t.Errorf("StripeCustomerID must be preserved after sidecar %d, got %q", tc.status, u.StripeCustomerID)
			}
			if fake.callCount() != 1 {
				t.Errorf("expected exactly 1 sidecar call, got %d", fake.callCount())
			}
		})
	}
}

// TestHandleDeleteAccount_PartialDelete_WhenLocalDeleteFailsAfterCancel
// exercises the most dangerous state: the sidecar successfully cancelled
// Stripe, but the local DELETE then failed. Before the round-1 fix the
// response claimed "No data was removed" even though billing was already
// gone; now it returns a truthful partial_delete error and logs the
// customer ID for operator follow-up.
//
// The "local delete fails" condition is injected via the sidecar hook —
// the existing seam that runs AFTER the cancel succeeds but BEFORE
// DeleteAccountAtomic. It drops mcp_audit_log (a leaf table with no
// inbound FKs, deleted from partway through DeleteAccountAtomic), so the
// local delete's `DELETE FROM mcp_audit_log` errors and rolls the whole
// transaction back. This replaces the pre-TASK-1959 mechanism, which
// relied on an activities.user_id FK violation — no longer reproducible
// now that FK is ON DELETE SET NULL. (Each test gets an isolated DB file
// copy, so the drop can't leak into sibling tests.)
func TestHandleDeleteAccount_PartialDelete_WhenLocalDeleteFailsAfterCancel(t *testing.T) {
	srv := testServer(t)
	fake := &fakeSidecar{}
	fake.hook = func(string) {
		// Sabotage the local delete AFTER the cancel has already fired.
		if _, err := srv.store.DB().Exec("DROP TABLE mcp_audit_log"); err != nil {
			t.Errorf("drop mcp_audit_log to force local-delete failure: %v", err)
		}
	}
	srv.SetCloudSidecar(fake)

	userID, token := bootstrapAccountDeleteUser(t, srv, "cus_paying_user")

	rr := deleteAccountReq(srv, map[string]interface{}{"password": "correct-horse-battery-staple"}, token)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rr.Code, rr.Body.String())
	}

	// Response must be the truthful partial-delete code, NOT a generic
	// internal_error that claims nothing was removed.
	body := rr.Body.String()
	if !strings.Contains(body, "partial_delete") {
		t.Errorf("expected partial_delete error code in body, got %s", body)
	}
	if !strings.Contains(body, "billing was cancelled") {
		t.Errorf("expected truthful 'billing was cancelled' message, got %s", body)
	}

	// Sidecar WAS called — that's the whole premise of this test.
	if fake.callCount() != 1 {
		t.Errorf("expected 1 cancel call, got %d", fake.callCount())
	}

	// And the user row is still present — ops needs it to investigate.
	u, err := srv.store.GetUser(userID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if u == nil {
		t.Fatal("user row must still exist after partial_delete")
	}
}

// TestHandleDeleteAccount_SkipsCancelOnWrongPassword — the password check
// runs BEFORE the sidecar call, so a wrong-password attempt must not leak
// a cancel RPC. (Otherwise an attacker with a session cookie but no
// password could still cancel the victim's Stripe subscription.)
func TestHandleDeleteAccount_SkipsCancelOnWrongPassword(t *testing.T) {
	srv := testServer(t)
	fake := &fakeSidecar{}
	srv.SetCloudSidecar(fake)

	_, token := bootstrapAccountDeleteUser(t, srv, "cus_paying_user")

	rr := deleteAccountReq(srv, map[string]interface{}{"password": "wrong-password"}, token)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("delete-account: expected 403 on wrong password, got %d: %s", rr.Code, rr.Body.String())
	}
	if fake.callCount() != 0 {
		t.Errorf("CancelCustomer must not be called on wrong-password attempts, got %d calls", fake.callCount())
	}
}

// enableTOTPForDeleteUser turns on 2FA for a bootstrapped delete-test user
// using a known secret, mirroring the real SetTOTPSecret → EnableTOTP flow,
// and returns the plaintext secret so callers can mint valid codes with
// totp.GenerateCode. The stored recovery codes are irrelevant to the delete
// path (it accepts only a TOTP code, not a recovery code), so a placeholder
// hashed value is fine here.
func enableTOTPForDeleteUser(t *testing.T, srv *Server, userID string) string {
	t.Helper()
	const secret = "JBSWY3DPEHPK3PXP" // standard base32 test vector
	if err := srv.store.SetTOTPSecret(userID, secret); err != nil {
		t.Fatalf("SetTOTPSecret: %v", err)
	}
	if err := srv.store.EnableTOTP(userID, secret, "placeholder-hashed-recovery-code"); err != nil {
		t.Fatalf("EnableTOTP: %v", err)
	}
	return secret
}

// validTOTPCode mints the current valid TOTP code for a secret, using the
// same library the handler validates against.
func validTOTPCode(t *testing.T, secret string) string {
	t.Helper()
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	return code
}

// TestHandleDeleteAccount_SuccessBody pins the exact JSON envelope a UI
// consumes on a successful delete: {"ok": true} and nothing else. The
// cascade/skip tests above assert only the 200 status; the web Danger Zone
// keys its "account deleted → hard-redirect to /login" transition off this
// body (web/src/routes/console/settings/+page.svelte::deleteAccount awaits
// api.auth.deleteAccount which types the response as {ok: boolean}). A
// regression that renamed "ok", returned an empty 200, or leaked an extra
// field would break that transition while still returning 200, so it needs
// its own assertion. This is the non-2FA, non-Stripe baseline; the TOTP
// happy-path test below covers the same envelope on the 2FA branch.
func TestHandleDeleteAccount_SuccessBody(t *testing.T) {
	srv := testServer(t)
	fake := &fakeSidecar{}
	srv.SetCloudSidecar(fake)

	_, token := bootstrapAccountDeleteUser(t, srv, "") // no Stripe customer, no 2FA

	rr := deleteAccountReq(srv, map[string]interface{}{"password": "correct-horse-battery-staple"}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete-account: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("expected application/json Content-Type on success, got %q", ct)
	}

	var resp map[string]interface{}
	parseJSON(t, rr, &resp)
	if resp["ok"] != true {
		t.Errorf("expected {\"ok\": true}, got %v", resp)
	}
	// Pin the envelope: the success body carries exactly one key. A stray
	// extra field (error echo, message, user payload) would be a silent
	// contract widening the UI never opted into.
	if len(resp) != 1 {
		t.Errorf("success body must be exactly {\"ok\": true}, got %d keys: %v", len(resp), resp)
	}
}

// TestHandleDeleteAccount_TOTPEnabled_ValidCode — a 2FA-enabled account with
// the correct password AND a valid TOTP code deletes successfully (200
// {ok:true}) and the user row is gone.
func TestHandleDeleteAccount_TOTPEnabled_ValidCode(t *testing.T) {
	srv := testServer(t)
	fake := &fakeSidecar{}
	srv.SetCloudSidecar(fake)

	userID, token := bootstrapAccountDeleteUser(t, srv, "")
	secret := enableTOTPForDeleteUser(t, srv, userID)

	rr := deleteAccountReq(srv, map[string]interface{}{
		"password":  "correct-horse-battery-staple",
		"totp_code": validTOTPCode(t, secret),
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid TOTP code, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	parseJSON(t, rr, &resp)
	if resp["ok"] != true {
		t.Errorf("expected {ok:true} on successful delete, got %v", resp)
	}

	u, err := srv.store.GetUser(userID)
	if err != nil {
		t.Fatalf("get user after delete: %v", err)
	}
	if u != nil {
		t.Error("expected user to be deleted after valid-TOTP delete")
	}
}

// TestHandleDeleteAccount_TOTPEnabled_MissingCode — a 2FA-enabled account
// with the correct password but NO totp_code is rejected with totp_required,
// no Stripe cancel is attempted, and the user row survives.
func TestHandleDeleteAccount_TOTPEnabled_MissingCode(t *testing.T) {
	srv := testServer(t)
	fake := &fakeSidecar{}
	srv.SetCloudSidecar(fake)

	userID, token := bootstrapAccountDeleteUser(t, srv, "cus_paying_user")
	enableTOTPForDeleteUser(t, srv, userID)

	rr := deleteAccountReq(srv, map[string]interface{}{
		"password": "correct-horse-battery-staple",
	}, token)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when TOTP code is missing, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "totp_required") {
		t.Errorf("expected totp_required error code, got %s", rr.Body.String())
	}
	if fake.callCount() != 0 {
		t.Errorf("CancelCustomer must not fire when TOTP is missing, got %d calls", fake.callCount())
	}

	u, err := srv.store.GetUser(userID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if u == nil {
		t.Error("user row must survive a missing-TOTP delete attempt")
	}
}

// TestHandleDeleteAccount_TOTPEnabled_InvalidCode — a 2FA-enabled account
// with the correct password but a WRONG totp_code is rejected with
// totp_invalid, no Stripe cancel is attempted, and the user row survives.
func TestHandleDeleteAccount_TOTPEnabled_InvalidCode(t *testing.T) {
	srv := testServer(t)
	fake := &fakeSidecar{}
	srv.SetCloudSidecar(fake)

	userID, token := bootstrapAccountDeleteUser(t, srv, "cus_paying_user")
	secret := enableTOTPForDeleteUser(t, srv, userID)

	// Derive a code guaranteed to differ from the currently-valid one.
	wrong := "000000"
	if wrong == validTOTPCode(t, secret) {
		wrong = "111111"
	}

	rr := deleteAccountReq(srv, map[string]interface{}{
		"password":  "correct-horse-battery-staple",
		"totp_code": wrong,
	}, token)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on invalid TOTP code, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "totp_invalid") {
		t.Errorf("expected totp_invalid error code, got %s", rr.Body.String())
	}
	if fake.callCount() != 0 {
		t.Errorf("CancelCustomer must not fire on invalid TOTP, got %d calls", fake.callCount())
	}

	u, err := srv.store.GetUser(userID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if u == nil {
		t.Error("user row must survive an invalid-TOTP delete attempt")
	}
}

// TestHandleDeleteAccount_NoTOTP_Unaffected — an account WITHOUT 2FA is
// unchanged by this feature: no totp_code is required, and supplying one is
// simply ignored. The delete succeeds exactly as before.
func TestHandleDeleteAccount_NoTOTP_Unaffected(t *testing.T) {
	srv := testServer(t)
	fake := &fakeSidecar{}
	srv.SetCloudSidecar(fake)

	userID, token := bootstrapAccountDeleteUser(t, srv, "") // no TOTP enabled

	// No totp_code supplied — must still delete for a non-2FA account.
	rr := deleteAccountReq(srv, map[string]interface{}{
		"password": "correct-horse-battery-staple",
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("non-2FA delete: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	u, err := srv.store.GetUser(userID)
	if err != nil {
		t.Fatalf("get user after delete: %v", err)
	}
	if u != nil {
		t.Error("expected non-2FA user to be deleted without a TOTP code")
	}
}
