package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/decision"
)

// The attention set on the dashboard (TASK-3118), which reads none of it since
// TASK-4643. The provider is a stub whose Noul answers are chosen by a marker
// in the item's TITLE (HUMAN-HIGH answers needs_human_decision at 0.9).
type attentionStub struct {
	mu   sync.Mutex
	fail error
}

func (p *attentionStub) Name() string  { return "stub" }
func (p *attentionStub) Model() string { return "stub-1" }
func (p *attentionStub) Ask(_ context.Context, state any, qs map[string]decision.Question) (map[string]decision.Answer, decision.Usage, error) {
	p.mu.Lock()
	fail := p.fail
	p.mu.Unlock()
	if fail != nil {
		return nil, decision.Usage{}, fail
	}
	raw, _ := state.(json.RawMessage)
	s := string(raw)
	score := func(key string) float64 {
		if key == decision.AttentionNeedsHuman && strings.Contains(s, "HUMAN-HIGH") {
			return 0.9
		}
		return 0.1
	}
	out := map[string]decision.Answer{}
	for k := range qs {
		out[k] = decision.Answer{Kind: decision.KindNoul, Noul: score(k)}
	}
	return out, decision.Usage{}, nil
}

func attachAttentionRunner(t *testing.T, srv *Server) *attentionStub {
	t.Helper()
	reg, err := decision.ProductionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	p := &attentionStub{}
	r := decision.NewRunner(srv.store, p, reg)
	srv.SetDecisionRunner(r)
	return p
}

func runDecisionTick(t *testing.T, srv *Server) {
	t.Helper()
	if _, err := srv.decisionRunner().RunOnce(context.Background(), "test", 100, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func attentionFor(resp DashboardResponse, ref string) []DashboardAttention {
	var out []DashboardAttention
	for _, a := range resp.Attention {
		if a.ItemRef == ref {
			out = append(out, a)
		}
	}
	return out
}

// No provider: no decision-derived entries and nothing degraded — the
// dashboard is what it was before this unit.
func TestDashboardAttention_NoProviderAddsNothing(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Pick vendor HUMAN-HIGH", "fields": `{"status":"open"}`})
	resp := getDashboard(t, srv, slug)
	if n := len(filterAttention(resp.Attention, "needs_human")); n != 0 {
		t.Fatalf("no provider, %d needs_human entries", n)
	}
	if containsString(resp.DegradedSections, "attention.decisions") {
		t.Fatal("no provider, yet attention.decisions is degraded")
	}
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TASK-4643: the dashboard reads no judged answer. A stored, current
// needs_human_decision answer above the old 0.7 threshold, on an open item,
// with the runner configured, makes no attention entry; a failing provider
// marks nothing degraded, since no dashboard section reads its answers.
func TestDashboardNoNeedsHumanAttention(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	p := attachAttentionRunner(t, srv)
	slug := createWSWithCollections(t, srv)
	it := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Pick vendor HUMAN-HIGH", "fields": `{"status":"open"}`})
	runDecisionTick(t, srv)

	stored := false
	for _, d := range getDecisions(t, srv, slug, it.Slug).Decisions {
		if pr, ok := decision.NoulValue(d); ok && d.QuestionKey == decision.AttentionNeedsHuman && d.Current && pr >= 0.7 {
			stored = true
		}
	}
	if !stored {
		t.Fatal("precondition: no current needs_human_decision answer at or above 0.7 is stored")
	}
	resp := getDashboard(t, srv, slug)
	if got := filterAttention(resp.Attention, "needs_human"); len(got) != 0 {
		t.Fatalf("needs_human entries = %+v, want none", got)
	}
	if got := attentionFor(resp, it.Ref); len(got) != 0 {
		t.Fatalf("the judged item has attention entries: %+v", got)
	}

	// A failing provider with an owed job: still nothing degraded.
	p.mu.Lock()
	p.fail = errors.New("upstream down")
	p.mu.Unlock()
	// A new item owes an undebounced first ask, which now fails.
	anything := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Anything", "fields": `{"status":"open"}`})
	runDecisionTick(t, srv)
	if j, err := srv.store.GetDecisionJob(anything.ID, decision.AttentionSetName); err != nil || j == nil || j.LastError == "" {
		t.Fatalf("precondition: the attention job is not recorded as failing (job=%+v, err=%v)", j, err)
	}
	resp = getDashboard(t, srv, slug)
	if containsString(resp.DegradedSections, "attention.decisions") {
		t.Fatalf("a failing provider degraded the dashboard: %v", resp.DegradedSections)
	}
	if got := filterAttention(resp.Attention, "needs_human"); len(got) != 0 {
		t.Fatalf("needs_human entries with a failing provider = %+v, want none", got)
	}
}
