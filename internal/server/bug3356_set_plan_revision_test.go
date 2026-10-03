package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BUG-3356: /admin/plan carries the revision the sidecar derived the plan
// at, and a stale one is refused with a reason, still as a 200 so the
// Stripe webhook does not retry it forever.

func postSetPlanAny(t *testing.T, srv *Server, body map[string]any) map[string]any {
	t.Helper()
	body["cloud_secret"] = planSourceSecret
	req := cloudAdminReq(t, "POST", "/api/v1/admin/plan", body, map[string]string{"X-Cloud-Secret": planSourceSecret})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBUG3356_SetPlanRefusesAStaleRevision(t *testing.T) {
	srv, _, u := planSourceServer(t)

	got := postSetPlanAny(t, srv, map[string]any{"user_id": u.ID, "plan": "free", "source": "stripe", "revision": 2000, "subscription_id": "sub_x"})
	if got["applied"] != true {
		t.Fatalf("newer write: %v", got)
	}
	got = postSetPlanAny(t, srv, map[string]any{"user_id": u.ID, "plan": "pro", "source": "stripe", "revision": 1000, "subscription_id": "sub_x"})
	if got["applied"] != false || got["reason"] != "stale_revision" || got["plan"] != "free" {
		t.Errorf("older write landing late: %v, want refused stale_revision holding free", got)
	}
	row, err := srv.store.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Plan != "free" {
		t.Errorf("row plan = %q, want free", row.Plan)
	}

	// The old shape (no revision) still applies.
	if got := postSetPlanAny(t, srv, map[string]any{"user_id": u.ID, "plan": "pro", "source": "stripe"}); got["applied"] != true {
		t.Errorf("an unrevisioned stripe write: %v, want applied", got)
	}
}

func TestBUG3356_SetPlanRefusesANegativeRevision(t *testing.T) {
	srv, _, u := planSourceServer(t)
	body := map[string]any{"user_id": u.ID, "plan": "pro", "source": "stripe", "revision": -5, "cloud_secret": planSourceSecret}
	req := cloudAdminReq(t, "POST", "/api/v1/admin/plan", body, map[string]string{"X-Cloud-Secret": planSourceSecret})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400: %s", rr.Code, rr.Body.String())
	}
}
