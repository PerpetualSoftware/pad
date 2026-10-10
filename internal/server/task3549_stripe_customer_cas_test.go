package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3549: POST /admin/stripe-customer-id never replaces a DIFFERENT stored
// Stripe customer. Overwriting the link is how a second checkout orphaned the
// first customer's subscription (still charging, its events finding no user).
// GET /admin/user-by-id lets the sidecar read the stored customer first.

const task3549Secret = "shh-task-3549"

func task3549Server(t *testing.T) (*Server, *models.User) {
	t.Helper()
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode(task3549Secret)
	u, err := srv.store.CreateUser(models.UserCreate{Email: "buyer@example.com", Name: "Buyer", Password: "pw-task-3549-x"})
	if err != nil {
		t.Fatal(err)
	}
	return srv, u
}

func setCustomer(t *testing.T, srv *Server, userID, customerID string) *httptest.ResponseRecorder {
	t.Helper()
	req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-customer-id", map[string]string{
		"user_id": userID, "customer_id": customerID, "cloud_secret": task3549Secret,
	}, map[string]string{"X-Cloud-Secret": task3549Secret})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func storedCustomer(t *testing.T, srv *Server, userID string) string {
	t.Helper()
	u, err := srv.store.GetUser(userID)
	if err != nil || u == nil {
		t.Fatalf("get user: %v", err)
	}
	return u.StripeCustomerID
}

func TestStripeCustomerID_CompareAndSet(t *testing.T) {
	srv, u := task3549Server(t)

	if rr := setCustomer(t, srv, u.ID, "cus_first"); rr.Code != http.StatusOK {
		t.Fatalf("first set: %d %s", rr.Code, rr.Body.String())
	}
	// The same id again is idempotent (a retried webhook, two racing checkouts
	// that both got the one customer from Stripe's idempotency key).
	if rr := setCustomer(t, srv, u.ID, "cus_first"); rr.Code != http.StatusOK {
		t.Fatalf("same id again: %d %s", rr.Code, rr.Body.String())
	}

	rr := setCustomer(t, srv, u.ID, "cus_second")
	if rr.Code != http.StatusConflict {
		t.Fatalf("a different id must be refused, got %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "stripe_customer_conflict" || body.Error.Details["stored_customer_id"] != "cus_first" {
		t.Fatalf("the refusal names the stored customer: %+v", body.Error)
	}
	if got := storedCustomer(t, srv, u.ID); got != "cus_first" {
		t.Fatalf("stored customer replaced: %q", got)
	}
}

func TestStripeCustomerID_ReplaceIsNotTheSidecars(t *testing.T) {
	srv, u := task3549Server(t)
	if rr := setCustomer(t, srv, u.ID, "cus_first"); rr.Code != http.StatusOK {
		t.Fatalf("first set: %d", rr.Code)
	}
	// The cloud secret alone cannot ask to replace.
	req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-customer-id", map[string]any{
		"user_id": u.ID, "customer_id": "cus_second", "replace": true, "cloud_secret": task3549Secret,
	}, map[string]string{"X-Cloud-Secret": task3549Secret})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict || storedCustomer(t, srv, u.ID) != "cus_first" {
		t.Fatalf("replace via the cloud secret: %d, stored %q", rr.Code, storedCustomer(t, srv, u.ID))
	}
}

func TestStripeCustomerID_StoreCASIgnoresAnUnknownUser(t *testing.T) {
	srv, _ := task3549Server(t)
	got, err := srv.store.SetUserStripeCustomerIDIfUnset("no-such-user", "cus_x")
	if err != nil || got != "" {
		t.Fatalf("unknown user: %q %v", got, err)
	}
}

func TestUserByID_ReturnsTheStoredCustomer(t *testing.T) {
	srv, u := task3549Server(t)
	get := func(path string) *httptest.ResponseRecorder {
		req := cloudAdminReq(t, "GET", path, nil, map[string]string{"X-Cloud-Secret": task3549Secret})
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	rr := get("/api/v1/admin/user-by-id?user_id=" + u.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("user-by-id: %d %s", rr.Code, rr.Body.String())
	}
	var before map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &before)
	if before["user_id"] != u.ID || before["stripe_customer_id"] != "" {
		t.Fatalf("before any customer: %v", before)
	}
	setCustomer(t, srv, u.ID, "cus_first")
	var after map[string]any
	_ = json.Unmarshal(get("/api/v1/admin/user-by-id?user_id="+u.ID).Body.Bytes(), &after)
	if after["stripe_customer_id"] != "cus_first" {
		t.Fatalf("after: %v", after)
	}
	if rr := get("/api/v1/admin/user-by-id?user_id=no-such-user"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown user: %d", rr.Code)
	}
	// No secret, no session: the normal auth gate.
	req := cloudAdminReq(t, "GET", "/api/v1/admin/user-by-id?user_id="+u.ID, nil, nil)
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no credential: %d", rr.Code)
	}
}

func TestStripeCustomerID_AnAdminUserMayReplace(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode(task3549Secret)
	u, err := srv.store.CreateUser(models.UserCreate{Email: "buyer@example.com", Name: "Buyer", Password: "pw-task-3549-x"})
	if err != nil {
		t.Fatal(err)
	}
	if rr := setCustomer(t, srv, u.ID, "cus_first"); rr.Code != http.StatusOK {
		t.Fatalf("first set: %d", rr.Code)
	}
	rr := doAuthedJSON(srv, "POST", "/api/v1/admin/stripe-customer-id", map[string]any{
		"user_id": u.ID, "customer_id": "cus_support_fix", "replace": true,
	}, adminToken)
	if rr.Code != http.StatusOK || storedCustomer(t, srv, u.ID) != "cus_support_fix" {
		t.Fatalf("admin replace: %d %s, stored %q", rr.Code, rr.Body.String(), storedCustomer(t, srv, u.ID))
	}
}
