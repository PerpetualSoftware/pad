package decision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

	// Eligible, when set, narrows the set to items whose CURRENT state it
	// accepts. It is checked by the runner at evaluation time, never by the
	// store's resolver: the resolver runs inside the write transaction and
	// sees only a collection slug, while a predicate like "not terminal"
	// needs the item's fields and its collection's schema. An ineligible
	// item's job is dropped without a provider call; the next write
	// re-enqueues it, so an item reopened later is evaluated then.
	Eligible func(item *models.Item, coll *models.Collection) bool

	// Resolve, when set, supplies the questions per WORKSPACE at evaluation
	// time, in place of Questions (TASK-3119: one question per active
	// convention, which change when a convention is edited or switched off).
	// It is called on every evaluation and every currency check, so an
	// answer to a question no longer asked is never current. An empty map
	// means nothing to ask: the job completes with no provider call.
	Resolve func(ctx context.Context, s *store.Store, workspaceID string) (map[string]Question, error)

	// WithLinks adds the item's parent and blocking links to the state
	// (TASK-3119: a convention whose subject is a relation cannot be judged
	// without them). Only a set that asks for it gets them: the shared
	// builder's bytes, and so every other set's state hashes, are unchanged.
	WithLinks bool

	// NoTrail leaves the comment trail out of the state (TASK-3119 U2a). The
	// conventions set asks about the ITEM: a trail is discussion, it was
	// clipped at 2,000 runes per comment (so a break past that was invisible
	// anyway), and it was most of every call's input. Comments are asked about
	// one at a time instead (U2b). The U2 gates measured this state against
	// U0's labels: precision held at 100%, recall rose on two conventions, and
	// no state was truncated.
	NoTrail bool

	// MaxPerCall bounds the questions sent in one provider call; a larger
	// set is split into several calls. Zero sends them all in one.
	MaxPerCall int
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

// Registry holds the registered question sets. Production builds its registry
// with ProductionRegistry (attention.go); tests register their own.
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
	if len(qs.Questions) == 0 && qs.Resolve == nil {
		return fmt.Errorf("decision: question set %q has no questions", qs.Name)
	}
	if len(qs.Questions) > 0 && qs.Resolve != nil {
		return fmt.Errorf("decision: question set %q has both Questions and Resolve", qs.Name)
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
	// Links is present only for a set built WithLinks (TASK-3119), and then
	// always, as [] when the item has none: "this item has no parent" is
	// exactly what a relation convention needs to read. A pointer so nil
	// is omitted and every other set's bytes stay as they were.
	Links *[]ItemStateLink `json:"links,omitempty"`
}

// ItemStateLink is one of the item's parent or blocking links, as the item
// sees it: relation is "parent", "blocks" or "blocked_by".
type ItemStateLink struct {
	Relation string `json:"relation"`
	Ref      string `json:"ref"`
	Title    string `json:"title"`
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
	return buildItemState(item, comments, nil)
}

// BuildItemStateWithLinks is BuildItemState plus the item's links, for a set
// built WithLinks. links may be empty but not nil: the member is then [].
func BuildItemStateWithLinks(item *models.Item, comments []models.Comment, links []ItemStateLink) (BuiltState, error) {
	if links == nil {
		links = []ItemStateLink{}
	}
	return buildItemState(item, comments, &links)
}

func buildItemState(item *models.Item, comments []models.Comment, links *[]ItemStateLink) (BuiltState, error) {
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
		Links:      links,
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

// QuestionFingerprint is hex sha256 of the model and one question's full
// definition. An answer is current only while its fingerprint still matches:
// rewording the instructions, changing a criterion, or re-pinning the model
// changes what was asked, and the old answer is not an answer to the new
// question (codex round 6).
//
// Marshalled from a fixed struct, so field order is the struct's and map keys
// (Choice options) are sorted by encoding/json: one question has one print.
func QuestionFingerprint(model string, q Question) string {
	b, _ := json.Marshal(struct {
		Model        string            `json:"model"`
		Kind         Kind              `json:"kind"`
		Instructions string            `json:"instructions"`
		Options      map[string]string `json:"options,omitempty"`
		Levels       []string          `json:"levels,omitempty"`
		TrueDesc     string            `json:"true_desc,omitempty"`
		FalseDesc    string            `json:"false_desc,omitempty"`
	}{model, q.Kind, q.Instructions, q.Options, q.Levels, q.TrueDesc, q.FalseDesc})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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

	// usage, when set, is told what every provider call consumed, by set
	// (TASK-3119: the per-write spend must be visible before any Cloud
	// rollout). The server wires it to its metrics.
	usage func(set string, u Usage)

	// logger receives the per-evaluation spend line; nil means slog.Default().
	// A field rather than the global so a test can read its own lines.
	logger *slog.Logger
}

// SetUsageObserver installs fn, called after every provider call with the
// set's name and the call's usage. nil removes it. Not safe to call while
// jobs run; the server sets it once at startup.
func (r *Runner) SetUsageObserver(fn func(set string, u Usage)) {
	if r != nil {
		r.usage = fn
	}
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

// Provider returns the runner's provider (nil for a nil runner). For a
// non-nil runner this is never nil either — [NewRunner] returns nil
// whenever its Provider argument is nil, so the two are never out of sync.
//
// Exposed for a caller that needs a plain synchronous Ask — a single
// question answered against caller-supplied state, with no enqueue, no
// idempotency check, and nothing stored (PLAN-3114 unit 5, TASK-3120: the
// playbook-match endpoint). [Runner.Evaluate] is the wrong tool for that:
// it drives the owed-jobs pipeline (registry lookup by name, state built
// from an ITEM's row, a stored idempotency check, and a written
// models.ItemDecision row), all of which a one-off Choice over free text has
// no use for.
func (r *Runner) Provider() Provider {
	if r == nil {
		return nil
	}
	return r.provider
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

// ErrWorkspaceDeleted reports that the item's workspace is soft-deleted. The
// job is HELD, not dropped: a restore must find the work still owed.
var ErrWorkspaceDeleted = errors.New("decision: item's workspace is soft-deleted")

// ErrUnknownSet reports a job for a set no longer registered: dropped.
var ErrUnknownSet = errors.New("decision: question set is not registered")

// ErrSetNotApplicable reports a job for a set that does not cover the item's
// CURRENT collection — the item moved after the job was owed: dropped.
var ErrSetNotApplicable = errors.New("decision: question set does not apply to the item's collection")

// State builds the item's current state, as a set without links sees it.
func (r *Runner) State(itemID string) (*models.Item, BuiltState, error) {
	return r.stateFor(itemID, QuestionSet{})
}

// stateFor builds the item's current state as qs sees it: with its links
// when qs is WithLinks, and without the comment trail when qs is NoTrail.
func (r *Runner) stateFor(itemID string, qs QuestionSet) (*models.Item, BuiltState, error) {
	item, err := r.store.GetItem(itemID)
	if err != nil {
		return nil, BuiltState{}, fmt.Errorf("decision: read item %s: %w", itemID, err)
	}
	if item == nil || item.DeletedAt != nil {
		return nil, BuiltState{}, ErrItemGone
	}
	var comments []models.Comment
	if !qs.NoTrail {
		comments, err = r.store.RecentComments(itemID, RecentTrailWindow)
		if err != nil {
			return nil, BuiltState{}, err
		}
	}
	if !qs.WithLinks {
		st, err := BuildItemState(item, comments)
		return item, st, err
	}
	links, err := r.stateLinks(itemID)
	if err != nil {
		return nil, BuiltState{}, err
	}
	st, err := BuildItemStateWithLinks(item, comments, links)
	return item, st, err
}

// stateLinks is the item's parent and blocking links as the item sees them,
// in a fixed order so the hash moves only when the links do.
func (r *Runner) stateLinks(itemID string) ([]ItemStateLink, error) {
	rows, err := r.store.GetItemLinks(itemID)
	if err != nil {
		return nil, fmt.Errorf("decision: read links of %s: %w", itemID, err)
	}
	out := []ItemStateLink{}
	for _, l := range rows {
		switch {
		case l.LinkType == models.ItemLinkTypeParent && l.SourceID == itemID:
			out = append(out, ItemStateLink{Relation: "parent", Ref: l.TargetRef, Title: l.TargetTitle})
		case l.LinkType == models.ItemLinkTypeBlocks && l.SourceID == itemID:
			out = append(out, ItemStateLink{Relation: "blocks", Ref: l.TargetRef, Title: l.TargetTitle})
		case l.LinkType == models.ItemLinkTypeBlocks && l.TargetID == itemID:
			out = append(out, ItemStateLink{Relation: "blocked_by", Ref: l.SourceRef, Title: l.SourceTitle})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Relation != out[j].Relation {
			return out[i].Relation < out[j].Relation
		}
		if out[i].Ref != out[j].Ref {
			return out[i].Ref < out[j].Ref
		}
		return out[i].Title < out[j].Title
	})
	return out, nil
}

// questionsFor is the set's questions for the item's workspace now: its
// fixed Questions, or what Resolve returns. Each resolved question is
// validated, so a malformed one fails the evaluation instead of being sent.
func (r *Runner) questionsFor(ctx context.Context, qs QuestionSet, workspaceID string) (map[string]Question, error) {
	if qs.Resolve == nil {
		return qs.Questions, nil
	}
	qmap, err := qs.Resolve(ctx, r.store, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("decision: resolve set %q: %w", qs.Name, err)
	}
	for k, q := range qmap {
		if k == "" {
			return nil, fmt.Errorf("decision: set %q resolved an empty question key", qs.Name)
		}
		if err := q.Validate(); err != nil {
			return nil, fmt.Errorf("decision: set %q, question %q: %w", qs.Name, k, err)
		}
	}
	return qmap, nil
}

// Evaluate asks the named set about the item, unless answers for its current
// state are already stored. It reports whether the provider was called.
func (r *Runner) Evaluate(ctx context.Context, itemID, setName string) (called bool, err error) {
	if r == nil {
		return false, nil
	}
	qs, ok := r.registry.Get(setName)
	if !ok {
		return false, ErrUnknownSet
	}
	item, st, err := r.stateFor(itemID, qs)
	if err != nil {
		return false, err
	}
	// Applicability is re-checked HERE, against the collection the item is in
	// now, not trusted from enqueue time: a job owed before a move still names
	// the old collection's set (codex round 2). Checked before the idempotency
	// read so an inapplicable set never costs a call or a query.
	if ok, err := r.appliesNow(qs, item); err != nil {
		return false, err
	} else if !ok {
		return false, ErrSetNotApplicable
	}
	questions, err := r.questionsFor(ctx, qs, item.WorkspaceID)
	if err != nil {
		return false, err
	}
	if len(questions) == 0 {
		// Nothing to ask in this workspace now (no convention in scope):
		// the job is done, with no call.
		return false, nil
	}
	keys := make([]string, 0, len(questions))
	qhash := make(map[string]string, len(questions))
	for k, q := range questions {
		keys = append(keys, k)
		qhash[k] = QuestionFingerprint(r.provider.Model(), q)
	}
	sort.Strings(keys)
	have, err := r.store.HasItemDecisionsAtState(item.ID, qs.Name, st.Hash, qhash)
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
	//
	// This is the ONE liveness guard on the send path. A second, earlier copy
	// in State was redundant with it (mutant M26 survived its removal), so it
	// is gone; State serves the read path too, where nothing is sent. It runs
	// inside the loop below, immediately before each call.
	//
	// One call, or several of at most MaxPerCall questions each (in key
	// order, so a set is always split the same way). Rows are written only
	// once every call has answered: a partial set would fail the idempotency
	// check and be re-asked in full anyway.
	answers := make(map[string]Answer, len(keys))
	var usage Usage
	calls := 0
	// One line per evaluation that reached the provider, whatever came of it,
	// so the spend is readable from the server log on a box whose /metrics is
	// out of reach (TASK-3119).
	defer func() {
		if calls > 0 {
			r.logSpend(qs.Name, item, len(keys), calls, usage, st.Truncated || usage.StateTruncated, err)
		}
	}()
	for _, chunk := range chunkKeys(keys, qs.MaxPerCall) {
		cq := make(map[string]Question, len(chunk))
		for _, k := range chunk {
			cq[k] = questions[k]
		}
		// Liveness as the LAST statement before EACH send (codex r1 on
		// TASK-3119): a split set makes several calls, and a delete landing
		// between them must stop the rest. A call already made is reported
		// as made.
		itemLive, wsLive, err := r.store.ItemLiveness(item.ID)
		if err != nil {
			return calls > 0, err
		}
		if !itemLive {
			return calls > 0, ErrItemGone
		}
		if !wsLive {
			return calls > 0, ErrWorkspaceDeleted
		}
		got, u, err := r.provider.Ask(ctx, st.Bytes, cq)
		calls++
		if r.usage != nil {
			r.usage(qs.Name, u)
		}
		usage.InputTokens += u.InputTokens
		usage.OutputTokens += u.OutputTokens
		usage.StateTruncated = usage.StateTruncated || u.StateTruncated
		if err != nil {
			return true, err
		}
		for k, a := range got {
			answers[k] = a
		}
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
			QuestionHash:   qhash[key],
			ItemSeq:        item.Seq,
			StateTruncated: st.Truncated || usage.StateTruncated,
		})
	}
	return true, r.store.InsertItemDecisions(item.WorkspaceID, rows)
}

// logSpend writes the evaluation's spend line: counts and identifiers only,
// never a question, an answer, the item's text or an error message (a
// provider error can quote the request).
func (r *Runner) logSpend(set string, item *models.Item, questions, calls int, u Usage, truncated bool, err error) {
	l := r.logger
	if l == nil {
		l = slog.Default()
	}
	outcome := "stored"
	if err != nil {
		outcome = "failed"
	}
	l.Info("decision evaluation",
		"set", set,
		"workspace_id", item.WorkspaceID,
		"item_ref", item.Ref,
		"item_id", item.ID,
		"questions", questions,
		"calls", calls,
		"input_tokens", u.InputTokens,
		"output_tokens", u.OutputTokens,
		"truncated", truncated,
		"outcome", outcome)
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
	item, _, err := r.State(itemID)
	if err != nil {
		if errors.Is(err, ErrItemGone) {
			return []models.ItemDecision{}, nil
		}
		return nil, err
	}
	// Each set is judged against ITS state (with links or not) and ITS
	// questions as they stand now (a resolved set re-resolves), built once
	// per set.
	type setNow struct {
		st        BuiltState
		questions map[string]Question
		ok        bool
	}
	nows := map[string]*setNow{}
	nowFor := func(name string) (*setNow, error) {
		if n, seen := nows[name]; seen {
			return n, nil
		}
		n := &setNow{}
		nows[name] = n
		qs, registered := r.registry.Get(name)
		if !registered {
			return n, nil
		}
		_, st, err := r.stateFor(itemID, qs)
		if err != nil {
			return nil, err
		}
		qm, err := r.questionsFor(context.Background(), qs, item.WorkspaceID)
		if err != nil {
			return nil, err
		}
		n.st, n.questions, n.ok = st, qm, true
		return n, nil
	}
	model := r.provider.Model()
	applies := map[string]bool{}
	for i := range rows {
		// Current needs all of these to still hold: the item state, the
		// question as registered now under the model pinned now, and the set
		// still applying to the item. A set or key no longer registered has
		// no "now" to match, so it is not current. The last is not implied by
		// the state hash: a collection schema edit can make an item terminal,
		// and so ineligible, without changing a byte of its state (codex
		// round 2 on TASK-3118).
		now, err := nowFor(rows[i].QuestionSet)
		if err != nil {
			return nil, err
		}
		qhashNow := ""
		if now.ok {
			if q, ok := now.questions[rows[i].QuestionKey]; ok {
				qhashNow = QuestionFingerprint(model, q)
			}
		}
		if !(now.ok && rows[i].StateHash == now.st.Hash && qhashNow != "" && rows[i].QuestionHash == qhashNow) {
			continue
		}
		qs, _ := r.registry.Get(rows[i].QuestionSet)
		ok, seen := applies[qs.Name]
		if !seen {
			ok, err = r.appliesNow(qs, item)
			if err != nil {
				return nil, err
			}
			applies[qs.Name] = ok
		}
		rows[i].Current = ok
	}
	return rows, nil
}

// appliesNow reports whether the set would be asked about the item as it
// stands: its collection is covered and, when the set has one, its Eligible
// predicate accepts the item. Evaluate refuses to ASK on the same rule, and
// the read paths refuse to call an answer current on it, so an answer is never
// presented for an item the set would not ask about today.
func (r *Runner) appliesNow(qs QuestionSet, item *models.Item) (bool, error) {
	if !qs.appliesTo(item.CollectionSlug) {
		return false, nil
	}
	if qs.Eligible == nil {
		return true, nil
	}
	coll, err := r.store.GetCollection(item.CollectionID)
	if err != nil {
		return false, fmt.Errorf("decision: read collection %s: %w", item.CollectionID, err)
	}
	return coll != nil && qs.Eligible(item, coll), nil
}

// Job-handling bounds.
const (
	// maxDecisionAttempts: a job PERMANENTLY refused this many times in a row
	// is dropped. The next write to the item re-enqueues it, so dropping loses
	// nothing a user did — it stops a refused state (an over-budget item, a
	// malformed request) from costing a call every backoff forever. A
	// TRANSIENT failure is never dropped; see isPermanentFailure.
	maxDecisionAttempts = 5
	// decisionRetryBase is the first backoff; it doubles per attempt...
	decisionRetryBase = 30 * time.Second
	// ...up to decisionRetryCap, which is what a provider outage costs: one
	// call per owed job per hour until the provider is back.
	decisionRetryCap = time.Hour
)

// isPermanentFailure reports whether a failed evaluation says something about
// the REQUEST rather than about the provider's availability (codex round 6).
//
// Permanent: the provider answered 4xx other than 429 — the request itself is
// the problem, and asking again unchanged gets the same answer. Everything else
// is transient: 5xx, 429/529 after the provider's own retries, transport
// errors. A transient failure must never drop the job, or an outage would
// silently discard every evaluation owed while it lasted.
func isPermanentFailure(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 && apiErr.StatusCode != http.StatusTooManyRequests
	}
	return false
}

func decisionBackoff(attempts int) time.Duration {
	d := decisionRetryBase
	for i := 0; i < attempts && d < decisionRetryCap; i++ {
		d *= 2
	}
	if d > decisionRetryCap {
		d = decisionRetryCap
	}
	return d
}

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
		// Cancelled by shutdown, or by the provider being swapped or disabled
		// mid-pass (TASK-3121) — not a verdict on the job: no attempt counted.
		r.release(j)
		return
	case errors.Is(err, ErrWorkspaceDeleted):
		// Held for a restore, not a failure: released with no attempt, and
		// the claim scan will not offer it again while the workspace stays
		// deleted.
		r.release(j)
		return
	case err == nil:
		serr = r.store.CompleteDecisionJob(j)
	case errors.Is(err, ErrItemGone), errors.Is(err, ErrUnknownSet), errors.Is(err, ErrSetNotApplicable):
		serr = r.store.DropDecisionJob(j)
	case isPermanentFailure(err) && j.Attempts+1 >= maxDecisionAttempts:
		slog.Warn("decision job dropped after repeated refusals",
			"item_id", j.ItemID, "question_set", j.QuestionSet, "attempts", j.Attempts+1, "error", err)
		serr = r.store.DropDecisionJob(j)
	default:
		slog.Info("decision job failed, will retry",
			"item_id", j.ItemID, "question_set", j.QuestionSet, "attempt", j.Attempts+1,
			"permanent", isPermanentFailure(err), "error", err)
		serr = r.store.FailDecisionJob(j, err, decisionBackoff(j.Attempts))
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

// chunkKeys splits keys into runs of at most n; n <= 0 is one run.
func chunkKeys(keys []string, n int) [][]string {
	if n <= 0 || len(keys) <= n {
		return [][]string{keys}
	}
	var out [][]string
	for len(keys) > n {
		out = append(out, keys[:n])
		keys = keys[n:]
	}
	return append(out, keys)
}
