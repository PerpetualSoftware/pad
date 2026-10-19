package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The re-ask debounce (TASK-3916): decision_jobs.not_before. Runs on both
// backends via testStore (PAD_TEST_POSTGRES_URL selects Postgres).

const testDebounce = time.Hour

// debounceFixture installs a "triage" set with an hour's window and a
// "per-comment" set with none.
func debounceFixture(t *testing.T, sets ...string) (*Store, *models.Workspace, *models.Collection) {
	t.Helper()
	s, ws, col := decisionFixture(t)
	if len(sets) == 0 {
		sets = []string{"triage"}
	}
	s.SetDecisionSetResolver(func(string) []string { return sets })
	s.SetDecisionDebounce(func(set string) time.Duration {
		if set == "per-comment" {
			return 0
		}
		return testDebounce
	})
	return s, ws, col
}

// evaluated stores one answer for (item, set) at `at`, as the runner does when
// it completes an evaluation.
func evaluated(t *testing.T, s *Store, ws *models.Workspace, itemID, set string, at time.Time) {
	t.Helper()
	if err := s.InsertItemDecisions(ws.ID, []models.ItemDecision{{
		ItemID: itemID, QuestionSet: set, QuestionKey: "k", Kind: "noul",
		Answer: []byte(`{"type":"noul","noul":0.5}`), Provider: "fake", Model: "m",
		StateHash: "h-" + at.Format(time.RFC3339Nano), QuestionHash: "q", ItemSeq: 1, EvaluatedAt: decisionTime(at),
	}}); err != nil {
		t.Fatal(err)
	}
}

func claimN(t *testing.T, s *Store) []DecisionJob {
	t.Helper()
	c, err := s.ClaimDecisionJobs("r", 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func retitle(t *testing.T, s *Store, itemID, title string) {
	t.Helper()
	if _, err := s.UpdateItem(itemID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
}

// A first evaluation is never delayed: an item with no answers for the set is
// claimable at once, with not_before NULL.
func TestDecisionJobs_FirstEvaluationNotDebounced(t *testing.T) {
	s, ws, col := debounceFixture(t)
	item := createTestItem(t, s, ws.ID, col.ID, "Fresh", "")
	if j := mustJob(t, s, item.ID, "triage"); j.NotBefore != "" {
		t.Fatalf("a first evaluation was given not_before %q", j.NotBefore)
	}
	if c := claimN(t, s); len(c) != 1 {
		t.Fatalf("claimed %d, want the first evaluation at once", len(c))
	}
}

// A write inside the window after an evaluation owes a job that is NOT
// claimable until latest evaluation + window. The control legs: the same job
// IS claimable once the window has passed (the clock is moved by aging the
// evaluation), and a write after a window already past is claimable at once.
func TestDecisionJobs_DebounceDefersReask(t *testing.T) {
	s, ws, col := debounceFixture(t)
	item := createTestItem(t, s, ws.ID, col.ID, "Busy", "")
	c := claimN(t, s)
	if len(c) != 1 {
		t.Fatalf("precondition: claim the first job, got %d", len(c))
	}
	at := time.Now().Add(-10 * time.Minute)
	evaluated(t, s, ws, item.ID, "triage", at)
	if err := s.CompleteDecisionJob(c[0]); err != nil {
		t.Fatal(err)
	}

	retitle(t, s, item.ID, "Busy, edited")
	j := mustJob(t, s, item.ID, "triage")
	if want := decisionTime(at.Add(testDebounce)); j.NotBefore != want {
		t.Fatalf("not_before = %q, want latest evaluation + window %q", j.NotBefore, want)
	}
	if c := claimN(t, s); len(c) != 0 {
		t.Fatalf("a job inside its window was claimed: %+v", c)
	}

	// The window passes: age the job's not_before into the past.
	if _, err := s.db.Exec(s.q(`UPDATE decision_jobs SET not_before = ? WHERE item_id = ?`),
		decisionTime(time.Now().Add(-time.Second)), item.ID); err != nil {
		t.Fatal(err)
	}
	if c := claimN(t, s); len(c) != 1 {
		t.Fatalf("a job whose window has passed was not claimed (%d)", len(c))
	}

	// An evaluation older than the window: a new write is claimable at once.
	other := createTestItem(t, s, ws.ID, col.ID, "Quiet", "")
	if c := claimN(t, s); len(c) != 1 {
		t.Fatalf("precondition: claim the second item's first job, got %d", len(c))
	} else {
		evaluated(t, s, ws, other.ID, "triage", time.Now().Add(-2*testDebounce))
		if err := s.CompleteDecisionJob(c[0]); err != nil {
			t.Fatal(err)
		}
	}
	retitle(t, s, other.ID, "Quiet, edited")
	if j := mustJob(t, s, other.ID, "triage"); j.NotBefore != "" {
		t.Fatalf("a window already past gave not_before %q", j.NotBefore)
	}
}

// Racing writes coalesce: the second enqueue keeps the first not_before
// (COALESCE(old, new)) rather than pushing it back, so a stream of comments
// cannot postpone the re-ask indefinitely.
func TestDecisionJobs_DebounceCoalescesRacingComments(t *testing.T) {
	s, ws, col := debounceFixture(t)
	item := createTestItem(t, s, ws.ID, col.ID, "Chatty", "")
	c := claimN(t, s)
	at := time.Now().Add(-30 * time.Minute)
	evaluated(t, s, ws, item.ID, "triage", at)
	if err := s.CompleteDecisionJob(c[0]); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Author: "a", Body: "one"}); err != nil {
		t.Fatal(err)
	}
	first := mustJob(t, s, item.ID, "triage")
	if first.NotBefore == "" {
		t.Fatal("precondition: the first comment's job is not debounced")
	}
	// A newer evaluation lands (as if another path answered), then a second
	// comment: its computed not_before would be LATER, and must not win.
	evaluated(t, s, ws, item.ID, "triage", time.Now())
	if _, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Author: "a", Body: "two"}); err != nil {
		t.Fatal(err)
	}
	second := mustJob(t, s, item.ID, "triage")
	if second.Generation != first.Generation+1 {
		t.Fatalf("generation %d -> %d, want one coalesced row bumped once", first.Generation, second.Generation)
	}
	if second.NotBefore != first.NotBefore {
		t.Fatalf("not_before moved from %q to %q under a racing comment", first.NotBefore, second.NotBefore)
	}
	if n := countDecisionJobs(t, s); n != 1 {
		t.Fatalf("%d job rows, want 1", n)
	}
}

// A write that lands DURING an evaluation leaves the row behind (the
// generation rule). Its enqueue read the latest evaluation BEFORE the run's
// answers landed, so it found no window; the superseded completion recomputes
// not_before from the answers the run just stored, so the newer state waits a
// window after this run instead of being asked on the very next tick.
func TestDecisionJobs_SupersededCompletionSetsNotBefore(t *testing.T) {
	s, ws, col := debounceFixture(t)
	item := createTestItem(t, s, ws.ID, col.ID, "Race", "")
	c := claimN(t, s)
	retitle(t, s, item.ID, "Race, edited mid-evaluation")
	if j := mustJob(t, s, item.ID, "triage"); j.NotBefore != "" {
		t.Fatalf("precondition: the mid-run enqueue found an evaluation (%q)", j.NotBefore)
	}
	// Backdated so a now+window mutant differs by minutes, not microseconds.
	ran := time.Now().Add(-5 * time.Minute)
	evaluated(t, s, ws, item.ID, "triage", ran)
	if err := s.CompleteDecisionJob(c[0]); err != nil {
		t.Fatal(err)
	}
	j := mustJob(t, s, item.ID, "triage")
	if j.Generation != 2 || j.ClaimedBy != "" {
		t.Fatalf("superseded completion: generation=%d claimed_by=%q, want 2 and released", j.Generation, j.ClaimedBy)
	}
	if want := decisionTime(ran.Add(testDebounce)); j.NotBefore != want {
		t.Fatalf("not_before = %q, want the run's evaluation + window %q", j.NotBefore, want)
	}
	if c := claimN(t, s); len(c) != 0 {
		t.Fatalf("the superseding generation was claimable on the next tick: %+v", c)
	}
}

// A superseded run that STORED nothing (its state was already answered)
// recomputes from the older evaluation and invents no delay (ruling
// ae2e0a90): not_before is that evaluation plus the window while it is open,
// and NULL once it has passed, never this completion's now plus the window.
// One subtest per leg, so each can go red on its own.
func TestDecisionJobs_SupersededNoCallKeepsTheOlderWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		want func(older time.Time) string
	}{
		{"window past", 3 * testDebounce, func(time.Time) string { return "" }},
		{"window open", testDebounce / 2, func(older time.Time) string { return decisionTime(older.Add(testDebounce)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ws, col := debounceFixture(t)
			item := createTestItem(t, s, ws.ID, col.ID, "Idle", "")
			older := time.Now().Add(-tc.age)
			evaluated(t, s, ws, item.ID, "triage", older)
			c := claimN(t, s)
			if len(c) != 1 {
				t.Fatalf("precondition: claim the item, got %d", len(c))
			}
			retitle(t, s, item.ID, "Idle, edited")
			if err := s.CompleteDecisionJob(c[0]); err != nil {
				t.Fatal(err)
			}
			j := mustJob(t, s, item.ID, "triage")
			if want := tc.want(older); j.NotBefore != want || j.ClaimedBy != "" {
				t.Fatalf("not_before=%q claimed_by=%q, want %q and released", j.NotBefore, j.ClaimedBy, want)
			}
		})
	}
}

// The window runs from the LATEST evaluation, not the first: an item asked
// two hours ago and again ten minutes ago is inside its window.
func TestDecisionJobs_DebounceUsesLatestEvaluation(t *testing.T) {
	s, ws, col := debounceFixture(t)
	item := createTestItem(t, s, ws.ID, col.ID, "Twice", "")
	c := claimN(t, s)
	if len(c) != 1 {
		t.Fatalf("precondition: claim the first job, got %d", len(c))
	}
	evaluated(t, s, ws, item.ID, "triage", time.Now().Add(-2*testDebounce))
	latest := time.Now().Add(-10 * time.Minute)
	evaluated(t, s, ws, item.ID, "triage", latest)
	if err := s.CompleteDecisionJob(c[0]); err != nil {
		t.Fatal(err)
	}
	retitle(t, s, item.ID, "Twice, edited")
	if j := mustJob(t, s, item.ID, "triage"); j.NotBefore != decisionTime(latest.Add(testDebounce)) {
		t.Fatalf("not_before = %q, want the latest evaluation + window %q", j.NotBefore, decisionTime(latest.Add(testDebounce)))
	}
}

// A set with no window (a per-comment set) is never debounced, beside one
// that is: the window is per set, not global.
func TestDecisionJobs_PerCommentSetNotDebounced(t *testing.T) {
	s, ws, col := debounceFixture(t, "triage", "per-comment")
	item := createTestItem(t, s, ws.ID, col.ID, "Both", "")
	c := claimN(t, s)
	if len(c) != 2 {
		t.Fatalf("precondition: claim both first jobs, got %d", len(c))
	}
	now := time.Now().Add(-time.Minute)
	evaluated(t, s, ws, item.ID, "triage", now)
	evaluated(t, s, ws, item.ID, "per-comment", now)
	for _, j := range c {
		if err := s.CompleteDecisionJob(j); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Author: "a", Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	if j := mustJob(t, s, item.ID, "per-comment"); j.NotBefore != "" {
		t.Fatalf("the per-comment set was debounced to %q", j.NotBefore)
	}
	if j := mustJob(t, s, item.ID, "triage"); j.NotBefore == "" {
		t.Fatal("control: the item-level set beside it was not debounced")
	}
	got := claimN(t, s)
	if len(got) != 1 || got[0].QuestionSet != "per-comment" {
		t.Fatalf("claimed %+v, want only the per-comment job", got)
	}
}
