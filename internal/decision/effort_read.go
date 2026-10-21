package decision

import (
	"context"
	"encoding/json"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Readers for the staged attention answers (TASK-3959, IDEA-3906 B4b). The
// stored shape stays private to this package; these return what a reader
// needs and nothing of the wire vocabulary.

// EffortMediumMinProb is the least probability the effort_medium choice may
// have for its answer to count: under it the answer reads as "unclear".
const EffortMediumMinProb = 0.5

// EffortLevel reads a stored effort answer: the level it picked, 0-3, by
// the argmax of its probabilities (Answer.TopLevel), never by truncating the
// mean score. ok is false for a row of another kind, an unreadable answer or
// one without probabilities.
func EffortLevel(d models.ItemDecision) (level int, ok bool) {
	if d.Kind != string(KindScore) {
		return 0, false
	}
	var a storedAnswer
	if err := json.Unmarshal(d.Answer, &a); err != nil || len(a.Probabilities) == 0 {
		return 0, false
	}
	idx, _, ok := Answer{Kind: KindScore, Probabilities: a.Probabilities, Legend: a.Legend}.TopLevel()
	if !ok || idx < 0 || idx > 3 {
		return 0, false
	}
	return idx, true
}

// ChoiceValue reads a stored Choice answer: the option it picked and the
// probability of that option (0 when the answer carries no distribution).
func ChoiceValue(d models.ItemDecision) (choice string, prob float64, ok bool) {
	if d.Kind != string(KindChoice) {
		return "", 0, false
	}
	var a storedAnswer
	if err := json.Unmarshal(d.Answer, &a); err != nil || a.Choice == nil {
		return "", 0, false
	}
	return *a.Choice, a.Probabilities[*a.Choice], true
}

// CurrentQuestionHashes returns, per question key, the fingerprint of the
// question as the set asks it in the workspace now. A matching question_hash
// says only that the QUESTION is unchanged. Unlike the runner's own currency
// rule, it does not check the state hash, so an answer can be one item edit
// behind. The needs scorers accept that (TASK-3959 ruling 99c2bcfe, item 3)
// and say "being refreshed" while a job is owed. A nil runner returns nothing.
func (r *Runner) CurrentQuestionHashes(ctx context.Context, workspaceID, setName string) (map[string]string, error) {
	if r == nil {
		return nil, nil
	}
	qs, ok := r.registry.Get(setName)
	if !ok {
		return nil, ErrUnknownSet
	}
	questions, err := r.questionsFor(ctx, qs, workspaceID)
	if err != nil {
		return nil, err
	}
	model := r.provider.Model()
	out := make(map[string]string, len(questions))
	for k, q := range questions {
		out[k] = QuestionFingerprint(model, q)
	}
	return out, nil
}
