package server

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/decision"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/needs"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// The importance and simplicity scorers on GET /me/needs (TASK-3959, IDEA-3906
// B4b). Both fill needs.Row scores after visibility, routing and snooze, and
// neither calls a provider: simplicity reads the effort answers the decision
// runner stored, and falls back to a text heuristic. The scores are computed at
// read time for the viewer and stored nowhere.
//
// Score.Why carries a FIXED vocabulary (needsWhyVocabulary, listed in
// docs/api.md): no title, ref, count or other free text ever enters it.

// Why strings. Every string a scorer may emit is in needsWhyVocabulary.
const (
	whyPriorityCritical = "priority critical"
	whyPriorityHigh     = "priority high"
	whyPriorityLow      = "priority low"
	whyBlocksWork       = "blocks other open work"
	whyOverdue          = "overdue"
	whyDueToday         = "due today"
	whyDueSoon          = "due within two weeks"
	whyWaitingWeek      = "waiting a week or more"
	whyWaitingTwoWeeks  = "waiting two weeks or more"
	whyAsked            = "asked of you"
	whyNeedsHuman       = "judged to need a human"
	whyEstimated        = "estimated from the text"
	whyRefreshing       = "estimate is being refreshed"
)

var needsWhyVocabulary = []string{
	whyPriorityCritical, whyPriorityHigh, whyPriorityLow, whyBlocksWork,
	whyOverdue, whyDueToday, whyDueSoon, whyWaitingWeek, whyWaitingTwoWeeks,
	whyAsked, whyNeedsHuman, whyEstimated, whyRefreshing,
}

// Importance weights (the design's B3): priority, blocks, deadline, stakes,
// needs-human, age.
const (
	wPriority = 0.25
	wBlocks   = 0.20
	wDeadline = 0.20
	wStakes   = 0.15
	wHuman    = 0.10
	wAge      = 0.10
)

// stakesPart is the high_stakes component (S). It is held at 0 and nothing
// reads or surfaces the high_stakes answer: the question is not trusted until
// its labelled eval has run (lead ruling 440f5c63, filed as TASK-4011). Until
// then importance tops out at 85 and is NOT renormalised, because rescaling
// would move every score the day S switches on. Reading the answer into this
// constant is the one change that switch makes.
const stakesPart = 0.0

// The fan-out bounds: breadth-first over blocks, depth at most 5, count at most 99.
const (
	fanoutDepth = 5
	fanoutCap   = 99
)

// needsHintMax is the longest hint, in bytes (all ASCII).
const needsHintMax = 40

// effortHints are the hint heads by effort level; the medium suffix follows.
var effortHints = [4]string{"yes/no", "5 min", "under an hour", "research"}

// effortMediumSuffix is appended to the hint for a confident effort_medium
// answer; "unclear", an unknown option and a low-probability answer add none.
var effortMediumSuffix = map[string]string{
	"desk":         " at desk",
	"device":       ", needs a device",
	"other_person": ", needs someone else",
}

// needsMemo holds ONE workspace's batch reads for ONE call of the scorers, so
// importance and simplicity read each once. It is built per request per
// workspace and dropped with it: never cached across requests, since every
// input (links, answers, jobs, items) changes between them.
type needsMemo struct {
	s      *Server
	wsID   string
	runner *decision.Runner

	mu     sync.Mutex
	items  memoSlot[map[string]store.NeedsScoringItem]
	edges  memoSlot[[]store.BlocksEdge]
	colls  memoSlot[[]models.Collection]
	decs   memoSlot[map[string][]models.ItemDecision]
	owed   memoSlot[map[string]bool]
	qhash  memoSlot[map[string]string]
	human  memoSlot[map[string]map[string]float64]
	graph  *blocksGraph
	graphE error
}

type memoSlot[T any] struct {
	done bool
	val  T
	err  error
}

func memoLoad[T any](m *needsMemo, read string, slot *memoSlot[T], fn func() (T, error)) (T, error) {
	if !slot.done {
		slot.done = true
		if m.s.needsMemoLoad != nil {
			m.s.needsMemoLoad(m.wsID + ":" + read)
		}
		slot.val, slot.err = fn()
	}
	return slot.val, slot.err
}

func (s *Server) newNeedsMemo(wsID string) *needsMemo {
	return &needsMemo{s: s, wsID: wsID, runner: s.decisionRunner()}
}

type needsMemoKey struct{}

func withNeedsMemo(ctx context.Context, m *needsMemo) context.Context {
	return context.WithValue(ctx, needsMemoKey{}, m)
}

// memoFor is the memo the handler put in ctx for this workspace, or a fresh one
// for a caller (a test) that did not.
func (s *Server) memoFor(ctx context.Context, wsID string) *needsMemo {
	if m, ok := ctx.Value(needsMemoKey{}).(*needsMemo); ok && m.wsID == wsID {
		return m
	}
	return s.newNeedsMemo(wsID)
}

func rowIDs(rows []*needs.Row) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ItemID
	}
	return ids
}

// itemsFor reads the rows' items once and returns that read to every later
// caller, whatever rows it passes: every scorer sharing a memo must pass the
// same row slice (the handler does). The same holds for answers.
func (m *needsMemo) itemsFor(rows []*needs.Row) (map[string]store.NeedsScoringItem, error) {
	return memoLoad(m, "items", &m.items, func() (map[string]store.NeedsScoringItem, error) {
		return m.s.store.NeedsScoringItems(m.wsID, rowIDs(rows))
	})
}

// blocks is the workspace's blocks graph, built once. Like every memo read it
// runs under m.mu, which both scorers' Score methods hold for their whole call.
func (m *needsMemo) blocks() (*blocksGraph, error) {
	if m.graph != nil || m.graphE != nil {
		return m.graph, m.graphE
	}
	edges, err := memoLoad(m, "blocks_edges", &m.edges, func() ([]store.BlocksEdge, error) { return m.s.store.GetBlocksEdges(m.wsID) })
	if err == nil {
		var colls []models.Collection
		colls, err = memoLoad(m, "collections", &m.colls, func() ([]models.Collection, error) { return m.s.store.ListCollections(m.wsID) })
		if err == nil {
			m.graph = newBlocksGraph(edges, colls)
		}
	}
	m.graphE = err
	return m.graph, err
}

// answers returns the latest attention answers of the rows' items, the owed
// jobs, and the question hashes as the set asks them now. All three are nil
// without a runner: no provider, no answers worth trusting.
func (m *needsMemo) answers(ctx context.Context, rows []*needs.Row) (decs map[string][]models.ItemDecision, owed map[string]bool, qh map[string]string, err error) {
	if m.runner == nil {
		return nil, nil, nil, nil
	}
	if qh, err = memoLoad(m, "question_hashes", &m.qhash, func() (map[string]string, error) {
		return m.runner.CurrentQuestionHashes(ctx, m.wsID, decision.AttentionSetName)
	}); err != nil {
		return
	}
	if decs, err = memoLoad(m, "decisions", &m.decs, func() (map[string][]models.ItemDecision, error) {
		return m.s.store.LatestDecisionsForItems(m.wsID, rowIDs(rows), decision.AttentionSetName)
	}); err != nil {
		return
	}
	owed, err = memoLoad(m, "owed_jobs", &m.owed, func() (map[string]bool, error) {
		return m.s.store.OwedDecisionJobs(m.wsID, rowIDs(rows), decision.AttentionSetName)
	})
	return
}

// humanCurrent is the inbox's own verdict on which of the rows' attention
// answers are current: the flag path (WorkspaceFlagsForItems) at threshold 0
// returns every answer current by question hash AND item state, or held while
// its refresh is owed, by item and key. H and "judged to need a human" read
// needs_human only through it, so they agree with whether the router counts
// the item as flagged. Its failing bit is not read. Nil without a runner.
func (m *needsMemo) humanCurrent(rows []*needs.Row) (map[string]map[string]float64, error) {
	if m.runner == nil {
		return nil, nil
	}
	return memoLoad(m, "attention_current", &m.human, func() (map[string]map[string]float64, error) {
		cur, _, err := m.runner.WorkspaceFlagsForItems(m.wsID, decision.AttentionSetName, 0, rowIDs(rows))
		return cur, err
	})
}

// withCurrentHuman is decs without the needs_human answer unless the flag path
// still holds it current (cur, from humanCurrent). It copies; decs is shared.
func withCurrentHuman(decs []models.ItemDecision, cur map[string]float64) []models.ItemDecision {
	if _, ok := cur[decision.AttentionNeedsHuman]; ok {
		return decs
	}
	out := make([]models.ItemDecision, 0, len(decs))
	for _, d := range decs {
		if d.QuestionKey != decision.AttentionNeedsHuman {
			out = append(out, d)
		}
	}
	return out
}

// currentAnswer is the item's latest answer for a question key, if it answers
// the question as asked now.
func currentAnswer(decs []models.ItemDecision, qh map[string]string, key string) (models.ItemDecision, bool) {
	for _, d := range decs {
		if d.QuestionKey == key && qh[key] != "" && d.QuestionHash == qh[key] {
			return d, true
		}
	}
	return models.ItemDecision{}, false
}

// newNeedsScorers is the pair New registers. The runner is read per call, not
// here: it is attached after New (SetDecisionRunner).
func (s *Server) newNeedsScorers() []needs.Scorer {
	return []needs.Scorer{&importanceScorer{s: s}, &simplicityScorer{s}}
}

func nowOr(v needs.Viewer) time.Time {
	if v.Now.IsZero() {
		return time.Now().UTC()
	}
	return v.Now
}

// ---- importance ----

// loc is the calendar the deadline part reads "today" in; nil means time.Local,
// the dashboard's own (overdueToday). A field so a test needs no global.
type importanceScorer struct {
	s   *Server
	loc *time.Location
}

func (*importanceScorer) Name() string { return "importance" }

func (sc *importanceScorer) Score(ctx context.Context, v needs.Viewer, scope needs.Scope, rows []*needs.Row) (bool, error) {
	if len(rows) == 0 {
		return false, nil
	}
	m := sc.s.memoFor(ctx, scope.WorkspaceID)
	m.mu.Lock()
	defer m.mu.Unlock()
	items, err := m.itemsFor(rows)
	if err != nil {
		return true, err
	}
	g, err := m.blocks()
	if err != nil {
		return true, err
	}
	decs, _, qh, err := m.answers(ctx, rows)
	if err != nil {
		return true, err
	}
	hc, err := m.humanCurrent(rows)
	if err != nil {
		return true, err
	}
	now := nowOr(v)
	vis := newScopeSet(scope)
	for _, r := range rows {
		r.Importance = importanceIn(sc.loc, r, items[r.ItemID], g.fanout(r.ItemID, vis), withCurrentHuman(decs[r.ItemID], hc[r.ItemID]), qh, now)
	}
	return false, nil
}

func importanceOf(r *needs.Row, it store.NeedsScoringItem, blocked int, decs []models.ItemDecision, qh map[string]string, now time.Time) *needs.Score {
	return importanceIn(nil, r, it, blocked, decs, qh, now)
}

// importanceParts are the unweighted inputs, each in [0,1].
type importanceParts struct{ p, b, d, s, h, a float64 }

func importanceIn(loc *time.Location, r *needs.Row, it store.NeedsScoringItem, blocked int, decs []models.ItemDecision, qh map[string]string, now time.Time) *needs.Score {
	sc, _ := importanceDetail(loc, r, it, blocked, decs, qh, now)
	return sc
}

func importanceDetail(loc *time.Location, r *needs.Row, it store.NeedsScoringItem, blocked int, decs []models.ItemDecision, qh map[string]string, now time.Time) (*needs.Score, importanceParts) {
	var why []string
	p := 0.5
	switch strings.ToLower(r.Item.Priority) {
	case "critical":
		p, why = 1, append(why, whyPriorityCritical)
	case "high":
		p, why = 0.75, append(why, whyPriorityHigh)
	case "low":
		p, why = 0.25, append(why, whyPriorityLow)
	}
	b := math.Min(1, math.Log2(1+float64(blocked))/math.Log2(17))
	if blocked > 0 {
		why = append(why, whyBlocksWork)
	}
	d, dwhy := deadlinePart(it.Fields, now, loc)
	if dwhy != "" {
		why = append(why, dwhy)
	}
	days := now.Sub(r.WaitingSince).Hours() / 24
	a := math.Max(0, math.Min(1, days/14))
	switch {
	case days >= 14:
		why = append(why, whyWaitingTwoWeeks)
	case days >= 7:
		why = append(why, whyWaitingWeek)
	}
	h := 0.5
	if r.Why.Rule == needs.RuleAsked {
		h = 1
		why = append(why, whyAsked)
	} else if d, ok := currentAnswer(decs, qh, decision.AttentionNeedsHuman); ok {
		if pr, ok := decision.NoulValue(d); ok {
			h = math.Max(0, math.Min(1, pr))
			// The router's own threshold (handlers_me_needs.go), so the Why
			// agrees with whether the item is flagged.
			if h >= decision.AttentionThreshold {
				why = append(why, whyNeedsHuman)
			}
		}
	}
	val := wPriority*p + wBlocks*b + wDeadline*d + wStakes*stakesPart + wHuman*h + wAge*a
	// Snap before rounding: the weighted sum is a float, and a value that is
	// exactly x.5 mathematically must not land either side by summation order.
	v100 := math.Round(100*val*1e9) / 1e9
	return &needs.Score{Value: int(math.Round(v100)), Why: why}, importanceParts{p, b, d, stakesPart, h, a}
}

// deadlinePart is D: 1 when overdue or due today, falling to 0 over 14 days,
// 0 with no date. "Overdue" is decided by the dashboard's own itemOverdue over
// the server-local day (overdueToday), so the two agree for every stored value,
// timestamps and malformed dates included. Date parsing only separates "due
// today" from the slope among rows that are not overdue; the earliest of
// due_date and end_date counts. A nil loc is time.Local.
func deadlinePart(fieldsJSON string, now time.Time, loc *time.Location) (float64, string) {
	if loc == nil {
		loc = time.Local
	}
	todayStr := overdueToday(now.In(loc))
	if _, _, ok := itemOverdue(fieldsJSON, todayStr); ok {
		return 1, whyOverdue
	}
	today, _ := time.Parse("2006-01-02", todayStr)
	best, found := time.Time{}, false
	for _, key := range overdueDateFields {
		v := extractFieldValue(fieldsJSON, key)
		if len(v) < 10 {
			continue
		}
		t, err := time.Parse("2006-01-02", v[:10])
		if err != nil {
			continue
		}
		if !found || t.Before(best) {
			best, found = t, true
		}
	}
	if !found {
		return 0, ""
	}
	days := best.Sub(today).Hours() / 24
	switch {
	case days <= 0:
		return 1, whyDueToday
	case days < 14:
		return 1 - days/14, whyDueSoon
	}
	return 0, ""
}

// ---- blocks fan-out ----

// scopeSet is what the viewer may see in one workspace, as sets.
type scopeSet struct {
	all         bool
	collections map[string]bool
	items       map[string]bool
}

func newScopeSet(sc needs.Scope) scopeSet {
	v := scopeSet{all: sc.Unrestricted, collections: map[string]bool{}, items: map[string]bool{}}
	for _, id := range sc.CollectionIDs {
		v.collections[id] = true
	}
	for _, id := range sc.ItemIDs {
		v.items[id] = true
	}
	return v
}

func (v scopeSet) sees(collectionID, itemID string) bool {
	return v.all || v.collections[collectionID] || v.items[itemID]
}

// blocksGraph is a workspace's blocks links by blocker, with what it takes to
// tell whether a blocked item is still open.
type blocksGraph struct {
	bySource map[string][]store.BlocksEdge
	colls    map[string]*models.Collection
	open     map[string]bool
}

func newBlocksGraph(edges []store.BlocksEdge, colls []models.Collection) *blocksGraph {
	g := &blocksGraph{bySource: map[string][]store.BlocksEdge{}, colls: map[string]*models.Collection{}, open: map[string]bool{}}
	for _, e := range edges {
		g.bySource[e.SourceID] = append(g.bySource[e.SourceID], e)
	}
	for i := range colls {
		g.colls[colls[i].ID] = &colls[i]
	}
	return g
}

func (g *blocksGraph) isOpen(e store.BlocksEdge) bool {
	if o, ok := g.open[e.TargetID]; ok {
		return o
	}
	var fields map[string]any
	_ = json.Unmarshal([]byte(e.TargetFields), &fields)
	// A target whose collection is not in the map cannot be judged terminal or
	// not, so it fails closed: neither counted nor walked through.
	open := false
	if c := g.colls[e.TargetCollectionID]; c != nil {
		var schema models.CollectionSchema
		_ = models.UnmarshalItemFieldSchema([]byte(c.Schema), &schema)
		var settings models.CollectionSettings
		if c.Settings != "" {
			_ = json.Unmarshal([]byte(c.Settings), &settings)
		}
		open = !models.IsTerminalItem(fields, schema, settings)
	}
	g.open[e.TargetID] = open
	return open
}

// fanout counts the distinct items the root blocks, directly or through other
// blocked items: breadth-first with a visited set (the root starts in it, so a
// cycle never counts the root), at most fanoutDepth links deep, stopping at
// fanoutCap. An item the viewer cannot see, or that is terminal, is neither
// counted nor walked through, and the answer is a count only.
func (g *blocksGraph) fanout(root string, vis scopeSet) int {
	visited := map[string]bool{root: true}
	frontier := []string{root}
	n := 0
	for depth := 0; depth < fanoutDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, id := range frontier {
			for _, e := range g.bySource[id] {
				if visited[e.TargetID] {
					continue
				}
				visited[e.TargetID] = true
				if !vis.sees(e.TargetCollectionID, e.TargetID) || !g.isOpen(e) {
					continue
				}
				if n++; n >= fanoutCap {
					return fanoutCap
				}
				next = append(next, e.TargetID)
			}
		}
		frontier = next
	}
	return n
}

// ---- simplicity ----

type simplicityScorer struct{ s *Server }

func (*simplicityScorer) Name() string { return "simplicity" }

func (sc *simplicityScorer) Score(ctx context.Context, _ needs.Viewer, scope needs.Scope, rows []*needs.Row) (bool, error) {
	if len(rows) == 0 {
		return false, nil
	}
	m := sc.s.memoFor(ctx, scope.WorkspaceID)
	m.mu.Lock()
	defer m.mu.Unlock()
	items, err := m.itemsFor(rows)
	if err != nil {
		return true, err
	}
	decs, owed, qh, err := m.answers(ctx, rows)
	if err != nil {
		return true, err
	}
	for _, r := range rows {
		r.Simplicity = simplicityOf(r, items[r.ItemID], decs[r.ItemID], qh, owed[r.ItemID])
	}
	return false, nil
}

// simplicityOf is (3 - effort)/3 of 100. Effort is the level the stored effort
// answer picked (argmax, so the value and the hint cannot disagree), when that
// answer is to the question as asked now and its level reads as 0-3; otherwise
// a heuristic on the text.
// "Current" is the question hash only, not the item's state: an answer one edit
// behind is used, and a job owed for the item says so in Why.
func simplicityOf(r *needs.Row, it store.NeedsScoringItem, decs []models.ItemDecision, qh map[string]string, refreshing bool) *needs.Score {
	var why []string
	level, hint := 0, ""
	if d, ok := currentAnswer(decs, qh, decision.AttentionEffort); ok {
		if l, ok := decision.EffortLevel(d); ok {
			level, hint = l, effortHints[l]
			if md, ok := currentAnswer(decs, qh, decision.AttentionEffortMedium); ok {
				if c, p, ok := decision.ChoiceValue(md); ok && p >= decision.EffortMediumMinProb {
					hint += effortMediumSuffix[c]
				}
			}
		} else {
			level, hint = heuristicEffort(r.Item.Title, it.ContentRunes), ""
		}
	} else {
		level = heuristicEffort(r.Item.Title, it.ContentRunes)
	}
	if hint == "" {
		why = append(why, whyEstimated)
	}
	if refreshing {
		why = append(why, whyRefreshing)
	}
	return &needs.Score{Value: int(math.Round(float64(3-level) / 3 * 100)), Hint: hint, Why: why}
}

// heuristicEffort is the no-answer estimate: 0 for a question title with a
// short body, else by body length.
func heuristicEffort(title string, bodyRunes int) int {
	switch {
	case strings.HasSuffix(strings.TrimSpace(title), "?") && bodyRunes <= 600:
		return 0
	case bodyRunes <= 1500:
		return 1
	case bodyRunes <= 6000:
		return 2
	}
	return 3
}
