package decision

import (
	"encoding/json"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The `attention` question set (PLAN-3114 unit 3, TASK-3118): four questions over
// an open item and its recent trail. No surface reads the set (TASK-4643): the
// dashboard, the item page chip, catch-up and the item brief all stopped
// reading needs_human_decision, and Needs you reads only explicit signals. All
// four keys are in Unsurfaced, so no flag, chip or brief entry is made from
// them. Their answers are still asked and stored in item_decisions, for
// re-measurement, and the item's decisions list returns them.
//
// The instruction and criteria of needs_human_decision are carried VERBATIM
// from the eval in scripts/decision-eval (eval2.py, section D), which measured it at AUC 0.939
// over 70 human-tasks vs 70 tasks. Rewording it changes its question_hash and
// re-asks every item, and the eval's figure stops describing the question
// being asked — so change it only with a re-run.
//
// The set used to carry two more Nouls. `blocked` read "the text says it is
// blocked", which the dependency graph already answers (the dashboard's
// graph-blocked section); `waiting_on_external` flagged 0 of 676 items on
// 10-17 and only a chip read it. Both were retired in TASK-3915 / TASK-3924
// (IDEA-3906 half B; the measurements are on that idea's trail). Their stored
// answers stay in item_decisions and read as stale questions, since no
// registered question carries their keys.
//
// effort, effort_medium and high_stakes were STAGED here (TASK-3921, IDEA-3906
// half B4a). They ride the same call as needs_human_decision, so the cost is
// output tokens, not a second request. high_stakes is read by nothing yet (its
// labelled eval is TASK-4011), and the needs-you simplicity score no longer
// reads effort or effort_medium (TASK-4643): it is a text heuristic.
//
// The eval's state was {title, body}; the runner sends the richer
// BuildItemState (collection, fields, and the recent trail as well), so the
// eval figure is evidence about the question, not a measurement of this
// state shape. See TASK-3118's trail.
const (
	AttentionSetName = "attention"

	AttentionNeedsHuman = "needs_human_decision"

	// Staged keys; see the header.
	AttentionEffort       = "effort"
	AttentionEffortMedium = "effort_medium"
	AttentionHighStakes   = "high_stakes"
)

// AttentionSet returns the `attention` question set.
func AttentionSet() QuestionSet {
	return QuestionSet{
		Name: AttentionSetName,
		Questions: map[string]Question{
			AttentionNeedsHuman: Noul(
				"This work item is waiting on a judgment, approval, credential, or decision that only a human owner can supply, rather than on engineering work an AI agent could do unattended.",
				"the next step requires a human's decision, approval, sign-off, credentials, money, an external relationship, or hardware the agent cannot reach",
				"the next step is implementable by an engineer or AI agent from the description alone",
			),
			AttentionEffort: Score(
				"How much effort does it take the human owner to supply what this work item is waiting on, or to do its next step?",
				[]string{
					"a yes/no or one-line answer",
					"a short reply, or minutes at a computer",
					"up to an hour",
					"research, or hours of work",
				},
			),
			AttentionEffortMedium: Choice(
				"Where does the human owner have to be to supply what this work item is waiting on?",
				map[string]string{
					"desk":         "at a computer: writing, reading, a review or a setting",
					"device":       "at a particular device, account or piece of hardware, such as a phone, a test device or a console",
					"other_person": "it needs another person's answer or action",
					"unclear":      "the text does not say",
				},
			),
			AttentionHighStakes: Noul(
				"Getting this work item wrong would be costly: it touches money, security, credentials, legal matters or a release date.",
				"the work involves payments, secrets or access, a legal or compliance obligation, or a date someone is counting on",
				"an ordinary change that is easy to correct afterwards",
			),
		},
		// Every key: no surface reads the set (TASK-4643). With nothing
		// surfaced the set carries no Threshold.
		Unsurfaced: map[string]bool{
			AttentionNeedsHuman:   true,
			AttentionEffort:       true,
			AttentionEffortMedium: true,
			AttentionHighStakes:   true,
		},
		Eligible: attentionEligible,
	}
}

// attentionEligible admits open items in collections that track work. System
// collections (conventions, playbooks) hold rules and procedures, not work,
// and a REFERENCE collection (PLAN-3535, e.g. docs) holds material that just
// exists, so "is this waiting on a human" has no meaning there; a terminal
// item has no next step to wait on.
func attentionEligible(item *models.Item, coll *models.Collection) bool {
	if coll.IsSystem || !models.CollectionTracksWorkJSON(coll.Settings) {
		return false
	}
	var schema models.CollectionSchema
	_ = models.UnmarshalItemFieldSchema([]byte(coll.Schema), &schema)
	var settings models.CollectionSettings
	if coll.Settings != "" {
		_ = json.Unmarshal([]byte(coll.Settings), &settings)
	}
	var fields map[string]any
	if item.Fields != "" {
		_ = json.Unmarshal([]byte(item.Fields), &fields)
	}
	return !models.IsTerminalItem(fields, schema, settings)
}

// NoulValue reads the probability out of a stored Noul answer. ok is false
// for a row of another kind or an unreadable answer.
func NoulValue(d models.ItemDecision) (p float64, ok bool) {
	if d.Kind != string(KindNoul) {
		return 0, false
	}
	var a storedAnswer
	if err := json.Unmarshal(d.Answer, &a); err != nil || a.Noul == nil {
		return 0, false
	}
	return *a.Noul, true
}

// ProductionRegistry returns a registry holding every question set the
// server ships. ConventionsCommentsSet is defined but NOT shipped: it was
// retired in TASK-3924 (42% of all provider calls on 10-17 for 7 chip-level
// answers; IDEA-3906's trail). Its stored rows read as stale questions.
//
// A registration error is a programming error in a set defined in this
// package, and a test (TestProductionRegistryRegisters) holds it to none.
func ProductionRegistry() (*Registry, error) {
	reg := NewRegistry()
	if err := reg.Register(AttentionSet()); err != nil {
		return nil, err
	}
	if err := reg.Register(ConventionsSet()); err != nil {
		return nil, err
	}
	return reg, nil
}
