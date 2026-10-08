package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3119 U1: executable conventions. One Noul per active `always`
// convention, asked about every item write in a user collection: "does this
// content break it?". The answer becomes a "Possibly breaks CONVE-N" chip; it
// NEVER blocks or refuses a write.
//
// Measured before it was built (U0, TASK-3119's trail, 116 hand-labelled
// cases): precise when it fires on a convention whose subject is in the item,
// but shy (it catches blatant breaks, not subtle ones), and silent on rules
// that don't apply to an item (a behaviour rule fired 0 of 20). Hence the
// chip wording, and no "complies" state anywhere: no chip means nothing.
const (
	ConventionsSetName = "conventions"

	// ConventionKeyPrefix starts every question key: conv:<CONVE-ref>.
	ConventionKeyPrefix = "conv:"

	// ConventionThreshold is the Noul probability at or above which a chip
	// shows. U0 measured it: 100% precision on the conventions whose subject
	// is in the item.
	ConventionThreshold = 0.9

	// conventionMaxBodyBytes skips a convention whose body is longer: it would
	// dominate every call's prompt (one convention here is 31 KB). Skips are
	// logged by ref.
	conventionMaxBodyBytes = 4096

	// conventionsPerCall bounds the questions in one provider call. 12, from
	// the U2 gates' measure C (TASK-3119 trail): up to 13 questions in ONE
	// call gave identical precision and recall to one question per call on
	// every convention, and moved 0 of 116 answers by more than 0.2. The
	// state is sent once per call, so fewer calls is most of the saving.
	conventionsPerCall = 12

	// decisionCheckField is the conventions schema field that switches a
	// convention's check off (value "off"); absent or anything else is on.
	decisionCheckField = "decision_check"
)

// conventionQuestion is the Noul, worded exactly as U0 measured it.
func conventionQuestion(body string) Question {
	return Noul(
		"This content violates the following convention: "+body,
		"the content does what the convention forbids, or lacks what it requires",
		"the content complies with the convention, or the convention does not apply to it",
	)
}

// ConventionsSet returns the `conventions` question set.
func ConventionsSet() QuestionSet {
	return QuestionSet{
		Name:       ConventionsSetName,
		Resolve:    resolveConventions,
		WithLinks:  true,
		NoTrail:    true,
		MaxPerCall: conventionsPerCall,
		// User collections only: a convention or playbook is not work an
		// item-content rule is about.
		Eligible: func(_ *models.Item, coll *models.Collection) bool { return !coll.IsSystem },
	}
}

// ConventionsCommentsSetName is the comment subject of the conventions check
// (TASK-3119 U2b): the same questions, asked about each comment alone.
const ConventionsCommentsSetName = "conventions_comments"

// ConventionsCommentsSet returns the `conventions_comments` question set:
// ConventionsSet's questions, asked about each of the item's recent comments
// with the item's title and collection for context (BuildCommentState).
func ConventionsCommentsSet() QuestionSet {
	return QuestionSet{
		Name:       ConventionsCommentsSetName,
		Resolve:    resolveConventions,
		PerComment: true,
		// stateFor is not used for this set's asking; NoTrail keeps the item
		// state Decisions builds for it from reading comments it ignores.
		NoTrail:    true,
		MaxPerCall: conventionsPerCall,
		Eligible:   func(_ *models.Item, coll *models.Collection) bool { return !coll.IsSystem },
	}
}

// resolveConventions is the workspace's active `always` conventions whose
// check is not switched off, one question each, keyed conv:<ref>.
func resolveConventions(_ context.Context, s *store.Store, workspaceID string) (map[string]Question, error) {
	colls, err := s.ListTraitedCollections(workspaceID)
	if err != nil {
		return nil, err
	}
	coll := collections.FindByArtifactKind(colls, collections.BuiltinConvention)
	if coll == nil {
		return map[string]Question{}, nil
	}
	items, err := s.ListItems(workspaceID, models.ItemListParams{
		ScopeCollectionID: coll.ID,
		Fields:            map[string]string{"status": "active", "trigger": "always"},
	})
	if err != nil {
		return nil, fmt.Errorf("list conventions: %w", err)
	}
	out := make(map[string]Question, len(items))
	for i := range items {
		it := &items[i]
		if conventionCheckOff(it) || it.Ref == "" {
			continue
		}
		if len(it.Content) > conventionMaxBodyBytes {
			logSkippedConvention(workspaceID, it.Ref, len(it.Content))
			continue
		}
		out[ConventionKeyPrefix+it.Ref] = conventionQuestion(it.Content)
	}
	return out, nil
}

func conventionCheckOff(it *models.Item) bool {
	if it.Fields == "" {
		return false
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(it.Fields), &fields); err != nil {
		return false
	}
	v, _ := fields[decisionCheckField].(string)
	return v == "off"
}

// skipLogged makes each skipped convention named once per process, not on
// every write that resolves the set.
var skipLogged sync.Map

func logSkippedConvention(workspaceID, ref string, size int) {
	key := workspaceID + "/" + ref
	if _, seen := skipLogged.LoadOrStore(key, true); seen {
		return
	}
	slog.Info("conventions check: skipping a convention over the body cap",
		"workspace_id", workspaceID, "ref", ref, "bytes", size, "cap_bytes", conventionMaxBodyBytes)
}
