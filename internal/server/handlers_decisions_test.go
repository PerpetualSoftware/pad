package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/decision"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// stubProvider answers every question with a fixed Noul and counts calls.
type stubProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *stubProvider) Name() string  { return "stub" }
func (p *stubProvider) Model() string { return "stub-1" }
func (p *stubProvider) Ask(_ context.Context, _ any, qs map[string]decision.Question) (map[string]decision.Answer, decision.Usage, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	out := map[string]decision.Answer{}
	for k := range qs {
		out[k] = decision.Answer{Kind: decision.KindNoul, Noul: 0.75}
	}
	return out, decision.Usage{}, nil
}

func (p *stubProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func attachStubRunner(t *testing.T, srv *Server) *stubProvider {
	t.Helper()
	reg := decision.NewRegistry()
	if err := reg.Register(decision.QuestionSet{
		Name:      "triage",
		Questions: map[string]decision.Question{"urgent": decision.Noul("Is this urgent?", "", "")},
	}); err != nil {
		t.Fatal(err)
	}
	p := &stubProvider{}
	srv.SetDecisionRunner(decision.NewRunner(srv.store, p, reg))
	return p
}

type decisionsBody struct {
	Ref       string                `json:"ref"`
	Decisions []models.ItemDecision `json:"decisions"`
}

func getDecisions(t *testing.T, srv *Server, wsSlug, itemSlug string) decisionsBody {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+wsSlug+"/items/"+itemSlug+"/decisions", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET decisions: %d %s", rr.Code, rr.Body.String())
	}
	var b decisionsBody
	parseJSON(t, rr, &b)
	if b.Decisions == nil {
		t.Fatalf("decisions is null, want a list: %s", rr.Body.String())
	}
	return b
}

func itemJSONKeys(t *testing.T, srv *Server, wsSlug, itemSlug string) map[string]json.RawMessage {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+wsSlug+"/items/"+itemSlug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET item: %d %s", rr.Code, rr.Body.String())
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// No provider: the endpoint answers an empty list, the item has no
// `decisions` member, and no write left a job row.
func TestDecisions_NoProviderIsEmpty(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Plain", "fields": `{"status":"open"}`})

	if b := getDecisions(t, srv, slug, item.Slug); len(b.Decisions) != 0 {
		t.Fatalf("no provider, got %d decisions", len(b.Decisions))
	}
	if _, has := itemJSONKeys(t, srv, slug, item.Slug)["decisions"]; has {
		t.Fatal("item GET carries a decisions member with no provider configured")
	}
	if j, err := srv.store.GetDecisionJob(item.ID, "triage"); err != nil || j != nil {
		t.Fatalf("no provider, but a job row exists: %+v %v", j, err)
	}
}

// Through the HTTP doors end to end, with the tick as the evaluator — the
// binding under test is the wiring (write door -> queue -> tick -> read),
// not the runner called directly.
func TestDecisions_TickEvaluatesAndReadReportsCurrency(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	p := attachStubRunner(t, srv)
	tick := make(chan time.Time)
	srv.SetDecisionTickChannel(tick)
	srv.StartDecisionTick()
	t.Cleanup(srv.stopDecisionTick)

	slug := createWSWithCollections(t, srv)
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Wired", "fields": `{"status":"open"}`})
	if j, _ := srv.store.GetDecisionJob(item.ID, "triage"); j == nil {
		t.Fatal("create through the API did not enqueue a job")
	}

	runTickUntil := func(cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal("tick did not complete the evaluation within 5s")
			}
			select {
			case tick <- time.Now():
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	runTickUntil(func() bool {
		j, _ := srv.store.GetDecisionJob(item.ID, "triage")
		return j == nil && p.count() == 1
	})

	b := getDecisions(t, srv, slug, item.Slug)
	if len(b.Decisions) != 1 || !b.Decisions[0].Current || b.Decisions[0].QuestionKey != "urgent" {
		t.Fatalf("after evaluation: %+v", b.Decisions)
	}
	if _, has := itemJSONKeys(t, srv, slug, item.Slug)["decisions"]; !has {
		t.Fatal("item GET omits decisions although one is stored")
	}

	// A comment through the API: the stored answer is no longer current, and
	// the next tick re-evaluates.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/comments", map[string]string{"body": "new facts"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment: %d %s", rr.Code, rr.Body.String())
	}
	if b := getDecisions(t, srv, slug, item.Slug); b.Decisions[0].Current {
		t.Fatal("answer computed before the comment still reads as current")
	}
	runTickUntil(func() bool { return p.count() == 2 })
	runTickUntil(func() bool {
		b := getDecisions(t, srv, slug, item.Slug)
		return len(b.Decisions) == 1 && b.Decisions[0].Current
	})
}

func TestDecisions_UnknownItemIs404(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/NOPE-999/decisions", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown item: %d %s", rr.Code, rr.Body.String())
	}
}
