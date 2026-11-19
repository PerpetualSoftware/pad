package decision

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// Attention question set tests (TASK-3118).

// The eval's text, copied from eval2.py section D as a LITERAL — not read
// back through AttentionSet — so a rewording in attention.go fails here
// rather than being compared against itself.
func TestAttentionSet_QuestionsAreTheEvalsVerbatim(t *testing.T) {
	want := map[string]Question{
		"needs_human_decision": {
			Kind:         KindNoul,
			Instructions: "This work item is waiting on a judgment, approval, credential, or decision that only a human owner can supply, rather than on engineering work an AI agent could do unattended.",
			TrueDesc:     "the next step requires a human's decision, approval, sign-off, credentials, money, an external relationship, or hardware the agent cannot reach",
			FalseDesc:    "the next step is implementable by an engineer or AI agent from the description alone",
		},
		// The three staged questions (TASK-3921), also pinned as literals.
		"effort": {
			Kind:         KindScore,
			Instructions: "How much effort does it take the human owner to supply what this work item is waiting on, or to do its next step?",
			Levels: []string{
				"a yes/no or one-line answer",
				"a short reply, or minutes at a computer",
				"up to an hour",
				"research, or hours of work",
			},
		},
		"effort_medium": {
			Kind:         KindChoice,
			Instructions: "Where does the human owner have to be to supply what this work item is waiting on?",
			Options: map[string]string{
				"desk":         "at a computer: writing, reading, a review or a setting",
				"device":       "at a particular device, account or piece of hardware, such as a phone, a test device or a console",
				"other_person": "it needs another person's answer or action",
				"unclear":      "the text does not say",
			},
		},
		"high_stakes": {
			Kind:         KindNoul,
			Instructions: "Getting this work item wrong would be costly: it touches money, security, credentials, legal matters or a release date.",
			TrueDesc:     "the work involves payments, secrets or access, a legal or compliance obligation, or a date someone is counting on",
			FalseDesc:    "an ordinary change that is easy to correct afterwards",
		},
	}
	set := AttentionSet()
	got := set.Questions
	for key, w := range want {
		g, ok := got[key]
		if !ok {
			t.Errorf("question %q missing from the attention set", key)
			continue
		}
		if g.Kind != w.Kind || g.Instructions != w.Instructions || g.TrueDesc != w.TrueDesc || g.FalseDesc != w.FalseDesc ||
			!reflect.DeepEqual(g.Levels, w.Levels) || !reflect.DeepEqual(g.Options, w.Options) {
			t.Errorf("question %q differs from its pinned text:\n got %+v\nwant %+v", key, g, w)
		}
	}
	// TASK-3915 / TASK-3924 retired the other two: the graph owns blocked,
	// and waiting_on_external flagged nothing.
	for _, retired := range []string{"blocked", "waiting_on_external"} {
		if _, ok := got[retired]; ok {
			t.Errorf("retired question %q is still asked", retired)
		}
	}
	if len(got) != 4 {
		t.Errorf("attention set has %d questions, want 4", len(got))
	}
	// Every key is unsurfaced (TASK-4643), and the set carries no threshold.
	if len(set.Unsurfaced) != 4 || !set.Unsurfaced["effort"] || !set.Unsurfaced["effort_medium"] || !set.Unsurfaced["high_stakes"] || !set.Unsurfaced["needs_human_decision"] {
		t.Errorf("Unsurfaced = %v, want all four keys", set.Unsurfaced)
	}
	if set.Threshold != 0 {
		t.Errorf("Threshold = %v, want none", set.Threshold)
	}
	if set.MaxPerCall != 0 {
		t.Errorf("MaxPerCall = %d; the set must go in one call", set.MaxPerCall)
	}
}

func TestProductionRegistryRegisters(t *testing.T) {
	reg, err := ProductionRegistry()
	if err != nil {
		t.Fatalf("ProductionRegistry: %v", err)
	}
	if _, ok := reg.Get(AttentionSetName); !ok {
		t.Fatal("attention set not registered")
	}
	if _, ok := reg.Get(ConventionsSetName); !ok {
		t.Fatal("conventions set not registered")
	}
	// TASK-3119 adds `conventions` beside `attention`. Its comment subject
	// (U2b) is defined but no longer shipped (TASK-3924).
	if sets := reg.SetsFor("tasks"); len(sets) != 2 || sets[0] != AttentionSetName || sets[1] != ConventionsSetName {
		t.Fatalf("SetsFor(tasks) = %v, want [attention conventions]", sets)
	}
	if _, ok := reg.Get(ConventionsCommentsSetName); ok {
		t.Fatal("the retired conventions_comments set is still registered")
	}
}

// attentionFixture runs the PRODUCTION registry against a fake provider
// whose Noul answers the test sets per question key.
type attentionFixture struct {
	s     *store.Store
	r     *Runner
	f     *fake
	ws    *models.Workspace
	tasks *models.Collection

	mu      sync.Mutex
	answers map[string]float64
}

const statusSchema = `{"fields":[{"key":"status","type":"select","options":["open","in-progress","done"],"terminal_options":["done"]}]}`

// surfacedAttentionThreshold is the threshold the attention set surfaced
// needs_human_decision at before TASK-4643.
const surfacedAttentionThreshold = 0.7

// surfacedAttentionSet is the attention set as it shipped before TASK-4643:
// needs_human_decision surfaced at 0.7. No production set surfaces it now, but
// SurfacedDecisions and ItemReadDecisions still filter on Unsurfaced and
// Threshold, and their mechanism tests run over this copy.
func surfacedAttentionSet() QuestionSet {
	qs := AttentionSet()
	delete(qs.Unsurfaced, AttentionNeedsHuman)
	qs.Threshold = surfacedAttentionThreshold
	return qs
}

// surfacedRegistry is the production registry with surfacedAttentionSet in
// place of the attention set.
func surfacedRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, qs := range []QuestionSet{surfacedAttentionSet(), ConventionsSet()} {
		if err := reg.Register(qs); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func newAttentionFixture(t *testing.T) *attentionFixture {
	t.Helper()
	reg, err := ProductionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return newAttentionFixtureWith(t, reg)
}

// newSurfacedAttentionFixture is the fixture over surfacedRegistry.
func newSurfacedAttentionFixture(t *testing.T) *attentionFixture {
	t.Helper()
	return newAttentionFixtureWith(t, surfacedRegistry(t))
}

func newAttentionFixtureWith(t *testing.T, reg *Registry) *attentionFixture {
	t.Helper()
	fx := &attentionFixture{answers: map[string]float64{
		AttentionNeedsHuman: 0.9,
	}}
	fx.s = storetest.NewSQLite(t)
	var err error
	fx.ws, err = fx.s.CreateWorkspace(models.WorkspaceCreate{Name: "Attention"})
	if err != nil {
		t.Fatal(err)
	}
	fx.tasks, err = fx.s.CreateCollection(fx.ws.ID, models.CollectionCreate{Name: "Tasks", Schema: statusSchema})
	if err != nil {
		t.Fatal(err)
	}
	p, f := newFake(t, func(f *fake, req wireRequest, raw []byte, w http.ResponseWriter) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		out := map[string]any{}
		for key, q := range req.Questions {
			// Answer each question as ITS kind (the set mixes all three since
			// TASK-3921); the provider refuses a mismatched answer.
			switch q.Type {
			case string(KindScore):
				out[key] = map[string]any{"type": "score", "score": 1, "confidence": 0.9,
					"probabilities": map[string]float64{"1": 1}}
			case string(KindChoice):
				out[key] = map[string]any{"type": "choice", "choice": "desk", "confidence": 0.9,
					"probabilities": map[string]float64{"desk": 1}}
			default:
				out[key] = map[string]any{"type": "noul", "noul": fx.answers[key]}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-1.13.0", "answers": out,
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 5},
		})
	})
	fx.f = f
	fx.r = NewRunner(fx.s, p, reg)
	fx.r.Install(fx.s)
	// These tests are about currency and eligibility, and re-ask on the next
	// tick; the re-ask debounce (TASK-3916) has its own tests, which this
	// would otherwise make every write-then-tick here wait an hour for.
	fx.s.SetDecisionDebounce(nil)
	return fx
}

// Install installs the re-ask window beside the resolver (TASK-3916):
// ReaskDebounce for the item-level sets the production registry ships, none
// for a set not registered, and a nil runner removes it.
func TestInstall_DebouncesTheItemLevelSets(t *testing.T) {
	fx := newAttentionFixture(t)
	fx.r.Install(fx.s) // the fixture turned it off; put the production wiring back
	it := fx.item(t, fx.tasks.ID, "open")
	fx.tick(t)
	if _, err := fx.s.CreateComment(fx.ws.ID, it.ID, "", models.CommentCreate{Author: "wren", Body: "more"}); err != nil {
		t.Fatal(err)
	}
	j, err := fx.s.GetDecisionJob(it.ID, AttentionSetName)
	if err != nil || j == nil || j.NotBefore == "" {
		t.Fatalf("attention re-ask is not debounced: %+v %v", j, err)
	}
	if d := fx.r.debounceFor(ConventionsSetName); d != ReaskDebounce {
		t.Errorf("conventions window = %v, want %v", d, ReaskDebounce)
	}
	if d := fx.r.debounceFor("not-registered"); d != 0 {
		t.Errorf("an unregistered set has window %v, want none", d)
	}
	per := ConventionsCommentsSet()
	reg := NewRegistry()
	if err := reg.Register(per); err != nil {
		t.Fatal(err)
	}
	if d := NewRunner(fx.s, fx.r.provider, reg).debounceFor(per.Name); d != 0 {
		t.Errorf("a PerComment set has window %v, want none", d)
	}

	var nilRunner *Runner
	nilRunner.Install(fx.s)
	other := fx.item(t, fx.tasks.ID, "open")
	if j, _ := fx.s.GetDecisionJob(other.ID, AttentionSetName); j != nil {
		t.Fatalf("a nil runner left the resolver installed: %+v", j)
	}
}

func (fx *attentionFixture) set(key string, p float64) {
	fx.mu.Lock()
	fx.answers[key] = p
	fx.mu.Unlock()
}

func (fx *attentionFixture) tick(t *testing.T) {
	t.Helper()
	if _, err := fx.r.RunOnce(context.Background(), "tick", 50, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func (fx *attentionFixture) item(t *testing.T, collID, status string) *models.Item {
	t.Helper()
	it, err := fx.s.CreateItem(fx.ws.ID, collID, models.ItemCreate{Title: "Pick a vendor", Fields: `{"status":"` + status + `"}`})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// A terminal item and a system-collection item are never sent; the open item
// beside them is (the leg that shows the fixture CAN ask). Reopening the
// terminal item makes it eligible again.
func TestAttention_OnlyOpenItemsInUserCollectionsAreAsked(t *testing.T) {
	fx := newAttentionFixture(t)
	sys, err := fx.s.CreateCollection(fx.ws.ID, models.CollectionCreate{Name: "Conventions", Schema: statusSchema, IsSystem: true})
	if err != nil {
		t.Fatal(err)
	}
	open := fx.item(t, fx.tasks.ID, "open")
	done := fx.item(t, fx.tasks.ID, "done")
	rule := fx.item(t, sys.ID, "open")
	for _, it := range []*models.Item{open, done, rule} {
		if j, _ := fx.s.GetDecisionJob(it.ID, AttentionSetName); j == nil {
			t.Fatalf("precondition: no job owed for %s — the gate would be untested", it.ID)
		}
	}
	fx.tick(t)
	if len(fx.f.requests) != 1 {
		t.Fatalf("provider called %d times, want 1 (the open item only)", len(fx.f.requests))
	}
	for _, it := range []*models.Item{done, rule} {
		if j, _ := fx.s.GetDecisionJob(it.ID, AttentionSetName); j != nil {
			t.Errorf("ineligible item %s kept its job: %+v", it.ID, j)
		}
	}
	for _, it := range []*models.Item{open, done, rule} {
		ds, err := fx.r.Decisions(it.ID)
		if err != nil {
			t.Fatal(err)
		}
		if want := it == open; (len(ds) > 0) != want {
			t.Errorf("item %s: %d answers stored, want any = %v (only the open item)", it.ID, len(ds), want)
		}
	}

	reopened := `{"status":"open"}`
	if _, err := fx.s.UpdateItem(done.ID, models.ItemUpdate{Fields: &reopened}); err != nil {
		t.Fatal(err)
	}
	fx.tick(t)
	if len(fx.f.requests) != 2 {
		t.Fatalf("a reopened item was not asked (provider calls = %d, want 2)", len(fx.f.requests))
	}
}

// TASK-4643: needs_human_decision is stored for re-measurement and surfaced
// nowhere. At 0.9 (above any threshold the set ever had), the item read and
// the item brief carry no attention key and count nothing; the raw Decisions
// still returns all four rows.
func TestAttentionSet_NeedsHumanIsUnsurfaced(t *testing.T) {
	fx := newAttentionFixture(t)
	fx.set(AttentionNeedsHuman, 0.9)
	it := fx.item(t, fx.tasks.ID, "open")
	fx.tick(t)

	raw, err := fx.r.Decisions(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != len(AttentionSet().Questions) {
		t.Fatalf("Decisions returned %d rows, want all %d attention answers", len(raw), len(AttentionSet().Questions))
	}
	read, err := fx.r.ItemReadDecisions(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range read {
		if d.QuestionSet == AttentionSetName {
			t.Errorf("the item read carries attention key %s", d.QuestionKey)
		}
	}
	got, below, stale, err := fx.r.SurfacedDecisions(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || below != 0 || stale != 0 {
		t.Fatalf("surfaced=%v below=%d stale=%d, want none, 0, 0", surfacedKeys(got), below, stale)
	}
	if !AttentionSet().Unsurfaced[AttentionNeedsHuman] {
		t.Error("needs_human_decision is not in the attention set's Unsurfaced map")
	}
}
