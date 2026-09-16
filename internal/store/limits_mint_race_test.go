package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2808, store half. A limited user-scoped mint (WithPlanLimit) decides on
// the count it takes under the owner lock, inside its own transaction.
//
// Each case builds the race rather than sampling it. A competing transaction
// takes the same owner lock a limited mint takes and inserts a counted row
// WITHOUT committing. The mint under test must wait for it, and must then
// refuse, because the competitor's committed row fills the cap. The control
// holds the same lock but inserts nothing that counts; the mint waits, then
// succeeds.
//
// Postgres: the wait is observed in pg_stat_activity (waitForLockWait) and is
// never inferred from time. SQLite: the competitor is a BEGIN IMMEDIATE
// transaction. SQLite exposes no lock-wait view, so the pause before the
// commit only gives an unserialised (count-on-the-pool) version time to read
// the committed state and admit. The pause can therefore only make the test
// MISS such a version, never fail a correct one: a correct mint cannot finish
// while the competitor holds the write lock.

type limitedMint struct {
	name    string
	feature string
	// competitor inserts one row the feature counts, on the given transaction.
	competitor func(t *testing.T, s *Store, tx execQueryer, owner string)
	// mint runs the limited insert under test.
	mint func(s *Store, owner string, src *models.WorkspaceExport) error
	// needle is a fragment of the SQL the mint waits on, for pg_stat_activity.
	needle string
}

var limitedMints = []limitedMint{
	{
		name:    "CreateWorkspace",
		feature: "workspaces",
		competitor: func(t *testing.T, s *Store, tx execQueryer, owner string) {
			if _, err := s.createWorkspaceQ(tx, models.WorkspaceCreate{Name: "competitor", OwnerID: owner}); err != nil {
				t.Fatalf("competing workspace insert: %v", err)
			}
		},
		mint: func(s *Store, owner string, _ *models.WorkspaceExport) error {
			_, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "under-test", OwnerID: owner}, WithPlanLimit())
			return err
		},
		needle: "FOR NO KEY UPDATE",
	},
	{
		name:    "ImportWorkspace",
		feature: "workspaces",
		competitor: func(t *testing.T, s *Store, tx execQueryer, owner string) {
			if _, err := s.createWorkspaceQ(tx, models.WorkspaceCreate{Name: "competitor", OwnerID: owner}); err != nil {
				t.Fatalf("competing workspace insert: %v", err)
			}
		},
		mint: func(s *Store, owner string, src *models.WorkspaceExport) error {
			_, err := s.ImportWorkspace(src, "imported-under-test", owner, "", WithPlanLimit())
			return err
		},
		needle: "FOR NO KEY UPDATE",
	},
	{
		name:    "CreateAPIToken",
		feature: "api_tokens",
		competitor: func(t *testing.T, s *Store, tx execQueryer, owner string) {
			if _, err := tx.Exec(s.q(`
				INSERT INTO api_tokens (id, user_id, name, token_hash, prefix, scopes, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`),
				newID(), owner, "competitor", "hash-"+newID(), "pad_comp", `["*"]`, now()); err != nil {
				t.Fatalf("competing token insert: %v", err)
			}
		},
		mint: func(s *Store, owner string, _ *models.WorkspaceExport) error {
			_, err := s.CreateAPIToken(owner, models.APITokenCreate{Name: "under-test"}, 0, 0, WithPlanLimit())
			return err
		},
		needle: "FOR NO KEY UPDATE",
	},
}

// limitedMintFixture creates a free-plan owner, an export source for the
// import case, and sets the feature's cap to one more than the owner holds.
func limitedMintFixture(t *testing.T, s *Store, m limitedMint) (owner string, src *models.WorkspaceExport, limit int) {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{
		Email: "bug2808-" + m.name + "@example.com", Name: "L", Password: "pw-bug2808-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.SetUserPlan(u.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "source", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace(source): %v", err)
	}
	src, err = s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	current, err := s.userFeatureCountOn(s.db, u.ID, m.feature)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	limit = current + 1
	if err := s.SetUserPlanOverrides(u.ID, fmt.Sprintf(`{%q:%d}`, m.feature, limit)); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
	return u.ID, src, limit
}

func runLimitedMintRace(t *testing.T, s *Store, m limitedMint, competitorCounts bool) {
	t.Helper()
	owner, src, limit := limitedMintFixture(t, s, m)

	tx, err := s.db.Begin() // on SQLite this is BEGIN IMMEDIATE
	if err != nil {
		t.Fatalf("begin competitor: %v", err)
	}
	defer tx.Rollback()
	if s.dialect.Driver() == DriverPostgres {
		// The same lock a limited mint takes; a competing limited mint would hold it.
		if _, err := tx.Exec(s.q(`SELECT id FROM users WHERE id = ? FOR NO KEY UPDATE`), owner); err != nil {
			t.Fatalf("competitor lock: %v", err)
		}
	}
	if competitorCounts {
		m.competitor(t, s, tx, owner)
	}

	done := make(chan error, 1)
	go func() { done <- m.mint(s, owner, src) }()

	if s.dialect.Driver() == DriverPostgres {
		waitForLockWait(t, s, m.needle, done)
	} else {
		select {
		case err := <-done:
			t.Fatalf("the mint finished (err = %v) while the competitor held the write lock", err)
		case <-time.After(300 * time.Millisecond):
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit competitor: %v", err)
	}
	mintErr := <-done

	got, err := s.userFeatureCountOn(s.db, owner, m.feature)
	if err != nil {
		t.Fatalf("count after: %v", err)
	}
	var ple *PlanLimitError
	if competitorCounts {
		if !errors.As(mintErr, &ple) {
			t.Fatalf("mint err = %v, want *PlanLimitError (count %d, cap %d)", mintErr, got, limit)
		}
		if ple.Result.Feature != m.feature || ple.Result.Limit != limit || ple.Result.Current != limit {
			t.Errorf("PlanLimitError result = %+v, want feature %s at %d of %d", ple.Result, m.feature, limit, limit)
		}
		if got != limit {
			t.Errorf("%s count = %d after a refused mint, want the cap %d", m.feature, got, limit)
		}
		return
	}
	if mintErr != nil {
		t.Fatalf("control: mint err = %v, want success", mintErr)
	}
	if got != limit {
		t.Errorf("control: %s count = %d, want exactly the cap %d", m.feature, got, limit)
	}
}

func TestLimitedMint_WaitsForCompetitor_ThenRefuses(t *testing.T) {
	for _, m := range limitedMints {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			runLimitedMintRace(t, testStore(t), m, true)
		})
	}
}

func TestLimitedMint_WaitsForNonCountingHolder_ThenAdmits(t *testing.T) {
	for _, m := range limitedMints {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			runLimitedMintRace(t, testStore(t), m, false)
		})
	}
}

// Without WithPlanLimit the insert is unchanged: no lock, no count, no refusal
// even past the cap. Self-hosted callers, the CLI db import and autoCreate
// depend on that.
func TestLimitedMint_WithoutOption_NotEnforced(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u, err := s.CreateUser(models.UserCreate{Email: "bug2808-noopt@example.com", Name: "N", Password: "pw-bug2808-12345"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.SetUserPlan(u.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan: %v", err)
	}
	if err := s.SetUserPlanOverrides(u.ID, `{"workspaces":0,"api_tokens":0}`); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
	if _, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "free-anyway", OwnerID: u.ID}); err != nil {
		t.Errorf("CreateWorkspace without the option: %v", err)
	}
	if _, err := s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "free-anyway"}, 0, 0); err != nil {
		t.Errorf("CreateAPIToken without the option: %v", err)
	}
	// And with it, the same caps refuse.
	var ple *PlanLimitError
	if _, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "refused", OwnerID: u.ID}, WithPlanLimit()); !errors.As(err, &ple) {
		t.Errorf("CreateWorkspace with the option at cap: err = %v, want *PlanLimitError", err)
	}
	if _, err := s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "refused"}, 0, 0, WithPlanLimit()); !errors.As(err, &ple) {
		t.Errorf("CreateAPIToken with the option at cap: err = %v, want *PlanLimitError", err)
	}
}
