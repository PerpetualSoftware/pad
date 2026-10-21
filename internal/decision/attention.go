package decision

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The `attention` question set (PLAN-3114 unit 3, TASK-3118): four questions over
// an open item and its recent trail, read by the dashboard's attention
// section and shown as a chip on the item page.
// One of the four, needs_human_decision, is read by both; the other three are
// staged (below).
//
// The instruction and criteria of needs_human_decision are carried VERBATIM
// from the day-73 eval (eval2.py, section D), which measured it at AUC 0.939
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
// THREE MORE QUESTIONS are STAGED here (TASK-3921, IDEA-3906 half B4a):
// effort, effort_medium and high_stakes. They ride the same call as
// needs_human_decision, so the cost is output tokens, not a second request.
// Their reader is TASK-3959 (B4b): the needs-you scorers read effort and
// effort_medium (effort_read.go), so the set lists all three in Unsurfaced and
// no flag, chip or brief entry is made from them. high_stakes is read by
// nothing yet (its labelled eval is TASK-4011). Their answers are stored, and
// the item's decisions list returns them. The read rule that belongs to their
// reader, not to the set: an effort_medium answer whose top probability is
// under .5 means "unclear" (EffortMediumMinProb).
//
// The eval's state was {title, body}; the runner sends the richer
// BuildItemState (collection, fields, and the recent trail as well), so the
// eval figure is evidence about the question, not a measurement of this
// state shape. See TASK-3118's trail.
const (
	AttentionSetName = "attention"

	AttentionNeedsHuman = "needs_human_decision"

	// Staged before their reader (TASK-3959); see the header.
	AttentionEffort       = "effort"
	AttentionEffortMedium = "effort_medium"
	AttentionHighStakes   = "high_stakes"

	// AttentionThreshold is the Noul probability at or above which the
	// dashboard surfaces an item. 0.7 sits between the eval's 0.5 operating
	// point (precision 93%, recall 76%) and the docs' 0.9 destructive-action
	// band: a dashboard entry is advisory, not an action.
	AttentionThreshold = 0.7
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
		Unsurfaced: map[string]bool{
			AttentionEffort:       true,
			AttentionEffortMedium: true,
			AttentionHighStakes:   true,
		},
		Eligible:  attentionEligible,
		Threshold: AttentionThreshold,
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

// WorkspaceFlags returns, for every item in the workspace, the question keys
// in the named set whose latest answer is a Noul at or above threshold AND is
// CURRENT — computed for the question as registered now, under the model
// pinned now, from the item's state as it stands now, for an item the set
// still applies to (open, in a user collection). That is the same
// currency rule [Runner.Decisions] applies, with one deliberate exception:
// an answer whose re-ask is pending (below) is kept, although the item page
// shows it as not current until the tick answers again.
//
// Cost: one batch read for the workspace, then one state read (the item plus
// its recent trail) per item holding at least one ABOVE-THRESHOLD answer. An
// answer below the threshold is never surfaced, so its currency is never
// checked; the per-item reads are bounded by the flagged items, not by the
// workspace.
//
// An answer whose item has moved on is kept while its re-ask is PENDING
// (TASK-3915): a job is owed for the item and set and has no recorded
// failure, and the set still applies to the item. The change that moved the
// item owes that job, and the answer it replaces is the best estimate until
// the tick answers again; dropping it made the dashboard entry vanish for
// the whole wait and come back, which a debounced re-ask (TASK-3916) would
// stretch to an hour. Without a pending job, or once the job has failed, the
// moved answer is dropped: nothing is coming to replace it, and a provider
// outage, which failing reports, must not keep a stale flag up without
// bound. That costs one job read per moved candidate, never per item.
//
// failing reports that some owed job in the set has a recorded failure — the
// provider erroring. A nil runner returns nothing, not an error.
func (r *Runner) WorkspaceFlags(workspaceID, setName string, threshold float64) (flags map[string]map[string]float64, failing bool, err error) {
	return r.workspaceFlags(workspaceID, setName, threshold, nil)
}

// WorkspaceFlagsForItems is [Runner.WorkspaceFlags] restricted to the given
// items: the decision rows of any other item are dropped BEFORE its state is
// read, so an item the caller may not see can neither be evaluated nor make
// the call fail (TASK-3920: an error from a hidden item would leak one bit).
// An empty id list evaluates nothing.
func (r *Runner) WorkspaceFlagsForItems(workspaceID, setName string, threshold float64, itemIDs []string) (flags map[string]map[string]float64, failing bool, err error) {
	only := make(map[string]bool, len(itemIDs))
	for _, id := range itemIDs {
		only[id] = true
	}
	return r.workspaceFlags(workspaceID, setName, threshold, only)
}

// workspaceFlags implements both; a nil only evaluates every item.
func (r *Runner) workspaceFlags(workspaceID, setName string, threshold float64, only map[string]bool) (flags map[string]map[string]float64, failing bool, err error) {
	if r == nil {
		return nil, false, nil
	}
	qs, ok := r.registry.Get(setName)
	if !ok {
		return nil, false, ErrUnknownSet
	}
	model := r.provider.Model()
	// The set's questions as they stand now: fixed, or resolved for this
	// workspace (TASK-3119).
	questions, err := r.questionsFor(context.Background(), qs, workspaceID)
	if err != nil {
		return nil, false, err
	}
	qhashNow := make(map[string]string, len(questions))
	for k, q := range questions {
		qhashNow[k] = QuestionFingerprint(model, q)
	}
	rows, err := r.store.LatestWorkspaceDecisions(workspaceID, setName)
	if err != nil {
		return nil, false, err
	}
	// Candidates first, so the state read happens once per flagged item.
	type candidate struct {
		key       string
		p         float64
		stateHash string
	}
	byItem := make(map[string][]candidate)
	var order []string
	for _, d := range rows {
		if only != nil && !only[d.ItemID] {
			continue
		}
		// A staged key (TASK-3921) makes no flag and no state read.
		if qs.Unsurfaced[d.QuestionKey] {
			continue
		}
		p, v := surfacing(d, qhashNow[d.QuestionKey], threshold)
		if v != Surfaced {
			continue
		}
		if _, seen := byItem[d.ItemID]; !seen {
			order = append(order, d.ItemID)
		}
		byItem[d.ItemID] = append(byItem[d.ItemID], candidate{d.QuestionKey, p, d.StateHash})
	}
	flags = make(map[string]map[string]float64)
	for _, itemID := range order {
		item, st, serr := r.stateFor(itemID, qs)
		if errors.Is(serr, ErrItemGone) {
			continue
		}
		if serr != nil {
			return flags, false, serr
		}
		// The set must still apply — the item page's rule too (Decisions).
		// A collection schema edit can make an item terminal without moving
		// its state hash, so the hash check below would not catch it.
		ok, aerr := r.appliesNow(qs, item)
		if aerr != nil {
			return flags, false, aerr
		}
		if !ok {
			continue
		}
		var pending *bool
		for _, c := range byItem[itemID] {
			if c.stateHash != st.Hash {
				if pending == nil {
					job, jerr := r.store.GetDecisionJob(itemID, setName)
					if jerr != nil {
						return flags, false, jerr
					}
					owed := job != nil && job.LastError == ""
					pending = &owed
				}
				if !*pending {
					continue
				}
			}
			if flags[itemID] == nil {
				flags[itemID] = make(map[string]float64, len(byItem[itemID]))
			}
			flags[itemID][c.key] = c.p
		}
	}
	failing, err = r.store.DecisionJobsFailing(workspaceID, setName)
	if err != nil {
		return flags, false, err
	}
	return flags, failing, nil
}
