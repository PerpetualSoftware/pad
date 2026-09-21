package decision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// The runner: storage and evaluation of typed decisions about items
// (PLAN-3114 unit 2, TASK-3117).
//
// A write marks an evaluation owed (store.enqueueDecisionJobsTx, in the
// write's own transaction); the server's tick claims owed jobs and calls
// [Runner.Evaluate] for each. Nothing on a write path waits on the network.

// QuestionSet is a named group of questions asked about an item in ONE
// provider call.
type QuestionSet struct {
	// Name is the registry key and the stored question_set. Server-chosen,
	// never caller text.
	Name string

	// Collections restricts the set to items in these collection slugs.
	// Empty means every collection.
	Collections []string

	// Questions maps a stable question key to the question. A key is stored
	// with every answer, so renaming one orphans its history.
	Questions map[string]Question
}

func (qs QuestionSet) appliesTo(collectionSlug string) bool {
	if len(qs.Collections) == 0 {
		return true
	}
	for _, c := range qs.Collections {
		if c == collectionSlug {
			return true
		}
	}
	return false
}

// Registry holds the question sets later units register. This unit ships it
// empty in production; tests register their own.
type Registry struct {
	mu   sync.RWMutex
	sets map[string]QuestionSet
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{sets: map[string]QuestionSet{}} }

// Register adds a set, refusing an invalid one or a duplicate name: a second
// registration under one name would silently change what stored answers mean.
func (r *Registry) Register(qs QuestionSet) error {
	if qs.Name == "" {
		return errors.New("decision: question set has no name")
	}
	if len(qs.Questions) == 0 {
		return fmt.Errorf("decision: question set %q has no questions", qs.Name)
	}
	for key, q := range qs.Questions {
		if key == "" {
			return fmt.Errorf("decision: question set %q has an empty question key", qs.Name)
		}
		if err := q.Validate(); err != nil {
			return fmt.Errorf("decision: question set %q, question %q: %w", qs.Name, key, err)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.sets[qs.Name]; dup {
		return fmt.Errorf("decision: question set %q is already registered", qs.Name)
	}
	r.sets[qs.Name] = qs
	return nil
}

// Get returns a registered set.
func (r *Registry) Get(name string) (QuestionSet, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	qs, ok := r.sets[name]
	return qs, ok
}

// SetsFor names the sets that apply to a collection, sorted. It is the
// store's DecisionSetResolver, so it must stay pure (no database access): the
// store calls it inside the write transaction.
func (r *Registry) SetsFor(collectionSlug string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for name, qs := range r.sets {
		if qs.appliesTo(collectionSlug) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// The state bounds. They live beside the builder because they are PART OF THE
// HASH: changing one changes every item's state bytes, and so marks every
// stored answer not-current until it is re-evaluated. That is correct — the
// model would be reading different state — but it is a fleet-wide re-ask, so
// change them deliberately.
const (
	// RecentTrailWindow is how many of an item's newest comments the state
	// carries.
	RecentTrailWindow = 10

	// maxStateBodyRunes bounds the item body. The provider truncates only a
	// STRING state; a structured one over its budget is refused outright
	// (typesafe.go fitState), so the builder bounds the body itself — and
	// bounding it HERE, before hashing, is what keeps the hashed bytes
	// identical to the bytes sent.
	maxStateBodyRunes = 16000

	// maxStateCommentRunes bounds each trail comment, for the same reason.
	maxStateCommentRunes = 2000
)

// ItemState is the state a question set is asked about. Its JSON encoding is
// what the provider receives and what state_hash is computed over — one
// serialisation, see [BuildItemState].
type ItemState struct {
	Title      string            `json:"title"`
	Collection string            `json:"collection"`
	Fields     map[string]any    `json:"fields"`
	Body       string            `json:"body"`
	Trail      []ItemStateRemark `json:"recent_trail"`
}

// ItemStateRemark is one trail comment. The timestamp is included because
// "the last comment was three weeks ago" is a fact about the item; the
// comment's id is not, since it carries no meaning a model could read.
type ItemStateRemark struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// BuiltState is a built, serialised state.
type BuiltState struct {
	// Bytes is the exact JSON sent to the provider.
	Bytes json.RawMessage
	// Hash is hex sha256 of Bytes.
	Hash string
	// Truncated reports that the builder shortened the body or a comment.
	Truncated bool
}

// BuildItemState builds an item's state from its row and trail.
//
// ONE SERIALISATION PATH: the bytes returned are both hashed and sent. The
// runner passes them to the provider as json.RawMessage, which the provider's
// own json.Marshal emits verbatim (RawMessage compaction is the identity on
// json.Marshal output — pinned by a test that captures the request body).
// Hashing a separately-built value would be a second path that could drift.
//
// What is DELIBERATELY absent: seq, updated_at, sort orders, ids. None is
// something a question can read, and each changes on writes that change
// nothing else — a role-board reorder, a wiki-link re-resolution. Including
// them would make those writes cost a provider call (TASK-3117 ruling 2).
func BuildItemState(item *models.Item, comments []models.Comment) (BuiltState, error) {
	fields := map[string]any{}
	if item.Fields != "" {
		dec := json.NewDecoder(strings.NewReader(item.Fields))
		dec.UseNumber() // a stored 3.0 must not re-encode as 3 and move the hash
		if err := dec.Decode(&fields); err != nil {
			return BuiltState{}, fmt.Errorf("decision state: item %s fields: %w", item.ID, err)
		}
	}
	body, truncated := clipRunes(item.Content, maxStateBodyRunes)
	st := ItemState{
		Title:      item.Title,
		Collection: item.CollectionSlug,
		Fields:     fields,
		Body:       body,
		Trail:      []ItemStateRemark{},
	}
	if len(comments) > RecentTrailWindow {
		comments = comments[len(comments)-RecentTrailWindow:]
	}
	for _, c := range comments {
		cb, ct := clipRunes(c.Body, maxStateCommentRunes)
		truncated = truncated || ct
		st.Trail = append(st.Trail, ItemStateRemark{
			Author:    c.Author,
			Body:      cb,
			CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	b, err := json.Marshal(st)
	if err != nil {
		return BuiltState{}, fmt.Errorf("decision state: marshal item %s: %w", item.ID, err)
	}
	sum := sha256.Sum256(b)
	return BuiltState{Bytes: b, Hash: hex.EncodeToString(sum[:]), Truncated: truncated}, nil
}

func clipRunes(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos], true
		}
		i++
	}
	return s, false
}

// Runner evaluates owed decisions.
type Runner struct {
	store    *store.Store
	provider Provider
	registry *Registry

	// beforeAsk is a TEST-ONLY seam, nil in production: it runs immediately
	// before the final liveness check that precedes the provider call, so a
	// test can land a delete in exactly the window that check exists for.
	beforeAsk func()
}

// NewRunner returns a runner, or nil when provider is nil — the configured-off
// case. Every method is nil-safe, so the server holds a *Runner and never
// branches on the provider itself.
func NewRunner(s *store.Store, p Provider, reg *Registry) *Runner {
	if p == nil || s == nil {
		return nil
	}
	if reg == nil {
		reg = NewRegistry()
	}
	return &Runner{store: s, provider: p, registry: reg}
}

// Registry returns the runner's registry (nil for a nil runner).
func (r *Runner) Registry() *Registry {
	if r == nil {
		return nil
	}
	return r.registry
}

// Install wires the runner's registry into the store's write doors. With a
// nil runner it removes any resolver, so the doors enqueue nothing.
func (r *Runner) Install(s *store.Store) {
	if r == nil {
		s.SetDecisionSetResolver(nil)
		return
	}
	s.SetDecisionSetResolver(r.registry.SetsFor)
}

// ErrItemGone reports that the item no longer exists or is soft-deleted: the
// job is dropped rather than retried.
var ErrItemGone = errors.New("decision: item is gone")

// ErrUnknownSet reports a job for a set no longer registered: dropped.
var ErrUnknownSet = errors.New("decision: question set is not registered")

// ErrSetNotApplicable reports a job for a set that does not cover the item's
// CURRENT collection — the item moved after the job was owed: dropped.
var ErrSetNotApplicable = errors.New("decision: question set does not apply to the item's collection")

// State builds the item's current state.
func (r *Runner) State(itemID string) (*models.Item, BuiltState, error) {
	item, err := r.store.GetItem(itemID)
	if err != nil {
		return nil, BuiltState{}, fmt.Errorf("decision: read item %s: %w", itemID, err)
	}
	if item == nil || item.DeletedAt != nil {
		return nil, BuiltState{}, ErrItemGone
	}
	// The workspace or collection may be soft-deleted while the item row is
	// not; nothing of a deleted workspace goes to the provider.
	live, err := r.store.ItemEvaluable(itemID)
	if err != nil {
		return nil, BuiltState{}, err
	}
	if !live {
		return nil, BuiltState{}, ErrItemGone
	}
	comments, err := r.store.RecentComments(itemID, RecentTrailWindow)
	if err != nil {
		return nil, BuiltState{}, err
	}
	st, err := BuildItemState(item, comments)
	return item, st, err
}

// Evaluate asks the named set about the item, unless answers for its current
// state are already stored. It reports whether the provider was called.
func (r *Runner) Evaluate(ctx context.Context, itemID, setName string) (bool, error) {
	if r == nil {
		return false, nil
	}
	qs, ok := r.registry.Get(setName)
	if !ok {
		return false, ErrUnknownSet
	}
	item, st, err := r.State(itemID)
	if err != nil {
		return false, err
	}
	// Applicability is re-checked HERE, against the collection the item is in
	// now, not trusted from enqueue time: a job owed before a move still names
	// the old collection's set (codex round 2). Checked before the idempotency
	// read so an inapplicable set never costs a call or a query.
	if !qs.appliesTo(item.CollectionSlug) {
		return false, ErrSetNotApplicable
	}
	keys := make([]string, 0, len(qs.Questions))
	for k := range qs.Questions {
		keys = append(keys, k)
	}
	have, err := r.store.HasItemDecisionsAtState(item.ID, qs.Name, st.Hash, keys)
	if err != nil {
		return false, err
	}
	if have {
		return false, nil
	}

	if r.beforeAsk != nil {
		r.beforeAsk()
	}
	// Liveness again, as the LAST statement before the send (codex round 4):
	// the check in State ran before the idempotency read, and a delete landing
	// in between must still stop the call. RESIDUAL, stated rather than
	// implied: no fence can hold across a network request, so a delete that
	// commits after this read and before the provider receives the bytes is
	// not stopped — the window is one statement plus the request, per job.
	if live, err := r.store.ItemEvaluable(item.ID); err != nil {
		return false, err
	} else if !live {
		return false, ErrItemGone
	}
	answers, usage, err := r.provider.Ask(ctx, st.Bytes, qs.Questions)
	if err != nil {
		return true, err
	}
	rows := make([]models.ItemDecision, 0, len(answers))
	for _, key := range keys {
		a, ok := answers[key]
		if !ok {
			// The provider contract is one answer per question; a missing
			// one is a provider defect, and storing a partial set would make
			// the idempotency check re-ask forever.
			return true, fmt.Errorf("decision: provider returned no answer for %q", key)
		}
		raw, err := json.Marshal(storedAnswerOf(a))
		if err != nil {
			return true, err
		}
		rows = append(rows, models.ItemDecision{
			ItemID:         item.ID,
			QuestionSet:    qs.Name,
			QuestionKey:    key,
			Kind:           string(a.Kind),
			Answer:         raw,
			Confidence:     a.Confidence,
			Provider:       r.provider.Name(),
			Model:          r.provider.Model(),
			StateHash:      st.Hash,
			ItemSeq:        item.Seq,
			StateTruncated: st.Truncated || usage.StateTruncated,
		})
	}
	return true, r.store.InsertItemDecisions(item.WorkspaceID, rows)
}

// storedAnswer is the persisted shape of an [Answer]: the provider's wire
// vocabulary, with only the members the kind carries.
type storedAnswer struct {
	Type          Kind               `json:"type"`
	Choice        *string            `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Noul          *float64           `json:"noul,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

func storedAnswerOf(a Answer) storedAnswer {
	out := storedAnswer{Type: a.Kind, Legend: a.Legend, Probabilities: a.Probabilities, Confidence: a.Confidence}
	switch a.Kind {
	case KindChoice:
		c := a.Choice
		out.Choice = &c
	case KindScore:
		sc := a.Score
		out.Score = &sc
	case KindNoul:
		n := a.Noul
		out.Noul = &n
	}
	return out
}

// Decisions returns the item's latest answer per question, each marked
// Current when it was computed from the item's present state. A nil runner
// returns an empty list: no provider, no decisions, no error.
func (r *Runner) Decisions(itemID string) ([]models.ItemDecision, error) {
	if r == nil {
		return []models.ItemDecision{}, nil
	}
	rows, err := r.store.LatestItemDecisions(itemID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []models.ItemDecision{}, nil
	}
	_, st, err := r.State(itemID)
	if err != nil {
		if errors.Is(err, ErrItemGone) {
			return []models.ItemDecision{}, nil
		}
		return nil, err
	}
	for i := range rows {
		rows[i].Current = rows[i].StateHash == st.Hash
	}
	return rows, nil
}

// Job-handling bounds.
const (
	// maxDecisionAttempts: a job failing this many times in a row is dropped.
	// The next write to the item re-enqueues it, so dropping loses nothing a
	// user did — it stops a permanently-refused state (an over-budget item)
	// from costing a call every backoff forever.
	maxDecisionAttempts = 5
	// decisionRetryBase is the first backoff; it doubles per attempt.
	decisionRetryBase = 30 * time.Second
)

// RunOnce evaluates up to limit owed jobs, CLAIMING ONE AT A TIME, and
// returns how many it claimed.
//
// One at a time so that each job's lease starts when its own evaluation does
// (codex round 4). Claiming the whole pass up front made the last job wait
// behind every earlier provider call: at the provider's worst case (4 attempts
// x 60s plus up to 30s between them, ~5.5 min per job) twenty jobs is far past
// any lease, and an expired claim is taken by another instance — a duplicate
// evaluation. A single job's worst case fits inside the lease; see
// defaultDecisionClaimLease in the server.
func (r *Runner) RunOnce(ctx context.Context, runnerID string, limit int, lease time.Duration) (int, error) {
	if r == nil {
		return 0, nil
	}
	n := 0
	for n < limit {
		if ctx.Err() != nil {
			break // stopping: claim nothing further
		}
		jobs, err := r.store.ClaimDecisionJobs(runnerID, 1, lease)
		if err != nil {
			return n, err
		}
		if len(jobs) == 0 {
			break
		}
		n++
		r.runJob(ctx, jobs[0])
	}
	return n, nil
}

func (r *Runner) runJob(ctx context.Context, j store.DecisionJob) {
	_, err := r.Evaluate(ctx, j.ItemID, j.QuestionSet)
	var serr error
	switch {
	case err != nil && ctx.Err() != nil:
		// Cancelled by shutdown, not a verdict on the job: no attempt counted.
		r.release(j)
		return
	case err == nil:
		serr = r.store.CompleteDecisionJob(j)
	case errors.Is(err, ErrItemGone), errors.Is(err, ErrUnknownSet), errors.Is(err, ErrSetNotApplicable):
		serr = r.store.DropDecisionJob(j)
	case j.Attempts+1 >= maxDecisionAttempts:
		slog.Warn("decision job dropped after repeated failures",
			"item_id", j.ItemID, "question_set", j.QuestionSet, "attempts", j.Attempts+1, "error", err)
		serr = r.store.DropDecisionJob(j)
	default:
		slog.Info("decision job failed, will retry",
			"item_id", j.ItemID, "question_set", j.QuestionSet, "attempt", j.Attempts+1, "error", err)
		serr = r.store.FailDecisionJob(j, err, decisionRetryBase<<j.Attempts)
	}
	if serr != nil {
		// The claim's lease still bounds this: an unrecorded outcome leaves
		// the job claimed until the lease lapses, and then it is re-run.
		slog.Error("decision job outcome not recorded", "item_id", j.ItemID, "question_set", j.QuestionSet, "error", serr)
	}
}

func (r *Runner) release(j store.DecisionJob) {
	if err := r.store.ReleaseDecisionJob(j); err != nil {
		slog.Error("decision job not released", "item_id", j.ItemID, "question_set", j.QuestionSet, "error", err)
	}
}
