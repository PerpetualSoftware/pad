package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// BUG-4022: a decision failure the store refuses is still counted, and an
// empty message is never read as a supersede.

const poisonPrefix = "POISON-"

// wantFallbackError is the exact last_error the fail-open path writes: the
// fixed text, plus the SQLSTATE on Postgres (a RAISE EXCEPTION is P0001).
// SQLite has no SQLSTATE, so the fixed text stands alone.
func wantFallbackError(s *Store) string {
	if s.dialect.Driver() == DriverPostgres {
		return unrecordableDecisionError + " (SQLSTATE P0001)"
	}
	return unrecordableDecisionError
}

// installDecisionJobTrigger adds a BEFORE UPDATE trigger on decision_jobs that
// raises. With a non-empty prefix it raises only when NEW.last_error starts
// with it; with "" it raises on every update. Installed per test database.
func installDecisionJobTrigger(t *testing.T, s *Store, prefix string) {
	t.Helper()
	if s.dialect.Driver() == DriverPostgres {
		cond := "TRUE"
		if prefix != "" {
			cond = "NEW.last_error LIKE '" + prefix + "%'"
		}
		s.mustExec(t, `CREATE FUNCTION decision_jobs_poison() RETURNS trigger AS $$ BEGIN IF `+cond+` THEN RAISE EXCEPTION 'poisoned decision_jobs update'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql`)
		s.mustExec(t, `CREATE TRIGGER decision_jobs_poison BEFORE UPDATE ON decision_jobs FOR EACH ROW EXECUTE FUNCTION decision_jobs_poison()`)
		return
	}
	when := ""
	if prefix != "" {
		when = " WHEN NEW.last_error LIKE '" + prefix + "%'"
	}
	s.mustExec(t, `CREATE TRIGGER decision_jobs_poison BEFORE UPDATE ON decision_jobs FOR EACH ROW`+when+` BEGIN SELECT RAISE(ABORT, 'poisoned decision_jobs update'); END`)
}

func claimOneAttentionJob(t *testing.T, s *Store, wsID, itemID string) DecisionJob {
	t.Helper()
	if err := s.OweDecisionJobs(wsID, "attention", []string{itemID}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimDecisionJobs("r", 10, time.Hour)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %d, %v", len(claimed), err)
	}
	return claimed[0]
}

func decisionLease(t *testing.T, s *Store, itemID string) time.Time {
	t.Helper()
	var lease string
	if err := s.db.QueryRow(s.q(`SELECT lease_expires_at FROM decision_jobs WHERE item_id = ? AND question_set = 'attention'`), itemID).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	ts, err := time.Parse(decisionTimeFormat, lease)
	if err != nil {
		t.Fatalf("lease %q: %v", lease, err)
	}
	return ts
}

func TestFailDecisionJob_RefusedMessageIsRecordedFixed(t *testing.T) {
	s, ws, col := decisionFixture(t)
	s.SetDecisionSetResolver(func(string) []string { return nil })
	item := createTestItem(t, s, ws.ID, col.ID, "Poison", "")
	j := claimOneAttentionJob(t, s, ws.ID, item.ID)
	installDecisionJobTrigger(t, s, poisonPrefix)

	before := time.Now()
	if err := s.FailDecisionJob(j, errors.New(poisonPrefix+"provider body"), time.Hour); err != nil {
		t.Fatalf("a refused message must fail open, got: %v", err)
	}
	got := mustJob(t, s, item.ID, "attention")
	if got.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", got.Attempts)
	}
	if want := wantFallbackError(s); got.LastError != want {
		t.Fatalf("last_error = %q, want %q", got.LastError, want)
	}
	lease := decisionLease(t, s, item.ID)
	if lease.Before(before.Add(time.Hour-time.Minute)) || lease.After(time.Now().Add(time.Hour+time.Minute)) {
		t.Fatalf("lease = %v, want about now+1h", lease)
	}
	if failing, err := s.DecisionJobsFailing(ws.ID, "attention"); err != nil || !failing {
		t.Fatalf("DecisionJobsFailing = %v, %v; want true", failing, err)
	}
	if again, _ := s.ClaimDecisionJobs("r2", 10, time.Hour); len(again) != 0 {
		t.Fatalf("a job inside its backoff was claimed")
	}
}

func TestFailDecisionJob_BothWritesRefusedReturnsBoth(t *testing.T) {
	s, ws, col := decisionFixture(t)
	s.SetDecisionSetResolver(func(string) []string { return nil })
	item := createTestItem(t, s, ws.ID, col.ID, "Poison", "")
	j := claimOneAttentionJob(t, s, ws.ID, item.ID)
	installDecisionJobTrigger(t, s, "")

	err := s.FailDecisionJob(j, errors.New("boom"), time.Hour)
	if err == nil {
		t.Fatal("both writes refused, but FailDecisionJob returned nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "fail decision job") || !strings.Contains(msg, "(fallback)") {
		t.Fatalf("error does not carry both failures: %v", err)
	}
	if u, ok := err.(interface{ Unwrap() []error }); !ok || len(u.Unwrap()) != 2 {
		t.Fatalf("error is not a join of two: %T %v", err, err)
	}
}

func TestFailDecisionJob_EmptyMessageIsAFailureNotASupersede(t *testing.T) {
	cases := map[string]error{"empty error": errors.New(""), "nil cause": nil}
	for name, cause := range cases {
		t.Run(name, func(t *testing.T) {
			s, ws, col := decisionFixture(t)
			s.SetDecisionSetResolver(func(string) []string { return nil })
			item := createTestItem(t, s, ws.ID, col.ID, "Empty", "")
			j := claimOneAttentionJob(t, s, ws.ID, item.ID)
			if err := s.FailDecisionJob(j, cause, time.Hour); err != nil {
				t.Fatal(err)
			}
			got := mustJob(t, s, item.ID, "attention")
			if got.Attempts != 1 || got.LastError != noDecisionErrorMessage {
				t.Fatalf("attempts=%d last_error=%q; want 1 and %q", got.Attempts, got.LastError, noDecisionErrorMessage)
			}
			if again, _ := s.ClaimDecisionJobs("r2", 10, time.Hour); len(again) != 0 {
				t.Fatalf("an empty-message failure was claimable before its backoff")
			}
			if failing, err := s.DecisionJobsFailing(ws.ID, "attention"); err != nil || !failing {
				t.Fatalf("DecisionJobsFailing = %v, %v; want true", failing, err)
			}
		})
	}
}

// The fallback may carry a SQLSTATE, but only one of the fixed five-character
// shape: anything else read off the failing error is driver text (BUG-4022,
// ruling Q1). A real Postgres server only sends well-formed codes, so the
// shape check is pinned here by direct call.
func TestDecisionSQLState(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"postgres code", &pgconn.PgError{Code: "P0001"}, "P0001"},
		{"wrapped postgres code", fmt.Errorf("fail: %w", &pgconn.PgError{Code: "22021"}), "22021"},
		{"lower case", &pgconn.PgError{Code: "p0001"}, ""},
		{"too long", &pgconn.PgError{Code: "P0001 x"}, ""},
		{"too short", &pgconn.PgError{Code: "P001"}, ""},
		{"empty", &pgconn.PgError{}, ""},
		{"not postgres", errors.New("constraint failed"), ""},
		{"nil", nil, ""},
	} {
		if got := decisionSQLState(c.err); got != c.want {
			t.Errorf("%s: decisionSQLState = %q, want %q", c.name, got, c.want)
		}
	}
}

// An error from the first UPDATE does not prove it did not commit (a reply
// lost after the commit). The fallback must then not count the attempt again,
// and FailDecisionJob must return the first error rather than report success
// (BUG-4022 r1 H1). A trigger cannot both commit a write and fail it, since
// an error raised in a trigger aborts its own statement, so the row is put in
// the state a committed first write leaves (attempts moved, its message
// stored) and the poison trigger then refuses FailDecisionJob's first UPDATE.
func TestFailDecisionJob_FirstWriteCommittedIsNotCountedTwice(t *testing.T) {
	s, ws, col := decisionFixture(t)
	s.SetDecisionSetResolver(func(string) []string { return nil })
	item := createTestItem(t, s, ws.ID, col.ID, "Committed", "")
	j := claimOneAttentionJob(t, s, ws.ID, item.ID)
	const committed = "the message the first write stored"
	s.mustExec(t, `UPDATE decision_jobs SET attempts = attempts + 1, last_error = ? WHERE item_id = ? AND question_set = 'attention'`, committed, item.ID)
	installDecisionJobTrigger(t, s, poisonPrefix)

	err := s.FailDecisionJob(j, errors.New(poisonPrefix+"provider body"), time.Hour)
	if err == nil {
		t.Fatal("an unconfirmed failure write reported success")
	}
	if msg := err.Error(); !strings.Contains(msg, "fail decision job") || !strings.Contains(msg, "poisoned decision_jobs update") || strings.Contains(msg, "(fallback)") {
		t.Fatalf("want the first write's error alone, got: %v", err)
	}
	got := mustJob(t, s, item.ID, "attention")
	if got.Attempts != 1 || got.LastError != committed {
		t.Fatalf("attempts=%d last_error=%q; want 1 and %q (counted once, not overwritten)", got.Attempts, got.LastError, committed)
	}
}
