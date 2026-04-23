package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/xarmian/pad/internal/billing"
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
// Clears activities.user_id for the user after bootstrap because
// DeleteAccountAtomic doesn't cascade through activities (a pre-existing
// limitation unrelated to TASK-690). Without this, the FK from
// activities.user_id → users.id blocks the final DELETE FROM users and
// every happy-path test ends in a 500. Narrow test-only scrub — it
// preserves the audit rows themselves (for callers that inspect them) and
// only detaches the user reference, which mirrors the ON DELETE SET NULL
// behaviour the production schema will get when the FK gap is fixed in a
// follow-up migration.
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
	if _, err := srv.store.DB().Exec("UPDATE activities SET user_id = NULL WHERE user_id = ?", u.ID); err != nil {
		t.Fatalf("scrub activities.user_id for test user: %v", err)
	}
	return u.ID, token
}

// deleteAccountReq is a doRequestWithCookieFrom wrapper that pins the
// RemoteAddr to 127.0.0.1 — the same loopback address bootstrap was
// invoked from. Without this, the session-IP-change middleware writes an
// ActionSessionIPChanged audit row at request time (user_id column,
// FK → users.id), which then blocks the final DELETE FROM users at the
// end of DeleteAccountAtomic. Until the account-delete cascade fix for
// audit rows lands (pre-existing bug, tracked separately), these tests
// keep the IP stable to sidestep the FK gap.
func deleteAccountReq(srv *Server, body interface{}, token string) *httptest.ResponseRecorder {
	return doRequestWithCookieFrom(srv, "POST", "/api/v1/auth/delete-account", body, token, "127.0.0.1:4321")
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
