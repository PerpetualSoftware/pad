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
