package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3295 / PLAN-3291 DR-6, through the router: the sidecar's
// /api/v1/admin/plan names a source, a refused lowering is a 200 with
// applied:false (any other status makes the Stripe webhook retry forever),
// an omitted source means manual, and the admin user update forces.

const planSourceSecret = "plan-source-secret"

type setPlanResponse struct {
	Plan       string `json:"plan"`
	PlanSource string `json:"plan_source"`
	Applied    bool   `json:"applied"`
	OK         bool   `json:"ok"`
}

func planSourceServer(t *testing.T) (*Server, string, *models.User) {
	t.Helper()
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode(planSourceSecret)
	u, err := srv.store.CreateUser(models.UserCreate{
		Email: "payer@example.com", Name: "Payer", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, adminToken, u
}

func postSetPlan(t *testing.T, srv *Server, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body["cloud_secret"] = planSourceSecret
	req := cloudAdminReq(t, "POST", "/api/v1/admin/plan", body, map[string]string{"X-Cloud-Secret": planSourceSecret})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func decodeSetPlan(t *testing.T, rr *httptest.ResponseRecorder) setPlanResponse {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var out setPlanResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, rr.Body.String())
	}
	return out
}

func TestSetPlan_StripeFreeDoesNotClobberManualPro(t *testing.T) {
	srv, _, u := planSourceServer(t)

	// An operator grants pro; an omitted source is manual.
	got := decodeSetPlan(t, postSetPlan(t, srv, map[string]string{"user_id": u.ID, "plan": "pro"}))
	if !got.Applied || got.Plan != "pro" || got.PlanSource != store.PlanSourceManual {
		t.Fatalf("grant: %+v, want applied pro/manual", got)
	}

	// A Stripe cancellation for the same user is refused, as a 200.
	got = decodeSetPlan(t, postSetPlan(t, srv, map[string]string{"user_id": u.ID, "plan": "free", "source": "stripe"}))
	if got.Applied || !got.OK || got.Plan != "pro" || got.PlanSource != store.PlanSourceManual {
		t.Fatalf("stripe free: %+v, want ok, not applied, holding pro/manual", got)
	}
	row, err := srv.store.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Plan != "pro" {
		t.Fatalf("row plan = %q after a refused lowering, want pro", row.Plan)
	}
}

func TestSetPlan_StripeFreeLowersStripePro(t *testing.T) {
	srv, _, u := planSourceServer(t)

	got := decodeSetPlan(t, postSetPlan(t, srv, map[string]string{"user_id": u.ID, "plan": "pro", "source": "stripe"}))
	if !got.Applied || got.PlanSource != store.PlanSourceStripe {
		t.Fatalf("stripe pro: %+v", got)
	}
	got = decodeSetPlan(t, postSetPlan(t, srv, map[string]string{"user_id": u.ID, "plan": "free", "source": "stripe"}))
	if !got.Applied || got.Plan != "free" {
		t.Fatalf("stripe free: %+v, want applied free", got)
	}
}

func TestSetPlan_RejectsUnknownSource(t *testing.T) {
	srv, _, u := planSourceServer(t)
	rr := postSetPlan(t, srv, map[string]string{"user_id": u.ID, "plan": "pro", "source": "apple"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("source apple: status %d, want 400: %s", rr.Code, rr.Body.String())
	}
	row, err := srv.store.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Plan != "free" {
		t.Fatalf("a refused source still wrote: plan = %q", row.Plan)
	}
}

// The admin console's plan change is an operator's explicit choice: it
// lowers a Stripe pro, and the user detail then reports the source.
func TestAdminUpdateUser_PlanForcesAndReportsSource(t *testing.T) {
	srv, adminToken, u := planSourceServer(t)
	decodeSetPlan(t, postSetPlan(t, srv, map[string]string{"user_id": u.ID, "plan": "pro", "source": "stripe"}))

	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/admin/users/"+u.ID, map[string]any{"plan": "free"}, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rr.Code, rr.Body.String())
	}
	var patched struct {
		Plan       string `json:"plan"`
		PlanSource string `json:"plan_source"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &patched); err != nil {
		t.Fatal(err)
	}
	if patched.Plan != "free" || patched.PlanSource != store.PlanSourceManual {
		t.Fatalf("patch response = %+v, want free/manual", patched)
	}

	rr = doRequestWithCookie(srv, "GET", "/api/v1/admin/users/"+u.ID, nil, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rr.Code, rr.Body.String())
	}
	var detail struct {
		Plan       string `json:"plan"`
		PlanSource string `json:"plan_source"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Plan != "free" || detail.PlanSource != store.PlanSourceManual {
		t.Fatalf("detail = %+v, want free/manual", detail)
	}
}
