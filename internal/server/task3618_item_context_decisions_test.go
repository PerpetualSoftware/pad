package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/decision"
)

// keyedProvider answers each question key with its own Noul.
type keyedProvider map[string]float64

func (keyedProvider) Name() string  { return "keyed" }
func (keyedProvider) Model() string { return "keyed-1" }
func (p keyedProvider) Ask(_ context.Context, _ any, qs map[string]decision.Question) (map[string]decision.Answer, decision.Usage, error) {
	out := map[string]decision.Answer{}
	for k := range qs {
		out[k] = decision.Answer{Kind: decision.KindNoul, Noul: p[k]}
	}
	return out, decision.Usage{}, nil
}

// TASK-3618: the brief carries only the decisions their set would surface,
// counts the rest, and raises stale_decision only for a surfaced answer whose
// item has moved on.
func TestTASK3618_ItemContextCarriesOnlySurfacedDecisions(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	th := decision.AttentionThreshold
	set := func(wording string) decision.QuestionSet {
		return decision.QuestionSet{
			Name: "triage",
			Questions: map[string]decision.Question{
				"above": decision.Noul("Is this urgent?"+wording, "", ""),
				"at":    decision.Noul("Is this risky?", "", ""),
				"below": decision.Noul("Is this large?", "", ""),
			},
			Threshold: th,
		}
	}
	runner := func(wording string) *decision.Runner {
		reg := decision.NewRegistry()
		if err := reg.Register(set(wording)); err != nil {
			t.Fatal(err)
		}
		return decision.NewRunner(srv.store, keyedProvider{"above": 0.95, "at": th, "below": th - 0.01}, reg)
	}
	r := runner("")
	srv.SetDecisionRunner(r)

	slug := createWSWithCollections(t, srv)
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Brief", "fields": `{"status":"open"}`})
	if _, err := r.RunOnce(context.Background(), "t", 10, time.Minute); err != nil {
		t.Fatal(err)
	}

	type brief struct {
		Decisions []struct {
			QuestionKey string `json:"question_key"`
			Current     bool   `json:"current"`
		} `json:"decisions"`
		Below     int                   `json:"decisions_below_threshold"`
		StaleQ    int                   `json:"decisions_stale_question"`
		Conflicts []itemContextConflict `json:"conflicts"`
	}
	get := func() brief {
		t.Helper()
		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/context?budget=400000", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("context: %d %s", rr.Code, rr.Body.String())
		}
		var b brief
		if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	staleConflicts := func(b brief) int {
		n := 0
		for _, c := range b.Conflicts {
			if c.Kind == "stale_decision" {
				n++
			}
		}
		return n
	}
	keys := func(b brief) map[string]bool {
		m := map[string]bool{}
		for _, d := range b.Decisions {
			m[d.QuestionKey] = d.Current
		}
		return m
	}

	b := get()
	if k := keys(b); len(k) != 2 || !k["above"] || !k["at"] {
		t.Fatalf("decisions = %v, want above and at, both current", k)
	}
	if b.Below != 1 || b.StaleQ != 0 || staleConflicts(b) != 0 {
		t.Fatalf("below=%d staleQuestion=%d stale_decision=%d, want 1, 0, 0", b.Below, b.StaleQ, staleConflicts(b))
	}

	// The item moves on: the two surfaced answers stay, not current, each with
	// a stale_decision conflict; the below-threshold one raises none.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/comments", map[string]string{"body": "new facts"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment: %d %s", rr.Code, rr.Body.String())
	}
	b = get()
	if k := keys(b); len(k) != 2 || k["above"] || k["at"] {
		t.Fatalf("after the item moved: decisions = %v, want above and at, both not current", k)
	}
	if b.Below != 1 || staleConflicts(b) != 2 {
		t.Fatalf("after the item moved: below=%d stale_decision=%d, want 1 and 2", b.Below, staleConflicts(b))
	}

	// The "above" question is reworded: its answer is left out and counted
	// as a stale question, not below threshold, and raises no conflict.
	srv.SetDecisionRunner(runner(" (reworded)"))
	b = get()
	if k := keys(b); len(k) != 1 || !containsKey(k, "at") {
		t.Fatalf("after the rewording: decisions = %v, want only at", k)
	}
	if b.Below != 1 || b.StaleQ != 1 || staleConflicts(b) != 1 {
		t.Fatalf("after the rewording: below=%d staleQuestion=%d stale_decision=%d, want 1, 1, 1", b.Below, b.StaleQ, staleConflicts(b))
	}
}

func containsKey(m map[string]bool, k string) bool { _, ok := m[k]; return ok }
