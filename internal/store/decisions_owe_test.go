package store

import (
	"github.com/PerpetualSoftware/pad/internal/models"
	"testing"
	"time"
)

// TASK-3921: OweDecisionJobs and the backfill candidate query. Both backends
// via testStore.

func TestOweDecisionJobs_DebounceAndCoalescing(t *testing.T) {
	s, ws, col := debounceFixture(t)
	// Install a resolver that owes nothing, so the creations below leave no jobs.
	s.SetDecisionSetResolver(func(string) []string { return nil })
	recent := createTestItem(t, s, ws.ID, col.ID, "Recent", "")
	old := createTestItem(t, s, ws.ID, col.ID, "Old", "")
	fresh := createTestItem(t, s, ws.ID, col.ID, "Fresh", "")
	evaluated(t, s, ws, recent.ID, "triage", time.Now().Add(-10*time.Minute))
	evaluated(t, s, ws, old.ID, "triage", time.Now().Add(-2*time.Hour))
	if err := s.OweDecisionJobs(ws.ID, "triage", []string{recent.ID, old.ID, fresh.ID}); err != nil {
		t.Fatal(err)
	}
	if j := mustJob(t, s, recent.ID, "triage"); j.NotBefore == "" {
		t.Error("an item evaluated 10m ago was owed with no not_before")
	}
	if j := mustJob(t, s, old.ID, "triage"); j.NotBefore != "" {
		t.Errorf("an item evaluated 2h ago got not_before %q", j.NotBefore)
	}
	if j := mustJob(t, s, fresh.ID, "triage"); j.NotBefore != "" {
		t.Errorf("a never-evaluated item got not_before %q", j.NotBefore)
	}
	// Owing again coalesces into the same row.
	if err := s.OweDecisionJobs(ws.ID, "triage", []string{recent.ID}); err != nil {
		t.Fatal(err)
	}
	if n := countDecisionJobs(t, s); n != 3 {
		t.Errorf("%d job rows after a repeat; want 3", n)
	}
	if j := mustJob(t, s, recent.ID, "triage"); j.Generation != 2 {
		t.Errorf("generation = %d; want 2", j.Generation)
	}
}

func TestItemsMissingDecisionKey(t *testing.T) {
	s, ws, col := decisionFixture(t)
	ask := func(itemID string, keys ...string) {
		for _, k := range keys {
			if err := s.InsertItemDecisions(ws.ID, []models.ItemDecision{{
				ItemID: itemID, QuestionSet: "attention", QuestionKey: k, Kind: "noul",
				Answer: []byte(`{"type":"noul","noul":0.5}`), Provider: "fake", Model: "m",
				StateHash: "h", QuestionHash: "q", ItemSeq: 1, EvaluatedAt: decisionTime(time.Now()),
			}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	onlyNeeds := createTestItem(t, s, ws.ID, col.ID, "A", "")
	both := createTestItem(t, s, ws.ID, col.ID, "B", "")
	none := createTestItem(t, s, ws.ID, col.ID, "C", "")
	gone := createTestItem(t, s, ws.ID, col.ID, "D", "")
	otherSet := createTestItem(t, s, ws.ID, col.ID, "E", "")
	ask(onlyNeeds.ID, "needs_human_decision")
	ask(both.ID, "needs_human_decision", "effort")
	ask(gone.ID, "needs_human_decision")
	if err := s.InsertItemDecisions(ws.ID, []models.ItemDecision{{
		ItemID: otherSet.ID, QuestionSet: "conventions", QuestionKey: "needs_human_decision", Kind: "noul",
		Answer: []byte(`{"type":"noul","noul":0.5}`), Provider: "fake", Model: "m",
		StateHash: "h", QuestionHash: "q", ItemSeq: 1, EvaluatedAt: decisionTime(time.Now()),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteItem(gone.ID); err != nil {
		t.Fatal(err)
	}
	_ = none
	got, err := s.ItemsMissingDecisionKey("attention", "needs_human_decision", "effort")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ItemID != onlyNeeds.ID || got[0].WorkspaceID != ws.ID {
		t.Fatalf("candidates = %+v; want only item A", got)
	}
}

// CountClaimableDecisionJobs counts what ClaimDecisionJobs could take now: not
// a claimed job, not a debounced one, not a retired one, nor another item's.
func TestCountClaimableDecisionJobs(t *testing.T) {
	s, ws, col := debounceFixture(t)
	s.SetDecisionSetResolver(func(string) []string { return nil })
	recent := createTestItem(t, s, ws.ID, col.ID, "Recent", "")
	a := createTestItem(t, s, ws.ID, col.ID, "A", "")
	b := createTestItem(t, s, ws.ID, col.ID, "B", "")
	outside := createTestItem(t, s, ws.ID, col.ID, "Outside", "")
	evaluated(t, s, ws, recent.ID, "triage", time.Now().Add(-10*time.Minute))
	if err := s.OweDecisionJobs(ws.ID, "triage", []string{recent.ID, a.ID, b.ID, outside.ID}); err != nil {
		t.Fatal(err)
	}
	ids := []string{recent.ID, a.ID, b.ID}
	count := func(set string, at time.Time) int {
		t.Helper()
		n, err := s.CountClaimableDecisionJobs(set, ids, at)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	now := time.Now()
	// recent is debounced; outside is not asked about; the other set has none.
	if n := count("triage", now); n != 2 {
		t.Fatalf("claimable = %d; want 2 (a, b; recent is debounced)", n)
	}
	if n := count("other", now); n != 0 {
		t.Fatalf("another set counted %d", n)
	}
	if n, err := s.CountClaimableDecisionJobs("triage", nil, now); err != nil || n != 0 {
		t.Fatalf("no ids = %d, %v", n, err)
	}
	// Claim everything claimable (a, b, outside): none is claimable now.
	jobs, err := s.ClaimDecisionJobs("r", 10, time.Hour)
	if err != nil || len(jobs) != 3 {
		t.Fatalf("claim: %v %v", jobs, err)
	}
	if n := count("triage", now); n != 0 {
		t.Fatalf("after claiming claimable = %d; want 0 (claimed and debounced)", n)
	}
	// A lease that has run out makes the claimed jobs claimable again (a, b).
	if n := count("triage", now.Add(2*time.Hour)); n != 3 {
		t.Fatalf("expired leases + passed debounce claimable = %d; want 3", n)
	}
	// Retire them: none claimable now.
	for _, j := range jobs {
		if err := s.CompleteDecisionJob(j); err != nil {
			t.Fatal(err)
		}
	}
	if n := count("triage", now); n != 0 {
		t.Fatalf("after completing claimable = %d; want 0", n)
	}
	// Two hours on, only recent (debounce passed) remains.
	if n := count("triage", now.Add(2*time.Hour)); n != 1 {
		t.Fatalf("two hours on claimable = %d; want 1", n)
	}
}
