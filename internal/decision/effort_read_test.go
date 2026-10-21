package decision

import (
	"context"
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3959 1a r2: the effort readers, tested where they live.

func effortRow(kind, answer string) models.ItemDecision {
	return models.ItemDecision{QuestionKey: AttentionEffort, Kind: kind, Answer: []byte(answer)}
}

func TestEffortLevel(t *testing.T) {
	cases := []struct {
		name   string
		row    models.ItemDecision
		level  int
		wantOK bool
	}{
		// The mean of this distribution is 1.7, so truncating it would give 1.
		// The argmax is 2.
		{"argmax, not the mean", effortRow("score", `{"type":"score","probabilities":{"0":0.1,"1":0.2,"2":0.6,"3":0.1}}`), 2, true},
		{"a tie goes to the lower level", effortRow("score", `{"type":"score","probabilities":{"0":0.4,"1":0.4,"2":0.1,"3":0.1}}`), 0, true},
		{"top level", effortRow("score", `{"type":"score","probabilities":{"0":0.05,"1":0.05,"2":0.1,"3":0.8}}`), 3, true},
		// A key outside 0-3 that wins makes the answer unreadable rather than
		// guessing a level: it fails safe, with no level and no hint.
		{"a stray winning key is unreadable", effortRow("score", `{"type":"score","probabilities":{"1":0.1,"x":0.9}}`), 0, false},
		{"a level above 3 is unreadable", effortRow("score", `{"type":"score","probabilities":{"1":0.1,"4":0.9}}`), 0, false},
		{"another kind", effortRow("choice", `{"type":"choice","choice":"desk","probabilities":{"desk":1}}`), 0, false},
		{"no probabilities", effortRow("score", `{"type":"score","score":2}`), 0, false},
		{"malformed answer", effortRow("score", `{not json`), 0, false},
	}
	for _, c := range cases {
		level, ok := EffortLevel(c.row)
		if ok != c.wantOK || (ok && level != c.level) {
			t.Errorf("%s: EffortLevel = (%d, %v), want (%d, %v)", c.name, level, ok, c.level, c.wantOK)
		}
	}
}

func TestChoiceValue(t *testing.T) {
	choice, prob, ok := ChoiceValue(effortRow("choice", `{"type":"choice","choice":"device","probabilities":{"desk":0.3,"device":0.7}}`))
	if !ok || choice != "device" || prob != 0.7 {
		t.Fatalf("ChoiceValue = (%q, %v, %v), want (device, 0.7, true)", choice, prob, ok)
	}
	// Under EffortMediumMinProb the reader treats it as unclear; the value
	// itself is still reported.
	if _, prob, ok := ChoiceValue(effortRow("choice", `{"type":"choice","choice":"desk","probabilities":{"desk":0.4,"device":0.35,"other_person":0.25}}`)); !ok || prob >= EffortMediumMinProb {
		t.Fatalf("a weak choice reads prob %v ok %v", prob, ok)
	}
	if _, prob, ok := ChoiceValue(effortRow("choice", `{"type":"choice","choice":"desk"}`)); !ok || prob != 0 {
		t.Fatalf("a choice without a distribution: prob %v ok %v, want 0 true", prob, ok)
	}
	for name, row := range map[string]models.ItemDecision{
		"another kind": effortRow("score", `{"type":"score","probabilities":{"1":1}}`),
		"no choice":    effortRow("choice", `{"type":"choice","probabilities":{"desk":1}}`),
		"malformed":    effortRow("choice", `{not json`),
	} {
		if _, _, ok := ChoiceValue(row); ok {
			t.Errorf("%s: ChoiceValue ok", name)
		}
	}
}

func TestCurrentQuestionHashes_NilRunner(t *testing.T) {
	var r *Runner
	got, err := r.CurrentQuestionHashes(context.Background(), "ws", AttentionSetName)
	if err != nil || got != nil {
		t.Fatalf("a nil runner = (%v, %v), want (nil, nil)", got, err)
	}
}

// TASK-3959 1a r3: the hashes the reader reports are the ones the runner
// stamps on a stored answer, read back from the store rather than recomputed.
func TestCurrentQuestionHashes_MatchWhatTheRunnerStamps(t *testing.T) {
	fx := newAttentionFixture(t)
	it := fx.item(t, fx.tasks.ID, "open")
	fx.tick(t)
	stored, err := fx.s.LatestItemDecisions(it.ID)
	if err != nil || len(stored) == 0 {
		t.Fatalf("precondition: no stored answers: %v %v", stored, err)
	}
	got, err := fx.r.CurrentQuestionHashes(context.Background(), fx.ws.ID, AttentionSetName)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(stored) {
		t.Fatalf("hashes for %d keys, runner stored %d", len(got), len(stored))
	}
	for _, d := range stored {
		if d.QuestionHash == "" || got[d.QuestionKey] != d.QuestionHash {
			t.Errorf("key %q: reader %q, runner stamped %q", d.QuestionKey, got[d.QuestionKey], d.QuestionHash)
		}
	}
	if _, err := fx.r.CurrentQuestionHashes(context.Background(), fx.ws.ID, "no-such-set"); !errors.Is(err, ErrUnknownSet) {
		t.Fatalf("unknown set error = %v, want ErrUnknownSet", err)
	}
}
