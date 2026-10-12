package decision

import (
	"sort"
	"testing"

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
// rest as below threshold.
func TestTASK3618_SurfacedDecisionsThresholdBothSides(t *testing.T) {
	fx := newAttentionFixture(t)
	fx.set(AttentionNeedsHuman, 0.9)
	fx.set(AttentionBlocked, AttentionThreshold)
	fx.set(AttentionWaitingExternal, AttentionThreshold-0.01)
	it := fx.item(t, fx.tasks.ID, "open")
	fx.tick(t)

	got, below, staleQ, err := fx.r.SurfacedDecisions(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if keys := surfacedKeys(got); len(keys) != 2 || keys[0] != AttentionBlocked || keys[1] != AttentionNeedsHuman {
		t.Fatalf("surfaced = %v, want [blocked needs_human_decision]", keys)
	}
	if below != 1 || staleQ != 0 {
		t.Fatalf("below=%d staleQuestion=%d, want 1 and 0", below, staleQ)
	}
	for _, d := range got {
		if !d.Current {
			t.Fatalf("fresh answer %s not current", d.QuestionKey)
		}
	}

	// The dashboard's read agrees row for row: one predicate, two callers.
	flags, _, err := fx.r.WorkspaceFlags(fx.ws.ID, AttentionSetName, AttentionThreshold)
	if err != nil {
		t.Fatal(err)
	}
	var flagged []string
	for k := range flags[it.ID] {
		flagged = append(flagged, k)
	}
	sort.Strings(flagged)
	if len(flagged) != 2 || flagged[0] != AttentionBlocked || flagged[1] != AttentionNeedsHuman {
		t.Fatalf("WorkspaceFlags = %v, disagrees with SurfacedDecisions", flagged)
	}
}

// An above-threshold answer whose ITEM has moved on stays in the brief with
// current=false, where WorkspaceFlags drops it; a below-threshold one stays
// out either way.
func TestTASK3618_SurfacedKeepsAnAnswerWhoseItemMovedOn(t *testing.T) {
	fx := newAttentionFixture(t)
	it := fx.item(t, fx.tasks.ID, "open")
	fx.tick(t)
	if _, err := fx.s.CreateComment(fx.ws.ID, it.ID, "", models.CommentCreate{Author: "wren", Body: "new facts"}); err != nil {
		t.Fatal(err)
	}

	got, below, _, err := fx.r.SurfacedDecisions(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].QuestionKey != AttentionNeedsHuman || got[0].Current {
		t.Fatalf("surfaced = %+v, want needs_human_decision with current=false", got)
	}
	if below != 2 {
		t.Fatalf("below = %d, want 2 (blocked 0.2, waiting 0.1)", below)
	}
	if flags, _, _ := fx.r.WorkspaceFlags(fx.ws.ID, AttentionSetName, AttentionThreshold); len(flags[it.ID]) != 0 {
		t.Fatalf("precondition: the dashboard drops the moved-on answer, got %v", flags[it.ID])
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
	if len(got) != 0 || below != 2 || staleQ != 1 {
		t.Fatalf("surfaced=%v below=%d staleQuestion=%d, want none, 2, 1", surfacedKeys(got), below, staleQ)
	}
	// Counterfactual: the unchanged registry surfaces that same 0.9 row.
	if got, _, staleQ, _ := fx.r.SurfacedDecisions(it.ID); len(got) != 1 || staleQ != 0 {
		t.Fatalf("control: surfaced=%v staleQuestion=%d", surfacedKeys(got), staleQ)
	}
}

// Every production set that surfaces answers carries its threshold.
func TestTASK3618_ProductionSetsCarryTheirThreshold(t *testing.T) {
	want := map[string]float64{
		AttentionSetName:           AttentionThreshold,
		ConventionsSetName:         ConventionThreshold,
		ConventionsCommentsSetName: ConventionThreshold,
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
