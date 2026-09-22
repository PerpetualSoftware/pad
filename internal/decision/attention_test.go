package decision

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// Attention question set tests (TASK-3118).

// The eval's text, copied from eval2.py section D as a LITERAL — not read
// back through AttentionSet — so a rewording in attention.go fails here
// rather than being compared against itself.
func TestAttentionSet_QuestionsAreTheEvalsVerbatim(t *testing.T) {
	want := map[string]Question{
		"needs_human_decision": {
			Kind:         KindNoul,
			Instructions: "This work item is waiting on a judgment, approval, credential, or decision that only a human owner can supply, rather than on engineering work an AI agent could do unattended.",
			TrueDesc:     "the next step requires a human's decision, approval, sign-off, credentials, money, an external relationship, or hardware the agent cannot reach",
			FalseDesc:    "the next step is implementable by an engineer or AI agent from the description alone",
		},
		"blocked": {
			Kind:         KindNoul,
			Instructions: "The item's text says the work is currently blocked on something outside the item itself.",
			TrueDesc:     "an explicit dependency, wait, or blocker is named",
			FalseDesc:    "no blocker is stated; the work can start",
		},
	}
	got := AttentionSet().Questions
	for key, w := range want {
		g, ok := got[key]
		if !ok {
			t.Errorf("question %q missing from the attention set", key)
			continue
		}
		if g.Kind != w.Kind || g.Instructions != w.Instructions || g.TrueDesc != w.TrueDesc || g.FalseDesc != w.FalseDesc {
			t.Errorf("question %q differs from the eval's text:\n got %+v\nwant %+v", key, g, w)
		}
	}
	if _, ok := got[AttentionWaitingExternal]; !ok {
		t.Errorf("waiting_on_external missing")
	}
	if len(got) != 3 {
		t.Errorf("attention set has %d questions, want 3", len(got))
	}
}

func TestProductionRegistryRegisters(t *testing.T) {
	reg, err := ProductionRegistry()
	if err != nil {
		t.Fatalf("ProductionRegistry: %v", err)
	}
	if _, ok := reg.Get(AttentionSetName); !ok {
		t.Fatal("attention set not registered")
	}
	if sets := reg.SetsFor("tasks"); len(sets) != 1 || sets[0] != AttentionSetName {
		t.Fatalf("SetsFor(tasks) = %v, want [attention]", sets)
	}
}

// attentionFixture runs the PRODUCTION registry against a fake provider
// whose Noul answers the test sets per question key.
type attentionFixture struct {
	s     *store.Store
	r     *Runner
	f     *fake
	ws    *models.Workspace
	tasks *models.Collection

	mu      sync.Mutex
	answers map[string]float64
}

const statusSchema = `{"fields":[{"key":"status","type":"select","options":["open","in-progress","done"],"terminal_options":["done"]}]}`

func newAttentionFixture(t *testing.T) *attentionFixture {
	t.Helper()
	fx := &attentionFixture{answers: map[string]float64{
		AttentionNeedsHuman: 0.9, AttentionBlocked: 0.2, AttentionWaitingExternal: 0.1,
	}}
	fx.s = storetest.NewSQLite(t)
	var err error
	fx.ws, err = fx.s.CreateWorkspace(models.WorkspaceCreate{Name: "Attention"})
	if err != nil {
		t.Fatal(err)
	}
	fx.tasks, err = fx.s.CreateCollection(fx.ws.ID, models.CollectionCreate{Name: "Tasks", Schema: statusSchema})
	if err != nil {
		t.Fatal(err)
	}
	p, f := newFake(t, func(f *fake, req wireRequest, raw []byte, w http.ResponseWriter) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		out := map[string]any{}
		for key := range req.Questions {
			out[key] = map[string]any{"type": "noul", "noul": fx.answers[key]}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-1.13.0", "answers": out,
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 5},
		})
	})
	fx.f = f
	reg, err := ProductionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	fx.r = NewRunner(fx.s, p, reg)
	fx.r.Install(fx.s)
	return fx
}

func (fx *attentionFixture) set(key string, p float64) {
	fx.mu.Lock()
	fx.answers[key] = p
	fx.mu.Unlock()
}

func (fx *attentionFixture) tick(t *testing.T) {
	t.Helper()
	if _, err := fx.r.RunOnce(context.Background(), "tick", 50, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func (fx *attentionFixture) item(t *testing.T, collID, status string) *models.Item {
	t.Helper()
	it, err := fx.s.CreateItem(fx.ws.ID, collID, models.ItemCreate{Title: "Pick a vendor", Fields: `{"status":"` + status + `"}`})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// A terminal item and a system-collection item are never sent; the open item
// beside them is (the leg that shows the fixture CAN ask). Reopening the
// terminal item makes it eligible again.
func TestAttention_OnlyOpenItemsInUserCollectionsAreAsked(t *testing.T) {
	fx := newAttentionFixture(t)
	sys, err := fx.s.CreateCollection(fx.ws.ID, models.CollectionCreate{Name: "Conventions", Schema: statusSchema, IsSystem: true})
	if err != nil {
		t.Fatal(err)
	}
	open := fx.item(t, fx.tasks.ID, "open")
	done := fx.item(t, fx.tasks.ID, "done")
	rule := fx.item(t, sys.ID, "open")
	for _, it := range []*models.Item{open, done, rule} {
		if j, _ := fx.s.GetDecisionJob(it.ID, AttentionSetName); j == nil {
			t.Fatalf("precondition: no job owed for %s — the gate would be untested", it.ID)
		}
	}
	fx.tick(t)
	if len(fx.f.requests) != 1 {
		t.Fatalf("provider called %d times, want 1 (the open item only)", len(fx.f.requests))
	}
	for _, it := range []*models.Item{done, rule} {
		if j, _ := fx.s.GetDecisionJob(it.ID, AttentionSetName); j != nil {
			t.Errorf("ineligible item %s kept its job: %+v", it.ID, j)
		}
	}
	nouls, _, err := fx.r.WorkspaceNouls(fx.ws.ID, AttentionSetName)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := nouls[open.ID]; !ok || len(nouls) != 1 {
		t.Fatalf("answers stored for %v, want only the open item", nouls)
	}

	reopened := `{"status":"open"}`
	if _, err := fx.s.UpdateItem(done.ID, models.ItemUpdate{Fields: &reopened}); err != nil {
		t.Fatal(err)
	}
	fx.tick(t)
	if len(fx.f.requests) != 2 {
		t.Fatalf("a reopened item was not asked (provider calls = %d, want 2)", len(fx.f.requests))
	}
}

// WorkspaceNouls returns the LATEST answer per item and key, scoped to the
// workspace, and drops answers to a question that has since been reworded.
func TestWorkspaceNouls_LatestCurrentQuestionAndWorkspaceScoped(t *testing.T) {
	fx := newAttentionFixture(t)
	it := fx.item(t, fx.tasks.ID, "open")
	fx.tick(t)

	fx.set(AttentionNeedsHuman, 0.3)
	if _, err := fx.s.CreateComment(fx.ws.ID, it.ID, "", models.CommentCreate{Author: "wren", Body: "Dave picked vendor B"}); err != nil {
		t.Fatal(err)
	}
	fx.tick(t)
	nouls, failing, err := fx.r.WorkspaceNouls(fx.ws.ID, AttentionSetName)
	if err != nil || failing {
		t.Fatalf("WorkspaceNouls: failing=%v err=%v", failing, err)
	}
	if got := nouls[it.ID][AttentionNeedsHuman]; got != 0.3 {
		t.Fatalf("needs_human = %v, want the LATEST answer 0.3 (first was 0.9)", got)
	}

	other, err := fx.s.CreateWorkspace(models.WorkspaceCreate{Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	if n, _, _ := fx.r.WorkspaceNouls(other.ID, AttentionSetName); len(n) != 0 {
		t.Fatalf("another workspace saw %v", n)
	}

	// Same store and rows, a registry whose needs_human question is worded
	// differently: that key's stored answer no longer answers it.
	reworded := AttentionSet()
	q := reworded.Questions[AttentionNeedsHuman]
	q.Instructions += " (reworded)"
	reworded.Questions[AttentionNeedsHuman] = q
	reg := NewRegistry()
	if err := reg.Register(reworded); err != nil {
		t.Fatal(err)
	}
	r2 := NewRunner(fx.s, fx.r.provider, reg)
	n2, _, err := r2.WorkspaceNouls(fx.ws.ID, AttentionSetName)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := n2[it.ID][AttentionNeedsHuman]; ok {
		t.Error("an answer to the old wording was returned for the reworded question")
	}
	if _, ok := n2[it.ID][AttentionBlocked]; !ok {
		t.Error("an unchanged question's answer was dropped along with the reworded one")
	}

	if err := fx.s.DeleteItem(it.ID); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := fx.r.WorkspaceNouls(fx.ws.ID, AttentionSetName); len(n) != 0 {
		t.Fatalf("a deleted item's answers were returned: %v", n)
	}
}

func TestWorkspaceNouls_ReportsAFailingProvider(t *testing.T) {
	fx := newAttentionFixture(t)
	fx.item(t, fx.tasks.ID, "open")
	if _, failing, _ := fx.r.WorkspaceNouls(fx.ws.ID, AttentionSetName); failing {
		t.Fatal("precondition: failing before any attempt")
	}
	fx.f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"upstream down"}`, http.StatusServiceUnavailable)
	})
	fx.tick(t)
	if _, failing, err := fx.r.WorkspaceNouls(fx.ws.ID, AttentionSetName); err != nil || !failing {
		t.Fatalf("after a provider failure: failing=%v err=%v, want failing", failing, err)
	}
}

func TestWorkspaceNouls_NilRunner(t *testing.T) {
	var r *Runner
	n, failing, err := r.WorkspaceNouls("ws", AttentionSetName)
	if n != nil || failing || err != nil {
		t.Fatalf("nil runner: %v %v %v", n, failing, err)
	}
}
