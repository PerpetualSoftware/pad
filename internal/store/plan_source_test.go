package store

import (
	"strings"
	"testing"
)

// TASK-3295 / PLAN-3291 DR-6: a write that lowers a plan to free applies only
// from the source that set the plan (or when forced); every other write
// applies and takes the source over. Runs on SQLite, and on Postgres under
// make test-pg, because the rule is one conditional UPDATE in each dialect.
func TestSetUserPlan_SourceRule(t *testing.T) {
	t.Parallel()

	type state struct{ plan, source, expires string }
	cases := []struct {
		name        string
		start       state
		write       PlanWrite
		wantApplied bool
		want        state
	}{
		{
			name:        "stripe free vs a manual pro stays pro",
			start:       state{"pro", PlanSourceManual, "2030-01-01T00:00:00Z"},
			write:       PlanWrite{Plan: "free", Source: PlanSourceStripe},
			wantApplied: false,
			want:        state{"pro", PlanSourceManual, "2030-01-01T00:00:00Z"},
		},
		{
			name:        "stripe free vs a stripe pro goes free",
			start:       state{"pro", PlanSourceStripe, "2030-01-01T00:00:00Z"},
			write:       PlanWrite{Plan: "free", Source: PlanSourceStripe},
			wantApplied: true,
			want:        state{"free", PlanSourceStripe, ""},
		},
		{
			name:        "an admin force-lower applies over a stripe pro",
			start:       state{"pro", PlanSourceStripe, ""},
			write:       PlanWrite{Plan: "free", Source: PlanSourceManual, Force: true},
			wantApplied: true,
			want:        state{"free", PlanSourceManual, ""},
		},
		{
			name:        "an unforced manual free lowers a manual pro",
			start:       state{"pro", PlanSourceManual, ""},
			write:       PlanWrite{Plan: "free", Source: PlanSourceManual},
			wantApplied: true,
			want:        state{"free", PlanSourceManual, ""},
		},
		{
			name:        "an unforced manual free does not lower a stripe pro",
			start:       state{"pro", PlanSourceStripe, ""},
			write:       PlanWrite{Plan: "free", Source: PlanSourceManual},
			wantApplied: false,
			want:        state{"pro", PlanSourceStripe, ""},
		},
		{
			name:        "stripe free does not lower a manual self-hosted",
			start:       state{"self-hosted", PlanSourceManual, ""},
			write:       PlanWrite{Plan: "free", Source: PlanSourceStripe},
			wantApplied: false,
			want:        state{"self-hosted", PlanSourceManual, ""},
		},
		{
			name:        "a raise always applies and takes the source over",
			start:       state{"free", PlanSourceManual, ""},
			write:       PlanWrite{Plan: "pro", Source: PlanSourceStripe, ExpiresAt: "2031-01-01T00:00:00Z"},
			wantApplied: true,
			want:        state{"pro", PlanSourceStripe, "2031-01-01T00:00:00Z"},
		},
		{
			name:        "a same-level write is not a lowering and takes the source over",
			start:       state{"pro", PlanSourceManual, ""},
			write:       PlanWrite{Plan: "pro", Source: PlanSourceStripe},
			wantApplied: true,
			want:        state{"pro", PlanSourceStripe, ""},
		},
		{
			name:        "free to free from another source applies",
			start:       state{"free", PlanSourceManual, ""},
			write:       PlanWrite{Plan: "free", Source: PlanSourceStripe},
			wantApplied: true,
			want:        state{"free", PlanSourceStripe, ""},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := testStore(t)
			u := createTestUser(t, s, "plan-source@example.com", "Plan Source", "s3cret")
			if _, err := s.db.Exec(s.q(`UPDATE users SET plan = ?, plan_source = ?, plan_expires_at = ? WHERE id = ?`),
				tc.start.plan, tc.start.source, tc.start.expires, u.ID); err != nil {
				t.Fatal(err)
			}

			res, err := s.SetUserPlan(u.ID, tc.write)
			if err != nil {
				t.Fatalf("SetUserPlan: %v", err)
			}
			if res.Applied != tc.wantApplied {
				t.Errorf("Applied = %v, want %v", res.Applied, tc.wantApplied)
			}
			if res.Plan != tc.want.plan || res.Source != tc.want.source {
				t.Errorf("result = (%q, %q), want (%q, %q)", res.Plan, res.Source, tc.want.plan, tc.want.source)
			}

			// The row itself, read back through the user loader, not only
			// what SetUserPlan reported.
			got, err := s.GetUser(u.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Plan != tc.want.plan || got.PlanSource != tc.want.source || got.PlanExpiresAt != tc.want.expires {
				t.Errorf("row = (%q, %q, %q), want (%q, %q, %q)",
					got.Plan, got.PlanSource, got.PlanExpiresAt, tc.want.plan, tc.want.source, tc.want.expires)
			}
		})
	}
}

// A new user starts free with source manual (the column default), which is
// what a caller that omits a source writes too.
func TestSetUserPlan_NewUserIsManual(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := createTestUser(t, s, "fresh@example.com", "Fresh", "s3cret")
	got, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Plan != "free" || got.PlanSource != PlanSourceManual {
		t.Fatalf("new user = (%q, %q), want (free, manual)", got.Plan, got.PlanSource)
	}
}

func TestSetUserPlan_RefusesInvalidSourceAndMissingUser(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := createTestUser(t, s, "bad-source@example.com", "Bad Source", "s3cret")

	for _, src := range []string{"", "apple", "Stripe"} {
		if _, err := s.SetUserPlan(u.ID, PlanWrite{Plan: "pro", Source: src}); err == nil {
			t.Errorf("source %q: want an error, got nil", src)
		}
	}
	got, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Plan != "free" {
		t.Errorf("a refused source still wrote: plan = %q", got.Plan)
	}

	if _, err := s.SetUserPlan("no-such-user", PlanWrite{Plan: "pro", Source: PlanSourceManual}); err == nil {
		t.Error("missing user: want an error, got nil")
	}
}

// The migration's backfill marks a pro user with a Stripe customer as
// stripe-sourced, so their Stripe cancellation still lowers them. The
// statement is taken from the migration file of the dialect under test and
// run against rows reset to the pre-backfill state.
func TestMigrationPlanSourceBackfill(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	fs, path := migrationsFS, "migrations/101_user_plan_source.sql"
	if s.dialect.Driver() != DriverSQLite {
		fs, path = pgMigrationsFS, "pgmigrations/075_user_plan_source.sql"
	}
	body, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var backfill string
	for _, stmt := range strings.Split(string(body), ";") {
		lines := []string{}
		for _, l := range strings.Split(stmt, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "--") {
				lines = append(lines, l)
			}
		}
		if sql := strings.TrimSpace(strings.Join(lines, "\n")); strings.HasPrefix(sql, "UPDATE users") {
			backfill = sql
		}
	}
	if backfill == "" {
		t.Fatalf("%s: no UPDATE users statement found", path)
	}

	rows := []struct {
		email, plan, customer, want string
	}{
		{"paying@example.com", "pro", "cus_123", PlanSourceStripe},
		{"comped@example.com", "pro", "", PlanSourceManual},
		{"lapsed@example.com", "free", "cus_456", PlanSourceManual},
		{"selfhost@example.com", "self-hosted", "cus_789", PlanSourceManual},
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		u := createTestUser(t, s, r.email, r.email, "s3cret")
		ids[i] = u.ID
		if _, err := s.db.Exec(s.q(`UPDATE users SET plan = ?, stripe_customer_id = ?, plan_source = 'manual' WHERE id = ?`),
			r.plan, r.customer, u.ID); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.db.Exec(backfill); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	for i, r := range rows {
		got, err := s.GetUser(ids[i])
		if err != nil {
			t.Fatal(err)
		}
		if got.PlanSource != r.want {
			t.Errorf("%s (plan %q, customer %q): plan_source = %q, want %q", r.email, r.plan, r.customer, got.PlanSource, r.want)
		}
	}
}

// BackfillUserPlans writes a plan, so it takes the source over as manual: a
// free user carrying a stripe source (their Stripe subscription ended) must
// not become self-hosted still labelled stripe. A row it does not touch keeps
// its source.
func TestBackfillUserPlans_TakesSourceOver(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	lapsed := createTestUser(t, s, "lapsed-backfill@example.com", "Lapsed", "s3cret")
	paying := createTestUser(t, s, "paying-backfill@example.com", "Paying", "s3cret")
	if _, err := s.db.Exec(s.q(`UPDATE users SET plan = 'free', plan_source = 'stripe' WHERE id = ?`), lapsed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE users SET plan = 'pro', plan_source = 'stripe' WHERE id = ?`), paying.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.BackfillUserPlans("self-hosted"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id, plan, source string
	}{
		{lapsed.ID, "self-hosted", PlanSourceManual},
		{paying.ID, "pro", PlanSourceStripe},
	} {
		got, err := s.GetUser(c.id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Plan != c.plan || got.PlanSource != c.source {
			t.Errorf("user %s = (%q, %q), want (%q, %q)", c.id, got.Plan, got.PlanSource, c.plan, c.source)
		}
	}
}
