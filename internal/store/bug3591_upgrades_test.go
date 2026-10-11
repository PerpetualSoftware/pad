package store

import (
	"testing"
	"time"
)

// BUG-3591: the admin billing dashboard counted "new Pro signups" by
// created_at, so a long-time free account that upgraded inside the window
// was missed. SetUserPlan now stamps plan_upgraded_at when a write makes the
// plan pro from a not-pro effective plan, and CountBillingAggregates counts
// UpgradesInWindow by that stamp.

func bug3591User(t *testing.T, s *Store, email string, createdAgo time.Duration) string {
	t.Helper()
	u := createTestUser(t, s, email, "U", "s3cret")
	created := time.Now().Add(-createdAgo).UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(s.q(`UPDATE users SET created_at = ? WHERE id = ?`), created, u.ID); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func bug3591Set(t *testing.T, s *Store, id string, w PlanWrite) PlanWriteResult {
	t.Helper()
	res, err := s.SetUserPlan(id, w)
	if err != nil {
		t.Fatalf("SetUserPlan(%+v): %v", w, err)
	}
	return res
}

func bug3591Stamp(t *testing.T, s *Store, id string) string {
	t.Helper()
	var at string
	if err := s.db.QueryRow(s.q(`SELECT plan_upgraded_at FROM users WHERE id = ?`), id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func bug3591Backdate(t *testing.T, s *Store, id string, ago time.Duration) {
	t.Helper()
	at := time.Now().Add(-ago).UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(s.q(`UPDATE users SET plan_upgraded_at = ? WHERE id = ?`), at, id); err != nil {
		t.Fatal(err)
	}
}

func bug3591Counts(t *testing.T, s *Store) (upgrades, created int) {
	t.Helper()
	agg, err := s.CountBillingAggregates(time.Now().Add(-30 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return agg.UpgradesInWindow, agg.NewProSignups
}

// The case the bug names: an account created 60 days ago that upgrades today.
func TestBug3591_OldAccountUpgradingTodayCountsAsAnUpgrade(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	id := bug3591User(t, s, "old3591@example.com", 60*24*time.Hour)
	bug3591Set(t, s, id, PlanWrite{Plan: "pro", Source: PlanSourceStripe, Revision: 1000})

	if bug3591Stamp(t, s, id) == "" {
		t.Fatal("the upgrade left no plan_upgraded_at")
	}
	upgrades, created := bug3591Counts(t, s)
	if upgrades != 1 {
		t.Errorf("upgrades in window = %d, want 1", upgrades)
	}
	if created != 0 {
		t.Errorf("pro accounts created in window = %d, want 0 (created 60 days ago)", created)
	}
}

// Every write path that makes a plan pro stamps it: the manual one too.
func TestBug3591_ManualUpgradeStamps(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	id := bug3591User(t, s, "manual3591@example.com", 60*24*time.Hour)
	bug3591Set(t, s, id, PlanWrite{Plan: "pro", Source: PlanSourceManual, Force: true})
	if upgrades, _ := bug3591Counts(t, s); upgrades != 1 {
		t.Errorf("upgrades in window = %d, want 1", upgrades)
	}
}

// A pro-to-pro write (a renewal, or the sync repeating itself) is not an
// upgrade: an upgrade from before the window stays out of it.
func TestBug3591_RenewalKeepsTheEarlierStamp(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	id := bug3591User(t, s, "renew3591@example.com", 90*24*time.Hour)
	bug3591Set(t, s, id, PlanWrite{Plan: "pro", Source: PlanSourceStripe, Revision: 1000})
	bug3591Backdate(t, s, id, 40*24*time.Hour)
	before := bug3591Stamp(t, s, id)

	exp := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	bug3591Set(t, s, id, PlanWrite{Plan: "pro", ExpiresAt: exp, Source: PlanSourceStripe, Revision: 2000})
	if got := bug3591Stamp(t, s, id); got != before {
		t.Errorf("a renewal moved plan_upgraded_at from %q to %q", before, got)
	}
	if upgrades, _ := bug3591Counts(t, s); upgrades != 0 {
		t.Errorf("upgrades in window = %d, want 0 (upgraded 40 days ago)", upgrades)
	}
}

// An expired pro is free in effect, so taking pro again is an upgrade.
func TestBug3591_ExpiredProTakingProAgainIsAnUpgrade(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	id := bug3591User(t, s, "lapsed3591@example.com", 90*24*time.Hour)
	expired := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	bug3591Set(t, s, id, PlanWrite{Plan: "pro", ExpiresAt: expired, Source: PlanSourceStripe, Revision: 1000})
	bug3591Backdate(t, s, id, 60*24*time.Hour)

	bug3591Set(t, s, id, PlanWrite{Plan: "pro", Source: PlanSourceStripe, Revision: 2000})
	if upgrades, _ := bug3591Counts(t, s); upgrades != 1 {
		t.Errorf("upgrades in window = %d, want 1 (the lapsed pro came back)", upgrades)
	}
}

// An upgrade that churned before the window closed still happened in it.
func TestBug3591_UpgradeThatChurnedStillCounts(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	id := bug3591User(t, s, "churn3591@example.com", 60*24*time.Hour)
	bug3591Set(t, s, id, PlanWrite{Plan: "pro", Source: PlanSourceStripe, Revision: 1000})
	bug3591Set(t, s, id, PlanWrite{Plan: "free", Source: PlanSourceStripe, Revision: 2000})
	if upgrades, _ := bug3591Counts(t, s); upgrades != 1 {
		t.Errorf("upgrades in window = %d, want 1", upgrades)
	}
}

// A refused write changes nothing, the stamp included.
func TestBug3591_RefusedWriteDoesNotStamp(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	id := bug3591User(t, s, "stale3591@example.com", 60*24*time.Hour)
	bug3591Set(t, s, id, PlanWrite{Plan: "free", Source: PlanSourceStripe, Revision: 2000})
	res := bug3591Set(t, s, id, PlanWrite{Plan: "pro", Source: PlanSourceStripe, Revision: 1000})
	if res.Applied {
		t.Fatalf("PREMISE: the stale pro write applied: %+v", res)
	}
	if got := bug3591Stamp(t, s, id); got != "" {
		t.Errorf("a refused write stamped plan_upgraded_at = %q", got)
	}
}
