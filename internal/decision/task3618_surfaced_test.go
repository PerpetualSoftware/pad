package decision

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func surfacedKeys(ds []models.ItemDecision) []string {
	keys := make([]string, 0, len(ds))
	for _, d := range ds {
		keys = append(keys, d.QuestionKey)
	}
	sort.Strings(keys)
	return keys
}

// TASK-3618: SurfacedDecisions keeps exactly the answers WorkspaceFlags would
// flag, on both sides of the threshold (AT it is surfaced), and counts the
// rest as below threshold. One item answers AT the threshold, the other just
// under it (needs_human_decision is the one surfaced question since TASK-3921; the other three are staged).
func TestTASK3618_SurfacedDecisionsThresholdBothSides(t *testing.T) {
	fx := newAttentionFixture(t)
	fx.set(AttentionNeedsHuman, AttentionThreshold)
	at := fx.item(t, fx.tasks.ID, "open")
	fx.tick(t)
	fx.set(AttentionNeedsHuman, AttentionThreshold-0.01)
	under := fx.item(t, fx.tasks.ID, "open")
	fx.tick(t)

	got, below, staleQ, err := fx.r.SurfacedDecisions(at.ID)
	if err != nil {
		t.Fatal(err)
	}
	if keys := surfacedKeys(got); len(keys) != 1 || keys[0] != AttentionNeedsHuman || !got[0].Current {
		t.Fatalf("at the threshold: surfaced = %+v, want needs_human_decision, current", got)
	}
	if below != 0 || staleQ != 0 {
		t.Fatalf("at the threshold: below=%d staleQuestion=%d, want 0 and 0", below, staleQ)
	}
	got, below, staleQ, err = fx.r.SurfacedDecisions(under.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || below != 1 || staleQ != 0 {
		t.Fatalf("under the threshold: surfaced=%v below=%d staleQuestion=%d, want none, 1, 0", surfacedKeys(got), below, staleQ)
	}

	// The dashboard's read agrees row for row: one predicate, two callers.
	flags, _, err := fx.r.WorkspaceFlags(fx.ws.ID, AttentionSetName, AttentionThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := flags[at.ID][AttentionNeedsHuman]; !ok || len(flags) != 1 {
		t.Fatalf("WorkspaceFlags = %v, disagrees with SurfacedDecisions", flags)
	}
}

// An above-threshold answer whose ITEM has moved on stays in the brief with
// current=false. The dashboard keeps it too while the re-ask is pending
// (TASK-3915), and drops it when no job is owed; the brief keeps it either
// way.
func TestTASK3618_SurfacedKeepsAnAnswerWhoseItemMovedOn(t *testing.T) {
	fx := newAttentionFixture(t)
	it := fx.item(t, fx.tasks.ID, "open")
	fx.tick(t)
	if _, err := fx.s.CreateComment(fx.ws.ID, it.ID, "", models.CommentCreate{Author: "wren", Body: "new facts"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.s.DB().Exec(`DELETE FROM decision_jobs WHERE item_id = ?`, it.ID); err != nil {
		t.Fatal(err)
	}

	got, below, _, err := fx.r.SurfacedDecisions(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].QuestionKey != AttentionNeedsHuman || got[0].Current {
		t.Fatalf("surfaced = %+v, want needs_human_decision with current=false", got)
	}
	if below != 0 {
		t.Fatalf("below = %d, want 0", below)
	}
	if flags, _, _ := fx.r.WorkspaceFlags(fx.ws.ID, AttentionSetName, AttentionThreshold); len(flags[it.ID]) != 0 {
		t.Fatalf("precondition: with no job owed the dashboard drops the moved-on answer, got %v", flags[it.ID])
	}
}

// Answers stored under the attention set's RETIRED keys (TASK-3915 /
// TASK-3924) are neither surfaced nor flagged, and the brief counts them as
// stale questions: no migration removes the rows, and none is needed. The
// rows are written by a runner whose registry still asks the old questions,
// which is exactly what the database holds from before the change.
func TestTASK3915_RetiredAttentionKeysReadAsStaleQuestions(t *testing.T) {
	fx := newAttentionFixture(t)
	old := AttentionSet()
	old.Questions["blocked"] = Noul(
		"The item's text says the work is currently blocked on something outside the item itself.",
		"an explicit dependency, wait, or blocker is named",
		"no blocker is stated; the work can start",
	)
	old.Questions["waiting_on_external"] = Noul(
		"The next step on this work item is an action by someone outside this workspace.",
		"the item is waiting on a person, team, vendor, or service outside the workspace to act",
		"the next step is an action by the workspace's own people or agents",
	)
	oldReg := NewRegistry()
	if err := oldReg.Register(old); err != nil {
		t.Fatal(err)
	}
	fx.set("blocked", 0.95)
	fx.set("waiting_on_external", 0.95)
	it := fx.item(t, fx.tasks.ID, "open")
	rOld := NewRunner(fx.s, fx.r.provider, oldReg)
	if _, err := rOld.RunOnce(context.Background(), "old", 50, time.Minute); err != nil {
		t.Fatal(err)
	}
	// Control: the old registry surfaces all three.
	if got, _, _, _ := rOld.SurfacedDecisions(it.ID); len(got) != 3 {
		t.Fatalf("control: the old registry surfaced %v, want all three keys", surfacedKeys(got))
	}

	got, below, staleQ, err := fx.r.SurfacedDecisions(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if keys := surfacedKeys(got); len(keys) != 1 || keys[0] != AttentionNeedsHuman {
		t.Fatalf("surfaced = %v, want only needs_human_decision", keys)
	}
	if below != 0 || staleQ != 2 {
		t.Fatalf("below=%d staleQuestion=%d, want 0 and 2 (blocked, waiting_on_external)", below, staleQ)
	}
	flags, _, err := fx.r.WorkspaceFlags(fx.ws.ID, AttentionSetName, AttentionThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if len(flags[it.ID]) != 1 {
		t.Fatalf("WorkspaceFlags = %v, want needs_human_decision only", flags[it.ID])
	}
}

// An answer to a reworded question is neither surfaced nor counted below
// threshold: it is a stale question, however high its probability.
func TestTASK3618_RewordedQuestionCountsAsStaleQuestion(t *testing.T) {
	fx := newAttentionFixture(t)
	it := fx.item(t, fx.tasks.ID, "open")
	fx.tick(t)

	reworded := AttentionSet()
	q := reworded.Questions[AttentionNeedsHuman]
	q.Instructions += " (reworded)"
	reworded.Questions[AttentionNeedsHuman] = q
	reg := NewRegistry()
	if err := reg.Register(reworded); err != nil {
		t.Fatal(err)
	}
	r2 := NewRunner(fx.s, fx.r.provider, reg)

	got, below, staleQ, err := r2.SurfacedDecisions(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || below != 0 || staleQ != 1 {
		t.Fatalf("surfaced=%v below=%d staleQuestion=%d, want none, 0, 1", surfacedKeys(got), below, staleQ)
	}
	// Counterfactual: the unchanged registry surfaces that same 0.9 row.
	if got, _, staleQ, _ := fx.r.SurfacedDecisions(it.ID); len(got) != 1 || staleQ != 0 {
		t.Fatalf("control: surfaced=%v staleQuestion=%d", surfacedKeys(got), staleQ)
	}
}

// Every production set that surfaces answers carries its threshold.
func TestTASK3618_ProductionSetsCarryTheirThreshold(t *testing.T) {
	want := map[string]float64{
		AttentionSetName:   AttentionThreshold,
		ConventionsSetName: ConventionThreshold,
	}
	reg, err := ProductionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for name, th := range want {
		qs, ok := reg.Get(name)
		if !ok || qs.Threshold != th {
			t.Errorf("%s: Threshold = %v, want %v", name, qs.Threshold, th)
		}
	}
}
