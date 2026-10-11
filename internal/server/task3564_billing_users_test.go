package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3564: GET /admin/billing-users pages every user the reconcile sweep
// must re-sync (a stored Stripe customer, or a stripe-sourced plan), and only
// those, behind the same cloud-secret rule as its siblings.
func TestBillingUsersPagesTheUsersTheSweepNeeds(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode(cloudContractSecret)
	mk := func(email string) string {
		u, err := srv.store.CreateUser(models.UserCreate{Email: email, Name: "U", Password: "pw-billing-x"})
		if err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	linked := mk("linked@example.com")
	if err := srv.store.SetUserStripeCustomerID(linked, "cus_linked"); err != nil {
		t.Fatal(err)
	}
	stripeSourced := mk("sourced@example.com")
	if _, err := srv.store.DB().Exec(`UPDATE users SET plan = 'free', plan_source = 'stripe' WHERE id = ?`, stripeSourced); err != nil {
		t.Fatal(err)
	}
	mk("nobody@example.com") // no link, manual plan: not a billing user

	get := func(path string, hdr map[string]string) (*httptest.ResponseRecorder, struct {
		Users []map[string]any `json:"users"`
		Next  string           `json:"next"`
	}) {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, cloudAdminReq(t, "GET", path, nil, hdr))
		var out struct {
			Users []map[string]any `json:"users"`
			Next  string           `json:"next"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr, out
	}
	hdr := map[string]string{"X-Cloud-Secret": cloudContractSecret}

	// Paged one at a time, the cursor walks exactly the two billing users.
	var seen []string
	after := ""
	for i := 0; i < 5; i++ {
		rr, page := get("/api/v1/admin/billing-users?limit=1&after="+after, hdr)
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
		}
		for _, u := range page.Users {
			seen = append(seen, u["user_id"].(string))
		}
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	want := map[string]bool{linked: true, stripeSourced: true}
	if len(seen) != 2 || !want[seen[0]] || !want[seen[1]] {
		t.Fatalf("pages walked %v, want exactly the linked and the stripe-sourced user", seen)
	}

	// Without the secret (and without an admin session) it is refused.
	if rr, _ := get("/api/v1/admin/billing-users", nil); rr.Code == http.StatusOK {
		t.Fatalf("no credential answered %d", rr.Code)
	}
	if rr, _ := get("/api/v1/admin/billing-users", map[string]string{"X-Cloud-Secret": "wrong"}); rr.Code == http.StatusOK {
		t.Fatalf("a wrong secret answered %d", rr.Code)
	}
	// A bad limit is a 400.
	if rr, _ := get("/api/v1/admin/billing-users?limit=0", hdr); rr.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 answered %d, want 400", rr.Code)
	}
}
