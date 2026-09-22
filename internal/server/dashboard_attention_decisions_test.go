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

// Content-aware attention on the dashboard (TASK-3118). The provider is a
// stub whose Noul answers are chosen by a marker in the item's TITLE, so one
// workspace can hold items the model scores on either side of the threshold.
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
		switch {
		case key == decision.AttentionNeedsHuman && strings.Contains(s, "HUMAN-HIGH"):
			return 0.9
		case key == decision.AttentionNeedsHuman && strings.Contains(s, "HUMAN-LOW"):
			return 0.69
		case key == decision.AttentionBlocked && strings.Contains(s, "TEXT-BLOCKED"):
			return 0.85
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

func TestDashboardAttention_NeedsHumanAtThreshold(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	attachAttentionRunner(t, srv)
	slug := createWSWithCollections(t, srv)
	high := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Pick vendor HUMAN-HIGH", "fields": `{"status":"open"}`})
	low := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Pick vendor HUMAN-LOW", "fields": `{"status":"open"}`})
	runDecisionTick(t, srv)

	resp := getDashboard(t, srv, slug)
	got := filterAttention(attentionFor(resp, high.Ref), "needs_human")
	if len(got) != 1 {
		t.Fatalf("needs_human entries for the 0.9 item = %d, want 1: %+v", len(got), resp.Attention)
	}
	if !strings.Contains(got[0].Reason, "90%") {
		t.Errorf("reason %q does not name the probability", got[0].Reason)
	}
	if n := len(filterAttention(attentionFor(resp, low.Ref), "needs_human")); n != 0 {
		t.Errorf("an item at 0.69 (below the 0.7 threshold) was surfaced")
	}
	if resp.Degraded {
		t.Errorf("a healthy provider marked the dashboard degraded: %v", resp.DegradedSections)
	}
}

// The graph stays primary: an item it already flags gets no second, text-
// derived "blocked" entry (which would also collide on the web's
// `${type}:${item_slug}` key). An item only the text flags does get one,
// with a reason distinguishable from the graph's.
func TestDashboardAttention_TextBlockedJoinsOnlyWhenGraphIsSilent(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	attachAttentionRunner(t, srv)
	slug := createWSWithCollections(t, srv)
	blocker := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Upstream", "fields": `{"status":"open"}`})
	both := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Graph and TEXT-BLOCKED", "fields": `{"status":"open"}`})
	textOnly := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Only TEXT-BLOCKED", "fields": `{"status":"open"}`})
	createBlocksLink(t, srv, slug, blocker.Slug, both.ID)
	runDecisionTick(t, srv)

	resp := getDashboard(t, srv, slug)
	b := filterAttention(attentionFor(resp, both.Ref), "blocked")
	if len(b) != 1 || !strings.HasPrefix(b[0].Reason, "Blocked by Upstream") {
		t.Fatalf("graph-blocked item: blocked entries = %+v, want exactly the graph's", b)
	}
	tb := filterAttention(attentionFor(resp, textOnly.Ref), "blocked")
	if len(tb) != 1 {
		t.Fatalf("text-only blocked item: %d blocked entries, want 1", len(tb))
	}
	if strings.HasPrefix(tb[0].Reason, "Blocked by") || !strings.Contains(tb[0].Reason, "85%") {
		t.Errorf("text-derived reason %q is not distinguishable from the graph's or omits the probability", tb[0].Reason)
	}
}

// A stored answer does not outlive the item being open: the same item is
// surfaced while open (the counterfactual leg) and not once done.
func TestDashboardAttention_TerminalItemIsNotSurfaced(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	attachAttentionRunner(t, srv)
	slug := createWSWithCollections(t, srv)
	it := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Sign contract HUMAN-HIGH", "fields": `{"status":"open"}`})
	runDecisionTick(t, srv)
	if len(filterAttention(attentionFor(getDashboard(t, srv, slug), it.Ref), "needs_human")) != 1 {
		t.Fatal("precondition: the open item is not surfaced")
	}
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+it.Slug, map[string]interface{}{"fields": `{"status":"done"}`})
	if rr.Code != 200 {
		t.Fatalf("mark done: %d %s", rr.Code, rr.Body.String())
	}
	runDecisionTick(t, srv)
	if got := attentionFor(getDashboard(t, srv, slug), it.Ref); len(got) != 0 {
		t.Fatalf("a done item is still in attention: %+v", got)
	}
}

func TestDashboardAttention_FailingProviderMarksDegraded(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	p := attachAttentionRunner(t, srv)
	slug := createWSWithCollections(t, srv)
	p.fail = errors.New("upstream down")
	createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Anything", "fields": `{"status":"open"}`})
	runDecisionTick(t, srv)

	resp := getDashboard(t, srv, slug)
	if !resp.Degraded || !containsString(resp.DegradedSections, "attention.decisions") {
		t.Fatalf("degraded=%v sections=%v, want attention.decisions", resp.Degraded, resp.DegradedSections)
	}
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
