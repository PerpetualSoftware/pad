// Package decision provides a pluggable typed-decision provider: a small set
// of classification primitives (Choice, Score, Noul) answered in one round
// trip, with calibrated probabilities where the primitive has them.
//
// The package is a PROVIDER INTERFACE, not an integration with any one
// vendor. The only backend today is typesafe.ai's Jev model (see
// typesafe.go), selected by configuration.
//
// # Unconfigured means nil
//
// When no provider is configured, [New] returns a nil [Provider] and no
// error. Callers treat nil as "no decisions available" and behave exactly as
// they did before this package existed — there are deliberately no no-op
// implementations and no degraded paths, so that the absence of a provider is
// a visible branch at every call site rather than a silent difference in
// behaviour. See PLAN-3114 and TASK-3116.
package decision

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Kind names one of the three decision primitives.
//
// The wire values are the provider's, and the Go constants deliberately
// match them so a decoded answer needs no translation table.
type Kind string

const (
	// KindChoice picks exactly one of a set of named options, and returns a
	// probability per option plus a confidence.
	KindChoice Kind = "choice"

	// KindScore places the state on an ORDERED scale of levels, and returns
	// a probability per level plus a confidence. See [Answer.Score] for the
	// one genuinely surprising thing about this primitive.
	KindScore Kind = "score"

	// KindNoul returns the probability that a statement is true. It carries
	// NO confidence and NO probabilities — see [Answer.Confidence].
	KindNoul Kind = "noul"
)

// Question is one thing to ask about a state. Construct one with [Choice],
// [Score] or [Noul] rather than by literal: the three primitives take
// mutually incompatible criteria (a map, an ordered slice, and a pair of
// descriptions), so a hand-built literal can express a Choice carrying
// Score's levels, which no provider can answer.
type Question struct {
	Kind Kind

	// Instructions is the question in plain language.
	Instructions string

	// Options is Choice's criteria: option name -> what that option means.
	// Empty for the other kinds.
	Options map[string]string

	// Levels is Score's criteria: an ORDERED slice, lowest first. The order
	// is the scale, so it is part of the value and not a presentation
	// detail. Empty for the other kinds.
	Levels []string

	// TrueDesc and FalseDesc are Noul's optional criteria, describing what
	// makes the statement true and false respectively. Both empty is valid:
	// the provider then works from Instructions alone.
	TrueDesc, FalseDesc string
}

// Choice builds a Choice question. options maps each option name to a
// description of what that option means; at least two are required.
func Choice(instructions string, options map[string]string) Question {
	return Question{Kind: KindChoice, Instructions: instructions, Options: options}
}

// Score builds a Score question over an ORDERED scale, lowest level first.
// At least two levels are required.
func Score(instructions string, levels []string) Question {
	return Question{Kind: KindScore, Instructions: instructions, Levels: levels}
}

// Noul builds a Noul question — the probability that a statement is true.
// trueDesc and falseDesc are optional; pass "" for both to work from the
// instructions alone.
func Noul(instructions, trueDesc, falseDesc string) Question {
	return Question{Kind: KindNoul, Instructions: instructions, TrueDesc: trueDesc, FalseDesc: falseDesc}
}

// maxChoiceOptions is the provider's documented ceiling on Choice options.
const maxChoiceOptions = 255

// minLevels is the provider's documented floor on Score levels.
const minLevels = 2

// Validate reports whether the question is answerable as constructed.
//
// It rejects criteria belonging to a DIFFERENT kind as well as missing ones:
// a Choice carrying Levels is a caller who meant to call [Score], and
// silently dropping the levels would send a question the caller did not
// write.
func (q Question) Validate() error {
	if strings.TrimSpace(q.Instructions) == "" {
		return errors.New("decision: question instructions are empty")
	}
	switch q.Kind {
	case KindChoice:
		if len(q.Options) < 2 {
			return fmt.Errorf("decision: choice question needs at least 2 options, got %d", len(q.Options))
		}
		if len(q.Options) > maxChoiceOptions {
			return fmt.Errorf("decision: choice question has %d options, limit is %d", len(q.Options), maxChoiceOptions)
		}
		for name, desc := range q.Options {
			if strings.TrimSpace(name) == "" {
				return errors.New("decision: choice question has an empty option name")
			}
			if strings.TrimSpace(desc) == "" {
				return fmt.Errorf("decision: choice option %q has an empty description", name)
			}
		}
		if len(q.Levels) > 0 || q.TrueDesc != "" || q.FalseDesc != "" {
			return errors.New("decision: choice question carries another kind's criteria (levels or true/false)")
		}
	case KindScore:
		if len(q.Levels) < minLevels {
			return fmt.Errorf("decision: score question needs at least %d levels, got %d", minLevels, len(q.Levels))
		}
		for i, l := range q.Levels {
			if strings.TrimSpace(l) == "" {
				return fmt.Errorf("decision: score level %d is empty", i)
			}
		}
		if len(q.Options) > 0 || q.TrueDesc != "" || q.FalseDesc != "" {
			return errors.New("decision: score question carries another kind's criteria (options or true/false)")
		}
	case KindNoul:
		if len(q.Options) > 0 || len(q.Levels) > 0 {
			return errors.New("decision: noul question carries another kind's criteria (options or levels)")
		}
		// An asymmetric pair is almost certainly a mistake — the provider
		// reads criteria literally, so describing only one side biases the
		// answer toward the side that was described.
		if (q.TrueDesc == "") != (q.FalseDesc == "") {
			return errors.New("decision: noul question describes only one of true/false; give both or neither")
		}
	default:
		return fmt.Errorf("decision: unknown question kind %q", q.Kind)
	}
	return nil
}

// Answer is one question's answer. Which members are populated is decided by
// [Answer.Kind]; reading a member belonging to another kind is a caller bug
// that the zero value will not announce, which is why [Answer.Confidence] is
// a pointer.
type Answer struct {
	// Kind echoes the primitive that produced this answer.
	Kind Kind

	// Choice is the selected option name, for [KindChoice] only.
	Choice string

	// Score is the position on the scale, for [KindScore] only.
	//
	// IT IS NOT A LEVEL INDEX. The provider returns the
	// probability-weighted MEAN over level indices, so it is a float that
	// usually falls BETWEEN levels: a measured answer with
	// probabilities {"0":0.86,"1":0.14} returned score 0.14, which is
	// 0.86*0 + 0.14*1 and not the index of any level. To recover "which
	// level did it pick", take the argmax of Probabilities
	// ([Answer.TopLevel]); truncating or int-casting Score gives the wrong
	// level for any distribution whose mass does not sit on index 0.
	// (Measured against jev-1.13.0; the arithmetic is on TASK-3116's trail.)
	Score float64

	// Noul is the probability that the statement is true, for [KindNoul]
	// only. Note that this IS the answer — a Noul carries no separate
	// confidence, so the distance from 0.5 is all the certainty there is.
	Noul float64

	// Legend maps a stringified level index to that level's name, for
	// [KindScore] only. It echoes the Levels that were sent, so a stored
	// answer stays interpretable after the question's levels are reworded.
	Legend map[string]string

	// Probabilities is the distribution over outcomes. THE KEY VOCABULARY
	// DEPENDS ON KIND: option NAMES for [KindChoice], stringified level
	// INDICES ("0", "1", ...) for [KindScore]. Nil for [KindNoul], which
	// has no distribution. A consumer that assumes one vocabulary and is
	// handed the other reads a missing key as 0.0, so switch on Kind.
	Probabilities map[string]float64

	// Confidence is how sure the provider is, for [KindChoice] and
	// [KindScore].
	//
	// It is a POINTER because [KindNoul] answers carry no confidence at
	// all, and 0.0 is not a neutral stand-in for "absent" — it is the most
	// extreme value the scale has. A caller following the provider's own
	// recommended banding (act above 0.9, route to a human below 0.5) would
	// read every Noul answer as minimum confidence and route all of them to
	// a human, which states the opposite of the truth that the primitive
	// has no opinion on confidence. A nil pointer forces that check.
	Confidence *float64
}

// ConfidenceOr returns the answer's confidence, or def when the primitive
// carries none. Use it where a missing confidence has a defensible default;
// read [Answer.Confidence] directly where it does not.
func (a Answer) ConfidenceOr(def float64) float64 {
	if a.Confidence == nil {
		return def
	}
	return *a.Confidence
}

// TopLevel returns the highest-probability level of a Score answer: its index
// and its name from the legend. ok is false for any other kind, or when the
// answer carries no probabilities.
//
// This is the correct way to ask "which level did it pick" — see
// [Answer.Score] for why that field is not it. Ties resolve to the LOWEST
// index, so the result is deterministic rather than map-order dependent.
func (a Answer) TopLevel() (index int, name string, ok bool) {
	if a.Kind != KindScore || len(a.Probabilities) == 0 {
		return 0, "", false
	}
	keys := make([]string, 0, len(a.Probabilities))
	for k := range a.Probabilities {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return levelIndex(keys[i]) < levelIndex(keys[j]) })

	best := keys[0]
	for _, k := range keys[1:] {
		if a.Probabilities[k] > a.Probabilities[best] {
			best = k
		}
	}
	return levelIndex(best), a.Legend[best], true
}

// Usage reports what one [Provider.Ask] call consumed and whether the
// provider had to shorten the state to fit.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`

	// StateTruncated reports that the state was shortened at the provider
	// boundary to fit the backend's token budget, so the answers describe
	// LESS than the caller supplied.
	//
	// It lives on Usage rather than in its own return value because
	// [Provider.Ask]'s signature is fixed by TASK-3116's contract, and
	// Usage is already the "what did this call actually do" struct. A
	// caller acting on a truncated answer should say so wherever it
	// records the result.
	StateTruncated bool `json:"state_truncated,omitempty"`
}

// Provider answers typed questions about a state.
//
// A nil Provider is the configured-off case and is not an error; see the
// package doc.
type Provider interface {
	// Name is the backend's configuration name, e.g. "typesafe".
	Name() string

	// Model is the pinned model identifier this provider will use.
	Model() string

	// Ask answers every question about the same state in ONE round trip.
	//
	// state is marshalled as JSON, so a string, a map, or any
	// JSON-marshallable struct is accepted; prefer the narrowest state that
	// can answer the questions, because irrelevant state degrades accuracy.
	//
	// The returned map has one entry per question key. An error means NO
	// answers: this is deliberately all-or-nothing, since a partial map
	// would make "the provider had no opinion" indistinguishable from "the
	// call half failed".
	Ask(ctx context.Context, state any, questions map[string]Question) (map[string]Answer, Usage, error)
}
