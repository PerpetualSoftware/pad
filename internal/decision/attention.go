package decision

import (
	"encoding/json"
	"errors"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The `attention` question set (PLAN-3114 unit 3, TASK-3118): three Nouls
// over an open item and its recent trail, read by the dashboard's attention
// section and shown as chips on the item page.
//
// The instructions and criteria of needs_human_decision and blocked are
// carried VERBATIM from the day-73 eval (eval2.py, section D), which measured
// needs_human_decision at AUC 0.939 over 70 human-tasks vs 70 tasks. Rewording
// either changes its question_hash and re-asks every item, and the eval's
// figure stops describing the question being asked — so change them only
// with a re-run. waiting_on_external was not in the eval.
//
// The eval's state was {title, body}; the runner sends the richer
// BuildItemState (collection, fields, and the recent trail as well), so the
// eval figure is evidence about the question, not a measurement of this
// state shape. See TASK-3118's trail.
const (
	AttentionSetName = "attention"

	AttentionNeedsHuman      = "needs_human_decision"
	AttentionBlocked         = "blocked"
	AttentionWaitingExternal = "waiting_on_external"

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
			AttentionBlocked: Noul(
				"The item's text says the work is currently blocked on something outside the item itself.",
				"an explicit dependency, wait, or blocker is named",
				"no blocker is stated; the work can start",
			),
			AttentionWaitingExternal: Noul(
				"The next step on this work item is an action by someone outside this workspace.",
				"the item is waiting on a person, team, vendor, or service outside the workspace to act",
				"the next step is an action by the workspace's own people or agents",
			),
		},
		Eligible: attentionEligible,
	}
}

// attentionEligible admits open items in user collections. System
// collections (conventions, playbooks) hold rules and procedures, not work,
// so "is this waiting on a human" has no meaning there; a terminal item has
// no next step to wait on.
func attentionEligible(item *models.Item, coll *models.Collection) bool {
	if coll.IsSystem {
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
// server ships. A registration error is a programming error in a set defined
// in this package, and a test (TestProductionRegistryRegisters) holds it to
// none.
func ProductionRegistry() (*Registry, error) {
	reg := NewRegistry()
	if err := reg.Register(AttentionSet()); err != nil {
		return nil, err
	}
	return reg, nil
}

// WorkspaceFlags returns, for every item in the workspace, the question keys
// in the named set whose latest answer is a Noul at or above threshold AND is
// CURRENT — computed for the question as registered now, under the model
// pinned now, from the item's state as it stands now, for an item the set
// still applies to (open, in a user collection). That is the same
// currency rule [Runner.Decisions] applies, so the dashboard and the item page
// never disagree about whether an answer still describes the item.
//
// Cost: one batch read for the workspace, then one state read (the item plus
// its recent trail) per item holding at least one ABOVE-THRESHOLD answer. An
// answer below the threshold is never surfaced, so its currency is never
// checked; the per-item reads are bounded by the flagged items, not by the
// workspace.
//
// An answer whose item has moved on is dropped rather than shown: the change
// that moved it owes a job, and the tick answers again. What can lag without
// bound is a provider outage, which failing reports.
//
// failing reports that some owed job in the set has a recorded failure — the
// provider erroring. A nil runner returns nothing, not an error.
func (r *Runner) WorkspaceFlags(workspaceID, setName string, threshold float64) (flags map[string]map[string]float64, failing bool, err error) {
	if r == nil {
		return nil, false, nil
	}
	qs, ok := r.registry.Get(setName)
	if !ok {
		return nil, false, ErrUnknownSet
	}
	model := r.provider.Model()
	qhashNow := make(map[string]string, len(qs.Questions))
	for k, q := range qs.Questions {
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
		if want, ok := qhashNow[d.QuestionKey]; !ok || d.QuestionHash != want {
			continue
		}
		p, ok := NoulValue(d)
		if !ok || p < threshold {
			continue
		}
		if _, seen := byItem[d.ItemID]; !seen {
			order = append(order, d.ItemID)
		}
		byItem[d.ItemID] = append(byItem[d.ItemID], candidate{d.QuestionKey, p, d.StateHash})
	}
	flags = make(map[string]map[string]float64)
	for _, itemID := range order {
		item, st, serr := r.State(itemID)
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
		for _, c := range byItem[itemID] {
			if c.stateHash != st.Hash {
				continue
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
