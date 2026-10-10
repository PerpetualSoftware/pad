package store

import (
	"errors"
	"testing"
)

// TASK-3551: a non-cloud boot converts free users to self-hosted (unlimited),
// but never on a database a Pad Cloud instance owns. Runs on SQLite, and on
// Postgres under make test-pg.

func planOf(t *testing.T, s *Store, id string) string {
	t.Helper()
	u, err := s.GetUser(id)
	if err != nil {
		t.Fatal(err)
	}
	return u.Plan
}

func setPlanRaw(t *testing.T, s *Store, id, plan, source, customer string) {
	t.Helper()
	if _, err := s.db.Exec(s.q(`UPDATE users SET plan = ?, plan_source = ?, stripe_customer_id = ? WHERE id = ?`), plan, source, customer, id); err != nil {
		t.Fatal(err)
	}
}

// Control: an unmarked database with no billing state converts, as before.
func TestBackfillSelfHosted_SelfHostedDatabaseConverts(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := createTestUser(t, s, "sh@example.com", "SH", "s3cret")
	setPlanRaw(t, s, u.ID, "free", PlanSourceManual, "")

	n, own, err := s.BackfillSelfHostedPlans()
	if err != nil || own.Owned || n != 1 {
		t.Fatalf("n=%d owned=%v err=%v, want 1 converted", n, own.Owned, err)
	}
	if got := planOf(t, s, u.ID); got != "self-hosted" {
		t.Fatalf("plan = %q, want self-hosted", got)
	}
}

// A database a cloud boot marked converts nothing.
func TestBackfillSelfHosted_MarkedCloudDatabaseRefuses(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := createTestUser(t, s, "cloud@example.com", "Cloud", "s3cret")
	setPlanRaw(t, s, u.ID, "free", PlanSourceManual, "")
	if err := s.MarkCloudOwned(); err != nil {
		t.Fatal(err)
	}

	n, own, err := s.BackfillSelfHostedPlans()
	if !errors.Is(err, ErrCloudOwnedDatabase) || n != 0 || own.Marker != InstanceModeCloud {
		t.Fatalf("n=%d own=%+v err=%v, want ErrCloudOwnedDatabase on the marker", n, own, err)
	}
	if got := planOf(t, s, u.ID); got != "free" {
		t.Fatalf("plan = %q, want free (untouched)", got)
	}
}

// A Cloud database that predates the marker is recognised by billing state:
// a Stripe plan source, or a stored Stripe customer id, each on its own.
func TestBackfillSelfHosted_UnmarkedDatabaseWithBillingStateRefuses(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, source, customer string
	}{
		{"stripe plan source", PlanSourceStripe, ""},
		{"stripe customer id", PlanSourceManual, "cus_123"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testStore(t)
			free := createTestUser(t, s, "free@example.com", "Free", "s3cret")
			payer := createTestUser(t, s, "payer@example.com", "Payer", "s3cret")
			setPlanRaw(t, s, free.ID, "free", PlanSourceManual, "")
			setPlanRaw(t, s, payer.ID, "pro", c.source, c.customer)

			n, own, err := s.BackfillSelfHostedPlans()
			if !errors.Is(err, ErrCloudOwnedDatabase) || n != 0 || own.Marker != "" || own.StripeUsers != 1 {
				t.Fatalf("n=%d own=%+v err=%v, want ErrCloudOwnedDatabase on the fingerprint", n, own, err)
			}
			if got := planOf(t, s, free.ID); got != "free" {
				t.Fatalf("plan = %q, want free (untouched)", got)
			}
		})
	}
}

// release-cloud: refused while billing state remains, unless forced; once
// released the conversion runs (taking the source over, as the primitive
// does); a later cloud boot marks the database cloud-owned again.
func TestReleaseCloudOwnership(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	lapsed := createTestUser(t, s, "lapsed@example.com", "Lapsed", "s3cret")
	setPlanRaw(t, s, lapsed.ID, "free", PlanSourceStripe, "cus_1")
	if err := s.MarkCloudOwned(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ReleaseCloudOwnership(false); !errors.Is(err, ErrCloudFingerprintPresent) {
		t.Fatalf("release without force = %v, want ErrCloudFingerprintPresent", err)
	}
	if _, _, err := s.BackfillSelfHostedPlans(); !errors.Is(err, ErrCloudOwnedDatabase) {
		t.Fatalf("a refused release must leave the database cloud-owned; backfill err = %v", err)
	}

	if _, err := s.ReleaseCloudOwnership(true); err != nil {
		t.Fatal(err)
	}
	if n, _, err := s.BackfillSelfHostedPlans(); err != nil || n != 1 {
		t.Fatalf("after release: n=%d err=%v, want 1 converted", n, err)
	}
	if u, _ := s.GetUser(lapsed.ID); u.Plan != "self-hosted" || u.PlanSource != PlanSourceManual {
		t.Fatalf("lapsed user = (%q, %q), want (self-hosted, manual)", u.Plan, u.PlanSource)
	}

	if err := s.MarkCloudOwned(); err != nil {
		t.Fatal(err)
	}
	if own, err := s.GetCloudOwnership(); err != nil || !own.Owned || own.Marker != InstanceModeCloud {
		t.Fatalf("after a cloud boot: %+v err=%v, want cloud-owned again", own, err)
	}
}
