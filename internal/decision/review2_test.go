package decision

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Review round 2 (TASK-3116). Round 1 added the KIND check; round 2 found the
// rest of that population: an answer can contradict its question in every
// (kind, member) cell, not only in its kind. The rows below are that table —
// one refusal per rule checkAnswer enforces, and an ACCEPTANCE row at each
// boundary, so a check made too strict fails here as surely as one removed.

var (
	choiceQ = Choice("How severe is this bug?", map[string]string{
		"low": "cosmetic", "medium": "a real defect", "high": "breaks a core flow", "critical": "data loss",
	})
	scoreQ = Score("How severe is this bug?", []string{
		"cosmetic only", "annoying but harmless", "a real defect in normal use", "breaks a core flow", "data loss or security exposure",
	})
	noulQ = Noul("a statement", "", "")
)

func TestAnswerContradictingTheQuestionIsRefused(t *testing.T) {
	const legend5 = `"legend":{"0":"a","1":"b","2":"c","3":"d","4":"e"}`
	tests := []struct {
		name    string
		q       Question
		answer  string // the answer object for key "q"
		wantErr string // "" means the answer must be ACCEPTED
	}{
		// choice
		{"choice accepted", choiceQ, `{"type":"choice","choice":"high","confidence":0.63,"probabilities":{"high":0.72,"low":0.28}}`, ""},
		{"choice confidence 0 and 1 accepted", choiceQ, `{"type":"choice","choice":"low","confidence":0,"probabilities":{"low":1,"high":0}}`, ""},
		{"choice missing", choiceQ, `{"type":"choice","confidence":0.6,"probabilities":{"high":1}}`, "carries no choice"},
		{"choice not among options", choiceQ, `{"type":"choice","choice":"urgent","confidence":0.6,"probabilities":{"high":1}}`, "not one of the options sent"},
		{"choice missing confidence", choiceQ, `{"type":"choice","choice":"high","probabilities":{"high":1}}`, "carries no confidence"},
		{"choice confidence above 1", choiceQ, `{"type":"choice","choice":"high","confidence":1.5,"probabilities":{"high":1}}`, "confidence 1.5 is outside"},
		{"choice confidence below 0", choiceQ, `{"type":"choice","choice":"high","confidence":-0.1,"probabilities":{"high":1}}`, "confidence -0.1 is outside"},
		{"choice missing probabilities", choiceQ, `{"type":"choice","choice":"high","confidence":0.6}`, "carries no probabilities"},
		{"choice probability key not an option", choiceQ, `{"type":"choice","choice":"high","confidence":0.6,"probabilities":{"high":0.5,"urgent":0.5}}`, `key "urgent"`},
		{"choice probability above 1", choiceQ, `{"type":"choice","choice":"high","confidence":0.6,"probabilities":{"high":1.2}}`, "probability 1.2"},

		// score
		{"score accepted", scoreQ, `{"type":"score","score":0.14,"confidence":0.88,` + legend5 + `,"probabilities":{"0":0.86,"1":0.14}}`, ""},
		{"score at the top level accepted", scoreQ, `{"type":"score","score":4,"confidence":1,` + legend5 + `,"probabilities":{"4":1}}`, ""},
		{"score at zero accepted", scoreQ, `{"type":"score","score":0,"confidence":1,"probabilities":{"0":1}}`, ""},
		{"score missing", scoreQ, `{"type":"score","confidence":0.8,"probabilities":{"0":1}}`, "carries no score"},
		{"score above the top level", scoreQ, `{"type":"score","score":4.5,"confidence":0.8,"probabilities":{"4":1}}`, "outside [0, 4]"},
		{"score below zero", scoreQ, `{"type":"score","score":-0.1,"confidence":0.8,"probabilities":{"0":1}}`, "outside [0, 4]"},
		{"score missing confidence", scoreQ, `{"type":"score","score":1,"probabilities":{"1":1}}`, "carries no confidence"},
		{"score missing probabilities", scoreQ, `{"type":"score","score":1,"confidence":0.8}`, "carries no probabilities"},
		{"score probability key past the levels", scoreQ, `{"type":"score","score":1,"confidence":0.8,"probabilities":{"5":1}}`, `key "5"`},
		{"score probability key non-canonical", scoreQ, `{"type":"score","score":1,"confidence":0.8,"probabilities":{"01":1}}`, `key "01"`},
		{"score probability key not a number", scoreQ, `{"type":"score","score":1,"confidence":0.8,"probabilities":{"junk":1}}`, `key "junk"`},
		{"score probability key negative", scoreQ, `{"type":"score","score":1,"confidence":0.8,"probabilities":{"-1":1}}`, `key "-1"`},
		{"score legend key past the levels", scoreQ, `{"type":"score","score":1,"confidence":0.8,"legend":{"9":"x"},"probabilities":{"1":1}}`, `legend key "9"`},
		{"score probability below 0", scoreQ, `{"type":"score","score":1,"confidence":0.8,"probabilities":{"1":-0.2}}`, "probability -0.2"},

		// noul
		{"noul accepted", noulQ, `{"type":"noul","noul":0.92}`, ""},
		{"noul at 1 accepted", noulQ, `{"type":"noul","noul":1}`, ""},
		{"noul zero accepted", noulQ, `{"type":"noul","noul":0}`, ""},
		{"noul missing", noulQ, `{"type":"noul"}`, "carries no noul value"},
		{"noul above 1", noulQ, `{"type":"noul","noul":1.01}`, "noul 1.01 is outside"},
		{"noul below 0", noulQ, `{"type":"noul","noul":-0.01}`, "noul -0.01 is outside"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _ := newFake(t, ok(`{"model":"jev-1.13.0","answers":{"q":`+tt.answer+`},"usage":{"input_tokens":1,"output_tokens":1}}`))
			answers, _, err := p.Ask(context.Background(), "s", map[string]Question{"q": tt.q})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("a valid answer was refused: %v", err)
				}
				if answers["q"].Kind != tt.q.Kind {
					t.Errorf("accepted answer has Kind %q, want %q", answers["q"].Kind, tt.q.Kind)
				}
				return
			}
			if err == nil {
				t.Fatalf("Ask accepted an answer that contradicts its question; want an error containing %q", tt.wantErr)
			}
			if answers != nil {
				t.Errorf("Ask returned answers alongside its error: %v — the call is all-or-nothing", answers)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
			if !strings.Contains(err.Error(), `question "q"`) {
				t.Errorf("error does not name the question: %q", err.Error())
			}
		})
	}
}

func TestUnsolicitedAnswerIsDropped(t *testing.T) {
	p, _ := newFake(t, ok(`{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.3},"internal_debug":{"type":"noul","noul":0.9}},"usage":{"input_tokens":1,"output_tokens":1}}`))
	answers, _, err := p.Ask(context.Background(), "s", map[string]Question{"q": noulQ})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, ok := answers["internal_debug"]; ok {
		t.Error("an answer to a question nobody asked was returned to the caller")
	}
	if len(answers) != 1 || answers["q"].Noul != 0.3 {
		t.Errorf("answers = %v, want exactly the one asked-for answer", answers)
	}
}

// The envelope reservation used to be a fixed 512 bytes. A model name longer
// than that, plus a state cut exactly to the limit, sent a request over the
// budget. The reservation is now the marshalled envelope itself.
func TestFittedRequestStaysInsideTheBudgetWithALongModelName(t *testing.T) {
	p := newTypesafe("k", strings.Repeat("m", 5000))
	wire := map[string]wireQuestion{"q": toWire(noulQ)}
	budget := int(float64(maxRequestTokens) * charsPerToken)

	state, truncated, err := p.fitState(strings.Repeat("x", budget), wire)
	if err != nil {
		t.Fatalf("fitState: %v", err)
	}
	if !truncated {
		t.Fatal("precondition: a budget-sized state must be truncated, or this test measures nothing")
	}
	full, err := json.Marshal(wireRequest{Model: p.model, State: state, Questions: wire})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(full) > budget {
		t.Errorf("fitted request is %d bytes, over the %d-byte budget by %d", len(full), budget, len(full)-budget)
	}
	// And the cut is tight, not merely safe: the reservation is exact, so the
	// whole request lands ON the budget. A reservation that over-reserved
	// would still pass the check above.
	if len(full) != budget {
		t.Errorf("fitted request is %d bytes, want exactly the %d-byte budget for a one-byte-per-char state", len(full), budget)
	}
}

// Round 2: the partial-read failure sat on BodyReadErr but was invisible to
// errors.Is, so a caller could not ask "did this fail because the read was
// cut off" without knowing the field.
func TestPartialBodyReadIsReachableWithErrorsIs(t *testing.T) {
	srv := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "200")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":{"error_ty`))
	})
	p := newTypesafe("test-key", "jev-1.13.0")
	p.setEndpoint(srv.URL)
	p.sleep = func(context.Context, time.Duration) error { return nil }

	_, _, err := p.Ask(context.Background(), "s", map[string]Question{"q": noulQ})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.BodyReadErr == nil {
		t.Fatalf("precondition: want an *APIError carrying a BodyReadErr, got %v", err)
	}
	if !errors.Is(err, apiErr.BodyReadErr) {
		t.Errorf("errors.Is(err, BodyReadErr) is false; the read failure is not in the chain")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("errors.Is(err, io.ErrUnexpectedEOF) is false for a body cut short (BodyReadErr = %v)", apiErr.BodyReadErr)
	}
	// Control leg: a complete body carries no read error, so nothing
	// spurious enters the chain.
	p2, _ := newFake(t, func(_ *fake, _ wireRequest, _ []byte, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":{"error_type":"api_usage_error","message":"Invalid request."}}`))
	})
	_, _, err = p2.Ask(context.Background(), "s", map[string]Question{"q": noulQ})
	if errors.Is(err, io.ErrUnexpectedEOF) {
		t.Error("a complete error body reported an unexpected EOF")
	}
}

// Review round 3: with 0 or 1 bytes left for the state, fitState returned
// "" — which still marshals to two bytes, so the request went over budget.
// This walks `remaining` across the edge by growing the instructions a byte
// at a time and holds one invariant at every step: either an error, or a
// request that fits.
func TestFitStateHoldsTheBudgetAtTheQuoteEdge(t *testing.T) {
	p := newTypesafe("test-key", "jev-1.13.0")
	budget := int(float64(maxRequestTokens) * charsPerToken)
	seen := map[int]bool{}

	for n := budget - 120; n < budget; n++ {
		wire := map[string]wireQuestion{"q": {Type: "noul", Instructions: strings.Repeat("i", n)}}
		env, err := json.Marshal(wireRequest{Model: p.model, State: "", Questions: wire})
		if err != nil {
			t.Fatal(err)
		}
		remaining := budget - (len(env) - 2)
		if remaining < -2 || remaining > 5 {
			continue
		}
		seen[remaining] = true
		// A structured state is never cut, and `0` marshals to ONE byte, so
		// with remaining >= 1 it fits and must not be refused (round 4).
		if remaining >= 1 {
			got, truncated, err := p.fitState(0, wire)
			if err != nil {
				t.Errorf("remaining=%d: a one-byte structured state was refused: %v", remaining, err)
			} else if truncated || got != 0 {
				t.Errorf("remaining=%d: structured state came back %v (truncated=%v), want it untouched", remaining, got, truncated)
			}
		}
		for _, state := range []string{"", "abc", strings.Repeat("x", 50)} {
			got, _, err := p.fitState(state, wire)
			if err != nil {
				if remaining >= 2 {
					t.Errorf("remaining=%d state=%q: refused although a state fits: %v", remaining, state, err)
				}
				continue
			}
			full, _ := json.Marshal(wireRequest{Model: p.model, State: got, Questions: wire})
			if len(full) > budget {
				t.Errorf("remaining=%d state=%q: request is %d bytes, %d over the budget",
					remaining, state, len(full), len(full)-budget)
			}
		}
	}
	// Precondition: the sweep must actually have crossed the edge, or it
	// asserted nothing about it.
	for _, r := range []int{0, 1, 2} {
		if !seen[r] {
			t.Errorf("sweep never produced remaining=%d; widen it", r)
		}
	}
}
