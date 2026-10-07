package decision

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-3119 U1a: the `conventions` set.

type convFixture struct {
	s     *store.Store
	r     *Runner
	f     *fake
	ws    *models.Workspace
	conv  *models.Collection
	tasks *models.Collection
	usage []string // set name per provider call, from the usage observer
}

func newConvFixture(t *testing.T) *convFixture {
	t.Helper()
	s := storetest.NewSQLite(t)
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Conv"})
	if err != nil {
		t.Fatal(err)
	}
	// The real conventions collection: trait, system flag, template schema.
	if err := s.SeedCollectionsFromTemplate(ws.ID, "blank"); err != nil {
		t.Fatal(err)
	}
	colls, err := s.ListTraitedCollections(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	tc := collections.FindByArtifactKind(colls, collections.BuiltinConvention)
	if tc == nil {
		t.Fatal("blank template seeded no conventions collection")
	}
	conv, err := s.GetCollection(tc.ID)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[{"key":"status","type":"text"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	p, f := newFake(t, noulChoiceHandler)
	reg := NewRegistry()
	if err := reg.Register(ConventionsSet()); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(s, p, reg)
	fx := &convFixture{s: s, r: r, f: f, ws: ws, conv: conv, tasks: tasks}
	r.SetUsageObserver(func(set string, _ Usage) { fx.usage = append(fx.usage, set) })
	return fx
}

func (fx *convFixture) convention(t *testing.T, title, fields, body string) *models.Item {
	t.Helper()
	it, err := fx.s.CreateItem(fx.ws.ID, fx.conv.ID, models.ItemCreate{Title: title, Fields: fields, Content: body})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (fx *convFixture) task(t *testing.T, title string) *models.Item {
	t.Helper()
	it, err := fx.s.CreateItem(fx.ws.ID, fx.tasks.ID, models.ItemCreate{Title: title, Fields: `{"status":"open"}`, Content: "body"})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func keysOf(m map[string]Question) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Only active `always` conventions whose check is on and whose body fits.
func TestConventions_ResolveScope(t *testing.T) {
	fx := newConvFixture(t)
	in := fx.convention(t, "In scope", `{"status":"active","trigger":"always"}`, "Use plain words.")
	explicitOn := fx.convention(t, "Explicit on", `{"status":"active","trigger":"always","decision_check":"on"}`, "Name the source.")
	fx.convention(t, "Draft", `{"status":"draft","trigger":"always"}`, "x")
	fx.convention(t, "Triggered", `{"status":"active","trigger":"on-commit"}`, "x")
	fx.convention(t, "Switched off", `{"status":"active","trigger":"always","decision_check":"off"}`, "x")
	fx.convention(t, "Too long", `{"status":"active","trigger":"always"}`, strings.Repeat("a", conventionMaxBodyBytes+1))

	q, err := resolveConventions(context.Background(), fx.s, fx.ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{ConventionKeyPrefix + explicitOn.Ref, ConventionKeyPrefix + in.Ref}
	sort.Strings(want)
	if got := keysOf(q); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("resolved %v, want %v", got, want)
	}
	if got := q[ConventionKeyPrefix+in.Ref].Instructions; got != "This content violates the following convention: Use plain words." {
		t.Fatalf("question wording = %q", got)
	}
}

// Seven conventions are asked in calls of at most five, every answer is
// stored, and the observer sees each call.
func TestConventions_SplitsCallsAndStoresEveryAnswer(t *testing.T) {
	fx := newConvFixture(t)
	for i := 0; i < 7; i++ {
		fx.convention(t, "Rule", `{"status":"active","trigger":"always"}`, "rule "+string(rune('a'+i)))
	}
	item := fx.task(t, "A task")
	called, err := fx.r.Evaluate(context.Background(), item.ID, ConventionsSetName)
	if err != nil || !called {
		t.Fatalf("evaluate: called=%v err=%v", called, err)
	}
	if len(fx.f.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(fx.f.requests))
	}
	sizes := []int{len(fx.f.requests[0].Questions), len(fx.f.requests[1].Questions)}
	sort.Ints(sizes)
	if sizes[0] != 2 || sizes[1] != 5 {
		t.Fatalf("questions per call = %v, want [2 5]", sizes)
	}
	if strings.Join(fx.usage, ",") != "conventions,conventions" {
		t.Fatalf("usage observer saw %v", fx.usage)
	}
	ds, err := fx.r.Decisions(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	current := 0
	for _, d := range ds {
		if d.QuestionSet == ConventionsSetName && d.Current {
			current++
		}
	}
	if current != 7 {
		t.Fatalf("current conventions answers = %d, want 7", current)
	}
}

// No convention in scope: nothing to ask, no call, not an error.
func TestConventions_NoneInScopeMakesNoCall(t *testing.T) {
	fx := newConvFixture(t)
	item := fx.task(t, "A task")
	called, err := fx.r.Evaluate(context.Background(), item.ID, ConventionsSetName)
	if err != nil || called || len(fx.f.requests) != 0 {
		t.Fatalf("called=%v err=%v requests=%d, want no call", called, err, len(fx.f.requests))
	}
}

// A system-collection item (a convention itself) is never asked about.
func TestConventions_SystemCollectionNotAsked(t *testing.T) {
	fx := newConvFixture(t)
	c := fx.convention(t, "Rule", `{"status":"active","trigger":"always"}`, "rule")
	_, err := fx.r.Evaluate(context.Background(), c.ID, ConventionsSetName)
	if !errors.Is(err, ErrSetNotApplicable) || len(fx.f.requests) != 0 {
		t.Fatalf("err=%v requests=%d, want ErrSetNotApplicable and no call", err, len(fx.f.requests))
	}
}

// The state carries links, [] when there are none.
func TestConventions_StateCarriesLinks(t *testing.T) {
	fx := newConvFixture(t)
	fx.convention(t, "Rule", `{"status":"active","trigger":"always"}`, "rule")
	parent := fx.task(t, "The parent")
	child := fx.task(t, "The child")
	if _, err := fx.s.CreateItemLink(fx.ws.ID, models.ItemLinkCreate{TargetID: parent.ID, LinkType: models.ItemLinkTypeParent}, child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.r.Evaluate(context.Background(), child.ID, ConventionsSetName); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.r.Evaluate(context.Background(), parent.ID, ConventionsSetName); err != nil {
		t.Fatal(err)
	}
	stateOf := func(i int) json.RawMessage {
		var body struct {
			State json.RawMessage `json:"state"`
		}
		if err := json.Unmarshal([]byte(fx.f.rawBodies[i]), &body); err != nil {
			t.Fatal(err)
		}
		return body.State
	}
	links := func(i int) []ItemStateLink {
		var st ItemState
		if err := json.Unmarshal(stateOf(i), &st); err != nil {
			t.Fatal(err)
		}
		if st.Links == nil {
			t.Fatalf("request %d state has no links member", i)
		}
		return *st.Links
	}
	if got := links(0); len(got) != 1 || got[0].Relation != "parent" || got[0].Title != "The parent" || got[0].Ref == "" {
		t.Fatalf("child links = %+v", got)
	}
	// The parent has no parent or blocking link: present, and empty.
	if got := links(1); len(got) != 0 {
		t.Fatalf("parent links = %+v, want []", got)
	}
	if !strings.Contains(string(stateOf(1)), `"links":[]`) {
		t.Fatalf(`state lacks "links":[]: %s`, stateOf(1))
	}
}

// Every other set's state is byte-for-byte what it was: the links member is
// omitted, and the hash equals the one main computed for the same item.
func TestConventions_SharedBuilderHashUnchanged(t *testing.T) {
	item := &models.Item{ID: "i1", Title: "Pin <me> &  ", CollectionSlug: "tasks", Fields: `{"status":"open","n":3.0}`, Content: "body text"}
	c := []models.Comment{{ID: "c1", Author: "Dave", Body: "a remark", CreatedAt: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}}
	st, err := BuildItemState(item, c)
	if err != nil {
		t.Fatal(err)
	}
	// Computed by this exact code on origin/main 6903eeee, before TASK-3119.
	const mainHash = "5566b5806f78c5cbfad912128b755af9dd34e74741fdde36c9c1ab04854b6879"
	if st.Hash != mainHash || strings.Contains(string(st.Bytes), "links") {
		t.Fatalf("shared builder changed: hash %s, bytes %s", st.Hash, st.Bytes)
	}
}

// An answer is current only while its convention is still asked and the
// state (links included) is unchanged.
func TestConventions_CurrencyFollowsTheConventionAndTheLinks(t *testing.T) {
	fx := newConvFixture(t)
	rule := fx.convention(t, "Rule", `{"status":"active","trigger":"always"}`, "rule")
	item := fx.task(t, "A task")
	if _, err := fx.r.Evaluate(context.Background(), item.ID, ConventionsSetName); err != nil {
		t.Fatal(err)
	}
	isCurrent := func() bool {
		ds, err := fx.r.Decisions(item.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range ds {
			if d.QuestionKey == ConventionKeyPrefix+rule.Ref {
				return d.Current
			}
		}
		t.Fatal("no answer for the rule")
		return false
	}
	if !isCurrent() {
		t.Fatal("fresh answer not current")
	}

	// A new link moves the state: not current until re-asked.
	other := fx.task(t, "Other")
	if _, err := fx.s.CreateItemLink(fx.ws.ID, models.ItemLinkCreate{TargetID: other.ID, LinkType: models.ItemLinkTypeBlocks}, item.ID); err != nil {
		t.Fatal(err)
	}
	if isCurrent() {
		t.Fatal("answer still current after a link changed the state")
	}
	if _, err := fx.r.Evaluate(context.Background(), item.ID, ConventionsSetName); err != nil {
		t.Fatal(err)
	}
	if !isCurrent() {
		t.Fatal("re-asked answer not current")
	}

	// The convention switched off: no longer asked, so not current.
	off := `{"status":"active","trigger":"always","decision_check":"off"}`
	if _, err := fx.s.UpdateItem(rule.ID, models.ItemUpdate{Fields: &off}); err != nil {
		t.Fatal(err)
	}
	if isCurrent() {
		t.Fatal("answer current after its convention was switched off")
	}
}

// Codex r1: liveness is checked before EACH call of a split set. An item
// deleted while the first call is in flight is not sent in the second.
func TestConventions_DeleteBetweenSplitCallsStopsTheRest(t *testing.T) {
	fx := newConvFixture(t)
	for i := 0; i < 7; i++ {
		fx.convention(t, "Rule", `{"status":"active","trigger":"always"}`, "rule "+string(rune('a'+i)))
	}
	item := fx.task(t, "A task")
	fx.r.beforeAsk = nil
	// Delete the item from inside the first provider call.
	deleted := false
	fx.f.onRequest = func() {
		if !deleted {
			deleted = true
			if err := fx.s.DeleteItem(item.ID); err != nil {
				t.Error(err)
			}
		}
	}
	called, err := fx.r.Evaluate(context.Background(), item.ID, ConventionsSetName)
	if !errors.Is(err, ErrItemGone) || !called {
		t.Fatalf("called=%v err=%v; want the first call made, then ErrItemGone", called, err)
	}
	if len(fx.f.requests) != 1 {
		t.Fatalf("provider calls = %d; the second chunk was sent after the delete", len(fx.f.requests))
	}
}
