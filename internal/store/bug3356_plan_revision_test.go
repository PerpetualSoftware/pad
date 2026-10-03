package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3356: Stripe-derived plan writes are ordered by a revision (the
// sidecar's fetch time), and a plan past its expiry entitles nothing.
// Runs on SQLite, and on Postgres under make test-pg.

func readPlanRevision(t *testing.T, s *Store, userID string) (int64, string) {
	t.Helper()
	var rev int64
	var sub string
	if err := s.db.QueryRow(s.q(`SELECT plan_revision, plan_subscription_id FROM users WHERE id = ?`), userID).Scan(&rev, &sub); err != nil {
		t.Fatal(err)
	}
	return rev, sub
}

func TestSetUserPlan_RevisionOrdersStripeWrites(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := createTestUser(t, s, "rev3356@example.com", "Rev", "s3cret")

	write := func(plan string, rev int64, sub string) PlanWriteResult {
		t.Helper()
		res, err := s.SetUserPlan(u.ID, PlanWrite{Plan: plan, Source: PlanSourceStripe, Revision: rev, SubscriptionID: sub})
		if err != nil {
			t.Fatalf("SetUserPlan(%s, %d): %v", plan, rev, err)
		}
		return res
	}

	// A newer fetch says cancelled; an older one ("still active") lands
	// after it. The older one must not restore pro.
	if res := write("pro", 100, "sub_1"); !res.Applied {
		t.Fatalf("first write refused: %+v", res)
	}
	if res := write("free", 300, "sub_1"); !res.Applied {
		t.Fatalf("newer cancellation refused: %+v", res)
	}
	res := write("pro", 200, "sub_1")
	if res.Applied || res.Reason != PlanRefusedStaleRevision || res.Plan != "free" {
		t.Errorf("an older fetch landing late = %+v, want refused stale_revision holding free", res)
	}
	if res := write("pro", 300, "sub_1"); res.Applied || res.Reason != PlanRefusedStaleRevision {
		t.Errorf("an equal revision = %+v, want refused stale_revision", res)
	}
	if rev, sub := readPlanRevision(t, s, u.ID); rev != 300 || sub != "sub_1" {
		t.Errorf("stored revision = (%d, %q), want (300, sub_1)", rev, sub)
	}

	// A write with no revision (a sidecar that predates it) still applies
	// and leaves the stored revision alone, so ordering resumes after it.
	if res := write("pro", 0, ""); !res.Applied {
		t.Fatalf("an unrevisioned write refused: %+v", res)
	}
	if rev, sub := readPlanRevision(t, s, u.ID); rev != 300 || sub != "sub_1" {
		t.Errorf("an unrevisioned write moved the revision: (%d, %q)", rev, sub)
	}
	if res := write("free", 250, "sub_1"); res.Applied {
		t.Errorf("a stale revision applied after an unrevisioned write: %+v", res)
	}

	// The source rule still decides independently: a stripe write may not
	// lower a manual grant, whatever its revision.
	if _, err := s.SetUserPlan(u.ID, PlanWrite{Plan: "pro", Source: PlanSourceManual, Force: true}); err != nil {
		t.Fatal(err)
	}
	if res := write("free", 900, "sub_1"); res.Applied || res.Reason != PlanRefusedSource {
		t.Errorf("stripe lowering a manual grant = %+v, want refused by the source rule", res)
	}

	if _, err := s.SetUserPlan(u.ID, PlanWrite{Plan: "pro", Source: PlanSourceStripe, Revision: -1}); err == nil {
		t.Error("a negative revision was accepted")
	}
}

func TestEffectivePlanEnforcesExpiry(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name, plan, expires, want string
	}{
		{"no expiry", "pro", "", "pro"},
		{"expiry ahead", "pro", "2026-10-03T12:00:01Z", "pro"},
		{"expiry exactly now", "pro", "2026-10-03T12:00:00Z", "free"},
		{"expiry passed", "self-hosted", "2026-10-01T00:00:00Z", "free"},
		{"unreadable expiry", "pro", "next tuesday", "free"},
		{"blank plan", "", "", "free"},
	}
	for _, tc := range cases {
		u := &models.User{Plan: tc.plan, PlanExpiresAt: tc.expires}
		if got := u.EffectivePlan(now); got != tc.want {
			t.Errorf("%s: EffectivePlan = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The limit check decides from the effective plan: an expired pro gets
// free limits.
func TestCheckUserLimitTreatsAnExpiredPlanAsFree(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := createTestUser(t, s, "exp3356@example.com", "Exp", "s3cret")
	set := func(expires string) {
		t.Helper()
		if _, err := s.db.Exec(s.q(`UPDATE users SET plan = 'pro', plan_expires_at = ? WHERE id = ?`), expires, u.ID); err != nil {
			t.Fatal(err)
		}
	}

	set(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	res, err := s.CheckUserLimit(u.ID, "workspaces")
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan != "pro" || res.Limit != -1 {
		t.Fatalf("control: live pro = %+v, want unlimited pro", res)
	}

	set(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
	res, err = s.CheckUserLimit(u.ID, "workspaces")
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan != "free" || res.Limit < 0 {
		t.Errorf("expired pro = %+v, want free limits", res)
	}
}
