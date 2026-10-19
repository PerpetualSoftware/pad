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
		case key == decision.AttentionNeedsHuman && strings.Contains(s, "HUMAN-EDGE"):
			return decision.AttentionThreshold
		case key == retiredBlockedKey && strings.Contains(s, "TEXT-BLOCKED"):
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

// retiredBlockedKey is the attention set's text-derived `blocked` Noul,
// retired in TASK-3915. withRetiredBlocked rebuilds a registry that still
// asks it, the shape every server ran before that change.
const retiredBlockedKey = "blocked"

func withRetiredBlocked(t *testing.T) *decision.Registry {
	t.Helper()
	old := decision.AttentionSet()
	old.Questions[retiredBlockedKey] = decision.Noul(
		"The item's text says the work is currently blocked on something outside the item itself.",
		"an explicit dependency, wait, or blocker is named",
		"no blocker is stated; the work can start",
	)
	reg := decision.NewRegistry()
	if err := reg.Register(old); err != nil {
		t.Fatal(err)
	}
	return reg
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
	edge := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Pick vendor HUMAN-EDGE", "fields": `{"status":"open"}`})
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
	if n := len(filterAttention(attentionFor(resp, edge.Ref), "needs_human")); n != 1 {
		t.Errorf("an item exactly AT the threshold was not surfaced (entries = %d)", n)
	}
	if resp.Degraded {
		t.Errorf("a healthy provider marked the dashboard degraded: %v", resp.DegradedSections)
	}
}

// TASK-3915: "blocked" on the dashboard is the dependency graph's alone. The
// runner here still ASKS the retired text-derived question and its answer
// for both items is 0.85, so WorkspaceFlags hands the dashboard a blocked
// flag (the control below proves it). What keeps it off the dashboard is the
// handler, not the stale-question filter: production stores no such
// question, and that filter would hide the row by itself, which is why this
// test cannot use the production registry.
func TestDashboard_NoTextDerivedBlocked(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	reg := withRetiredBlocked(t)
	srv.SetDecisionRunner(decision.NewRunner(srv.store, &attentionStub{}, reg))
	slug := createWSWithCollections(t, srv)
	blocker := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Upstream", "fields": `{"status":"open"}`})
	both := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Graph and TEXT-BLOCKED", "fields": `{"status":"open"}`})
	textOnly := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Only TEXT-BLOCKED", "fields": `{"status":"open"}`})
	createBlocksLink(t, srv, slug, blocker.Slug, both.ID)
	runDecisionTick(t, srv)

	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatal(err)
	}
	flags, _, err := srv.decisionRunner().WorkspaceFlags(ws.ID, decision.AttentionSetName, decision.AttentionThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := flags[textOnly.ID][retiredBlockedKey]; !ok {
		t.Fatalf("control: the runner does not flag the text-only item as blocked, so the dashboard leg proves nothing: %v", flags)
	}

	resp := getDashboard(t, srv, slug)
	b := filterAttention(attentionFor(resp, both.Ref), "blocked")
	if len(b) != 1 || !strings.HasPrefix(b[0].Reason, "Blocked by Upstream") {
		t.Fatalf("graph-blocked item: blocked entries = %+v, want exactly the graph's", b)
	}
	if tb := filterAttention(attentionFor(resp, textOnly.Ref), "blocked"); len(tb) != 0 {
		t.Fatalf("a text-derived blocked entry reached the dashboard: %+v", tb)
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

// A collection edit can make an item terminal without touching the item: the
// item's state (and so its stored answer's currency) is unchanged, and no job
// is owed. The dashboard's own done check is what drops it.
func TestDashboardAttention_ItemMadeTerminalBySchemaEditIsNotSurfaced(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	attachAttentionRunner(t, srv)
	slug := createWSWithCollections(t, srv)
	schema := func(terminal string) string {
		return `{"fields":[{"key":"status","type":"select","options":["open","shelved","done"],"terminal_options":[` + terminal + `]}]}`
	}
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]any{"name": "Asks", "schema": schema(`"done"`)})
	if rr.Code != 201 {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	it := createItem(t, srv, slug, "asks", map[string]interface{}{"title": "Budget sign-off HUMAN-HIGH", "fields": `{"status":"shelved"}`})
	runDecisionTick(t, srv)
	if len(filterAttention(attentionFor(getDashboard(t, srv, slug), it.Ref), "needs_human")) != 1 {
		t.Fatal("precondition: the shelved (non-terminal) item is not surfaced")
	}
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/asks", map[string]any{"schema": schema(`"done","shelved"`)})
	if rr.Code != 200 {
		t.Fatalf("patch collection: %d %s", rr.Code, rr.Body.String())
	}
	if got := attentionFor(getDashboard(t, srv, slug), it.Ref); len(got) != 0 {
		t.Fatalf("an item its collection now calls terminal is still in attention: %+v", got)
	}
	// The item page reads the same rule through /decisions: every attention
	// answer is still stored, and none is current. (Before the edit they
	// were — the dashboard precondition above is that leg.)
	b := getDecisions(t, srv, slug, it.Slug)
	// One row per attention question (four since TASK-3921), staged keys included.
	if want := len(decision.AttentionSet().Questions); len(b.Decisions) != want {
		t.Fatalf("decisions = %d rows, want the %d stored answers", len(b.Decisions), want)
	}
	for _, d := range b.Decisions {
		if d.Current {
			t.Errorf("%s is current for an item its set no longer applies to", d.QuestionKey)
		}
	}
}
