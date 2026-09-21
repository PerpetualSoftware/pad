package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// Runner tests (TASK-3117). The provider is the REAL typesafe provider pointed
// at a fake server, so what these tests see is what went over the wire.

const triageSet = "triage"

func triageQuestions() map[string]Question {
	return map[string]Question{
		"urgent": Noul("Is this item urgent?", "", ""),
		"kind":   Choice("What kind of item is this?", map[string]string{"bug": "a defect", "chore": "maintenance"}),
	}
}

// noulChoiceHandler answers every question it is sent.
func noulChoiceHandler(f *fake, req wireRequest, raw []byte, w http.ResponseWriter) {
	answers := map[string]any{}
	for key, q := range req.Questions {
		switch q.Type {
		case string(KindNoul):
			answers[key] = map[string]any{"type": "noul", "noul": 0.8}
		case string(KindChoice):
			answers[key] = map[string]any{"type": "choice", "choice": "bug", "confidence": 0.9,
				"probabilities": map[string]float64{"bug": 0.9, "chore": 0.1}}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model": "jev-1.13.0", "answers": answers,
		"usage": map[string]int{"input_tokens": 10, "output_tokens": 5},
	})
}

type runnerFixture struct {
	s      *store.Store
	r      *Runner
	f      *fake
	ws     *models.Workspace
	col    *models.Collection
	item   *models.Item
	called func() int
}

func newRunnerFixture(t *testing.T) *runnerFixture {
	t.Helper()
	s := storetest.NewSQLite(t)
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Runner"})
	if err != nil {
		t.Fatal(err)
	}
	col, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[{"key":"status","type":"text"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	p, f := newFake(t, noulChoiceHandler)
	reg := NewRegistry()
	if err := reg.Register(QuestionSet{Name: triageSet, Questions: triageQuestions()}); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(s, p, reg)
	r.Install(s)
	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Login <b>fails</b> & retries", Content: "Steps: open \u2028 the page", Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &runnerFixture{s: s, r: r, f: f, ws: ws, col: col, item: item, called: func() int { return len(f.requests) }}
}

func (fx *runnerFixture) evaluate(t *testing.T) bool {
	t.Helper()
	called, err := fx.r.Evaluate(context.Background(), fx.item.ID, triageSet)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return called
}

// ONE SERIALISATION PATH: the state bytes on the wire are byte-identical to
// the bytes hashed. The fixture's title and body carry <, >, & and U+2028,
// which json.Marshal escapes — a second marshal with different escaping would
// be caught here.
func TestRunner_HashedBytesAreTheBytesSent(t *testing.T) {
	fx := newRunnerFixture(t)
	if !fx.evaluate(t) {
		t.Fatal("first evaluation made no provider call")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fx.f.rawBodies[0]), &envelope); err != nil {
		t.Fatal(err)
	}
	_, st, err := fx.r.State(fx.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(envelope["state"], st.Bytes) {
		t.Fatalf("state on the wire differs from the hashed bytes:\nwire:   %s\nhashed: %s", envelope["state"], st.Bytes)
	}
	if !bytes.Contains(st.Bytes, []byte(`\u003cb\u003e`)) || !bytes.Contains(st.Bytes, []byte(`\u2028`)) {
		t.Fatalf("precondition: the fixture must exercise escaping, got %s", st.Bytes)
	}
}

// Idempotency: an unchanged state makes no second call.
func TestRunner_UnchangedStateShortCircuits(t *testing.T) {
	fx := newRunnerFixture(t)
	fx.evaluate(t)
	if fx.evaluate(t) {
		t.Fatal("re-evaluating an unchanged state called the provider")
	}
	if n := fx.called(); n != 1 {
		t.Fatalf("provider called %d times; want 1", n)
	}
}

// A comment changes the state (the trail is hashed), so it re-evaluates —
// the case items.seq alone would have missed (ruling 2).
func TestRunner_CommentReevaluates(t *testing.T) {
	fx := newRunnerFixture(t)
	fx.evaluate(t)
	before, err := fx.s.GetItem(fx.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.s.CreateComment(fx.ws.ID, fx.item.ID, "", models.CommentCreate{Author: "kestrel", Body: "reproduced on staging"}); err != nil {
		t.Fatal(err)
	}
	after, err := fx.s.GetItem(fx.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The counterfactual: seq did NOT move, so a seq-keyed runner would have
	// short-circuited here.
	if after.Seq != before.Seq {
		t.Fatalf("precondition: a comment moved items.seq (%d -> %d); this test no longer discriminates", before.Seq, after.Seq)
	}
	if !fx.evaluate(t) {
		t.Fatal("a new comment did not re-evaluate")
	}
}

// Writes that change nothing the state carries move seq but must not cost a
// call: a role-board reorder, a parent-link add (the only link kinds that
// bump seq are parent and implements), an assignment-only update.
func TestRunner_StateInvisibleWritesDoNotReevaluate(t *testing.T) {
	fx := newRunnerFixture(t)
	other, err := fx.s.CreateItem(fx.ws.ID, fx.col.ID, models.ItemCreate{Title: "Parent", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	role, err := fx.s.CreateAgentRole(fx.ws.ID, models.AgentRoleCreate{Name: "Implementer"})
	if err != nil {
		t.Fatal(err)
	}
	fx.evaluate(t)

	writes := []struct {
		name string
		do   func() error
	}{
		{"role-board reorder", func() error {
			return fx.s.UpdateRoleSortOrder(fx.ws.ID, []store.RoleSortUpdate{{ItemID: fx.item.ID, RoleSortOrder: 7}})
		}},
		{"parent link add", func() error {
			_, err := fx.s.CreateItemLink(fx.ws.ID, models.ItemLinkCreate{TargetID: other.ID, LinkType: "parent"}, fx.item.ID)
			return err
		}},
		{"role assignment", func() error {
			_, err := fx.s.UpdateItem(fx.item.ID, models.ItemUpdate{AgentRoleID: &role.ID})
			return err
		}},
	}
	for _, w := range writes {
		before, _ := fx.s.GetItem(fx.item.ID)
		if err := w.do(); err != nil {
			t.Fatalf("%s: %v", w.name, err)
		}
		after, _ := fx.s.GetItem(fx.item.ID)
		if after.Seq == before.Seq {
			t.Fatalf("precondition: %s did not move items.seq; this leg no longer discriminates", w.name)
		}
		if fx.evaluate(t) {
			t.Fatalf("%s re-evaluated an unchanged state", w.name)
		}
	}
	if n := fx.called(); n != 1 {
		t.Fatalf("provider called %d times across three state-invisible writes; want 1", n)
	}
}

// The read marks a row current only while its state is the item's state.
func TestRunner_DecisionsReadMarksCurrent(t *testing.T) {
	fx := newRunnerFixture(t)
	fx.evaluate(t)
	got, err := fx.r.Decisions(fx.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d decisions; want 2", len(got))
	}
	for _, d := range got {
		if !d.Current {
			t.Fatalf("%s: freshly evaluated answer is not current", d.QuestionKey)
		}
	}
	if _, err := fx.s.CreateComment(fx.ws.ID, fx.item.ID, "", models.CommentCreate{Author: "a", Body: "new info"}); err != nil {
		t.Fatal(err)
	}
	got, err = fx.r.Decisions(fx.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range got {
		if d.Current {
			t.Fatalf("%s: answer computed before a comment still reads as current", d.QuestionKey)
		}
	}
}

// Nil provider: no runner, no resolver, no job rows, and an empty read.
func TestRunner_NilProviderIsANoOp(t *testing.T) {
	s := storetest.NewSQLite(t)
	r := NewRunner(s, nil, NewRegistry())
	if r != nil {
		t.Fatal("NewRunner with a nil provider returned a runner")
	}
	r.Install(s)
	ws, _ := s.CreateWorkspace(models.WorkspaceCreate{Name: "Off"})
	col, _ := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[]}`})
	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if j, err := s.GetDecisionJob(item.ID, triageSet); err != nil || j != nil {
		t.Fatalf("nil provider left a job row: %+v, %v", j, err)
	}
	got, err := r.Decisions(item.ID)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("nil runner read = %v, %v; want an empty non-nil list", got, err)
	}
	if n, err := r.RunOnce(context.Background(), "x", 10, time.Minute); n != 0 || err != nil {
		t.Fatalf("nil runner RunOnce = %d, %v", n, err)
	}
}

// End to end through the queue: a write enqueues, one tick evaluates and
// retires the job, and a second tick finds nothing.
func TestRunner_RunOnceDrainsTheQueue(t *testing.T) {
	fx := newRunnerFixture(t)
	n, err := fx.r.RunOnce(context.Background(), "tick", 10, time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("RunOnce claimed %d (err %v); want 1", n, err)
	}
	if fx.called() != 1 {
		t.Fatalf("provider called %d times; want 1", fx.called())
	}
	if j, _ := fx.s.GetDecisionJob(fx.item.ID, triageSet); j != nil {
		t.Fatalf("job still owed after a successful evaluation: %+v", j)
	}
	if n, _ := fx.r.RunOnce(context.Background(), "tick", 10, time.Minute); n != 0 {
		t.Fatalf("second tick claimed %d; want 0", n)
	}
}

// A failing provider leaves the job owed with the failure recorded, and a
// deleted item's job is dropped rather than retried.
func TestRunner_FailureAndGoneItem(t *testing.T) {
	fx := newRunnerFixture(t)
	fx.f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"bad request"}`, http.StatusBadRequest)
	})
	if _, err := fx.r.RunOnce(context.Background(), "tick", 10, time.Minute); err != nil {
		t.Fatal(err)
	}
	j, _ := fx.s.GetDecisionJob(fx.item.ID, triageSet)
	if j == nil || j.Attempts != 1 || j.LastError == "" {
		t.Fatalf("after a provider failure the job is %+v; want attempts=1 with an error", j)
	}

	if err := fx.s.DeleteItem(fx.item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.r.Evaluate(context.Background(), fx.item.ID, triageSet); !errors.Is(err, ErrItemGone) {
		t.Fatalf("evaluating a deleted item: err = %v; want ErrItemGone", err)
	}
}

func TestRegistry_RefusesDuplicatesAndInvalidSets(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(QuestionSet{Name: "a", Questions: triageQuestions()}); err != nil {
		t.Fatal(err)
	}
	for i, qs := range []QuestionSet{
		{Name: "a", Questions: triageQuestions()},
		{Name: "", Questions: triageQuestions()},
		{Name: "b"},
		{Name: "c", Questions: map[string]Question{"": Noul("x", "", "")}},
		{Name: "d", Questions: map[string]Question{"k": Choice("x", map[string]string{"only": "one"})}},
	} {
		if err := reg.Register(qs); err == nil {
			t.Errorf("case %d: Register(%+v) accepted an invalid set", i, qs)
		}
	}
	reg2 := NewRegistry()
	_ = reg2.Register(QuestionSet{Name: "scoped", Collections: []string{"bugs"}, Questions: triageQuestions()})
	_ = reg2.Register(QuestionSet{Name: "all", Questions: triageQuestions()})
	if got := fmt.Sprint(reg2.SetsFor("bugs")); got != "[all scoped]" {
		t.Fatalf("SetsFor(bugs) = %s", got)
	}
	if got := fmt.Sprint(reg2.SetsFor("tasks")); got != "[all]" {
		t.Fatalf("SetsFor(tasks) = %s", got)
	}
}

// A pass that starts already cancelled claims nothing, calls nothing, and
// leaves the job owed with no attempt counted.
func TestRunner_CancelledPassReleasesWithoutCountingAFailure(t *testing.T) {
	fx := newRunnerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := fx.r.RunOnce(ctx, "stopping", 10, time.Hour)
	if err != nil || n != 0 {
		t.Fatalf("an already-cancelled pass claimed %d (err %v); want 0", n, err)
	}
	if fx.called() != 0 {
		t.Fatalf("a cancelled pass called the provider %d times", fx.called())
	}
	j, _ := fx.s.GetDecisionJob(fx.item.ID, triageSet)
	if j == nil || j.Attempts != 0 || j.ClaimedBy != "" {
		t.Fatalf("after a cancelled pass the job is %+v; want owed, unclaimed, attempts=0", j)
	}
}

// Cancelled MID-CALL: the provider request is in flight when shutdown
// cancels the context. runJob must release the claim without counting an
// attempt — distinct from the test above, where cancellation lands before
// any job starts and runJob is never reached.
func TestRunner_CancelledMidCallReleasesWithoutCountingAFailure(t *testing.T) {
	fx := newRunnerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	fx.f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		// Bounded: the server notices a client disconnect only after the
		// body is read, so an unbounded wait here holds Close() forever.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	n, err := fx.r.RunOnce(ctx, "stopping", 10, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("RunOnce claimed %d (err %v); want 1", n, err)
	}
	j, _ := fx.s.GetDecisionJob(fx.item.ID, triageSet)
	if j == nil || j.Attempts != 0 || j.ClaimedBy != "" || j.LastError != "" {
		t.Fatalf("after a mid-call cancel the job is %+v; want owed, unclaimed, attempts=0, no error", j)
	}
}

// A soft-deleted WORKSPACE or COLLECTION stops evaluation even though the item
// row itself is live: nothing its owner deleted goes to the provider. A
// workspace's jobs are held for its restore; a collection's are dropped.
func TestRunner_DeletedWorkspaceOrCollectionIsNeverSent(t *testing.T) {
	for _, tc := range []struct {
		name string
		del  func(fx *runnerFixture) error
	}{
		{"workspace", func(fx *runnerFixture) error { return fx.s.DeleteWorkspace(fx.ws.Slug) }},
		{"collection", func(fx *runnerFixture) error { return fx.s.DeleteCollection(fx.col.ID, "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRunnerFixture(t)
			if err := tc.del(fx); err != nil {
				t.Fatalf("soft-delete %s: %v", tc.name, err)
			}
			if it, _ := fx.s.GetItem(fx.item.ID); it == nil {
				t.Fatalf("precondition: GetItem hides the item after a %s delete; this leg no longer discriminates", tc.name)
			}
			if _, err := fx.r.RunOnce(context.Background(), "tick", 10, time.Minute); err != nil {
				t.Fatal(err)
			}
			if fx.called() != 0 {
				t.Fatalf("an item in a soft-deleted %s was sent to the provider", tc.name)
			}
			j, _ := fx.s.GetDecisionJob(fx.item.ID, triageSet)
			switch tc.name {
			case "workspace":
				// HELD, not dropped: a workspace restore must find the work
				// still owed (codex round 5).
				if j == nil || j.ClaimedBy != "" || j.Attempts != 0 {
					t.Fatalf("job for an item in a soft-deleted workspace is %+v; want held (owed, unclaimed, attempts=0)", j)
				}
			case "collection":
				// A collection has no restore path, so its jobs are dropped.
				if j != nil {
					t.Fatalf("job for an item in a soft-deleted collection was kept: %+v", j)
				}
			}
		})
	}
}

// Codex round 2 P2: a job owed for a set scoped to the item's OLD collection
// must not reach the provider after the item moves out of that collection.
func TestRunner_SetNoLongerApplicableAfterMoveIsNotAsked(t *testing.T) {
	s := storetest.NewSQLite(t)
	ws, _ := s.CreateWorkspace(models.WorkspaceCreate{Name: "Scope"})
	bugs, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Bugs", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	ideas, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Ideas", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	p, f := newFake(t, noulChoiceHandler)
	reg := NewRegistry()
	if err := reg.Register(QuestionSet{Name: "bug-triage", Collections: []string{bugs.Slug}, Questions: triageQuestions()}); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(s, p, reg)
	r.Install(s)

	item, err := s.CreateItem(ws.ID, bugs.ID, models.ItemCreate{Title: "Crash on save"})
	if err != nil {
		t.Fatal(err)
	}
	if j, _ := s.GetDecisionJob(item.ID, "bug-triage"); j == nil {
		t.Fatal("precondition: no job owed for the scoped set")
	}
	if _, err := s.MoveItem(item.ID, ideas.ID, item.Fields); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunOnce(context.Background(), "tick", 10, time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 0 {
		t.Fatalf("asked a set scoped to %q about an item now in %q", bugs.Slug, ideas.Slug)
	}
	if j, _ := s.GetDecisionJob(item.ID, "bug-triage"); j != nil {
		t.Fatalf("an inapplicable job was kept: %+v", j)
	}
}

// Codex round 4: jobs are claimed ONE AT A TIME, so a job's lease starts when
// its own evaluation starts. Claiming a whole pass up front let the last job
// wait out its lease behind the others' provider calls and be re-claimed by
// another instance — a duplicate evaluation.
func TestRunner_ClaimsOneJobAtATime(t *testing.T) {
	fx := newRunnerFixture(t)
	for i := 0; i < 2; i++ {
		if _, err := fx.s.CreateItem(fx.ws.ID, fx.col.ID, models.ItemCreate{Title: fmt.Sprintf("more %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := fx.s.ListItems(fx.ws.ID, models.ItemListParams{})
	if err != nil || len(items) != 3 {
		t.Fatalf("precondition: %d items (%v)", len(items), err)
	}
	maxClaimed := 0
	fx.f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claimed := 0
		for _, it := range items {
			if j, _ := fx.s.GetDecisionJob(it.ID, triageSet); j != nil && j.ClaimedBy != "" {
				claimed++
			}
		}
		if claimed > maxClaimed {
			maxClaimed = claimed
		}
		var req wireRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		noulChoiceHandler(fx.f, req, nil, w)
	})
	n, err := fx.r.RunOnce(context.Background(), "tick", 10, time.Hour)
	if err != nil || n != 3 {
		t.Fatalf("RunOnce processed %d (err %v); want 3", n, err)
	}
	if maxClaimed != 1 {
		t.Fatalf("up to %d jobs were claimed while a provider call was in flight; want 1", maxClaimed)
	}
}

// Codex round 4: liveness is re-checked immediately before the provider call,
// so a delete landing between building the state and asking stops the send.
func TestRunner_DeleteJustBeforeAskIsNotSent(t *testing.T) {
	fx := newRunnerFixture(t)
	fx.r.beforeAsk = func() {
		if err := fx.s.DeleteWorkspace(fx.ws.Slug); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fx.r.Evaluate(context.Background(), fx.item.ID, triageSet); !errors.Is(err, ErrWorkspaceDeleted) {
		t.Fatalf("err = %v; want ErrWorkspaceDeleted", err)
	}
	if fx.called() != 0 {
		t.Fatal("content of a workspace deleted before the call was sent to the provider")
	}
}

// Codex round 5: work owed in a workspace that is soft-deleted and then
// RESTORED is evaluated after the restore — nothing enqueues on restore (it
// is a fan-out), so the job must have survived the deletion.
func TestRunner_WorkspaceRestoreResumesHeldWork(t *testing.T) {
	fx := newRunnerFixture(t)
	if err := fx.s.DeleteWorkspace(fx.ws.Slug); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // more than one pass: a held job must not be claimed, nor spin
		if n, err := fx.r.RunOnce(context.Background(), "tick", 10, time.Minute); err != nil || n != 0 {
			t.Fatalf("pass %d over a deleted workspace claimed %d (err %v); want 0", i, n, err)
		}
	}
	if err := fx.s.RestoreWorkspace(fx.ws.Slug); err != nil {
		t.Fatal(err)
	}
	if n, err := fx.r.RunOnce(context.Background(), "tick", 10, time.Minute); err != nil || n != 1 {
		t.Fatalf("after restore RunOnce claimed %d (err %v); want 1", n, err)
	}
	if fx.called() != 1 {
		t.Fatalf("provider called %d times after restore; want 1", fx.called())
	}
}

// Codex round 6: an answer is current only for the QUESTION and MODEL that
// produced it. Rewording a question, or changing the pinned model, at an
// unchanged item state must re-ask — and the old answer must stop reading as
// current.
func TestRunner_QuestionOrModelChangeReasks(t *testing.T) {
	fx := newRunnerFixture(t)
	fx.evaluate(t)

	reworded := NewRegistry()
	qs := triageQuestions()
	qs["urgent"] = Noul("Does this block a release?", "", "")
	if err := reworded.Register(QuestionSet{Name: triageSet, Questions: qs}); err != nil {
		t.Fatal(err)
	}
	p2 := newTypesafe("test-key", "jev-1.13.0")
	p2.setEndpoint(fx.f.srv.URL)
	r2 := NewRunner(fx.s, p2, reworded)
	got, err := r2.Decisions(fx.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range got {
		want := d.QuestionKey != "urgent"
		if d.Current != want {
			t.Fatalf("after rewording `urgent`, %s current=%v; want %v", d.QuestionKey, d.Current, want)
		}
	}
	if called, err := r2.Evaluate(context.Background(), fx.item.ID, triageSet); err != nil || !called {
		t.Fatalf("a reworded question at an unchanged state was not re-asked (called=%v err=%v)", called, err)
	}

	p3 := newTypesafe("test-key", "jev-9.9.9")
	p3.setEndpoint(fx.f.srv.URL)
	r3 := NewRunner(fx.s, p3, reworded)
	if called, err := r3.Evaluate(context.Background(), fx.item.ID, triageSet); err != nil || !called {
		t.Fatalf("a model change at an unchanged state was not re-asked (called=%v err=%v)", called, err)
	}
}

// Codex round 6: a provider OUTAGE must not lose owed work. Transient failures
// (5xx, 429, transport) back off indefinitely; only a permanent refusal (a 4xx
// naming the request itself) is dropped after repeated attempts.
func TestRunner_TransientFailuresAreNeverDropped(t *testing.T) {
	fx := newRunnerFixture(t)
	fx.f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"upstream down"}`, http.StatusServiceUnavailable)
	})
	for i := 0; i < maxDecisionAttempts+2; i++ {
		// Expire any backoff so each pass claims the job again.
		expireDecisionBackoff(t, fx)
		if _, err := fx.r.RunOnce(context.Background(), "tick", 10, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if j, _ := fx.s.GetDecisionJob(fx.item.ID, triageSet); j == nil {
		t.Fatalf("a job failing only transiently was dropped after %d attempts", maxDecisionAttempts+2)
	}
}

func TestRunner_PermanentRefusalIsDroppedAfterRepeatedAttempts(t *testing.T) {
	fx := newRunnerFixture(t)
	fx.f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"bad request"}`, http.StatusBadRequest)
	})
	for i := 0; i < maxDecisionAttempts; i++ {
		expireDecisionBackoff(t, fx)
		if _, err := fx.r.RunOnce(context.Background(), "tick", 10, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if j, _ := fx.s.GetDecisionJob(fx.item.ID, triageSet); j != nil {
		t.Fatalf("a permanently refused job was kept after %d attempts: %+v", maxDecisionAttempts, j)
	}
}

// expireDecisionBackoff makes a held-for-backoff job claimable now, through
// the same release a superseding write would cause.
func expireDecisionBackoff(t *testing.T, fx *runnerFixture) {
	t.Helper()
	j, _ := fx.s.GetDecisionJob(fx.item.ID, triageSet)
	if j == nil || j.ClaimedBy == "" {
		return
	}
	if err := fx.s.ReleaseDecisionJob(store.DecisionJob{ItemID: fx.item.ID, QuestionSet: triageSet, ClaimedBy: j.ClaimedBy}); err != nil {
		t.Fatal(err)
	}
}

// M35: a workspace deleted between the claim and the send, through RunOnce —
// the job is HELD (owed, unclaimed, no attempt), not dropped.
func TestRunner_WorkspaceDeletedMidJobIsHeld(t *testing.T) {
	fx := newRunnerFixture(t)
	fx.r.beforeAsk = func() {
		if err := fx.s.DeleteWorkspace(fx.ws.Slug); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fx.r.RunOnce(context.Background(), "tick", 10, time.Minute); err != nil {
		t.Fatal(err)
	}
	j, _ := fx.s.GetDecisionJob(fx.item.ID, triageSet)
	if j == nil || j.ClaimedBy != "" || j.Attempts != 0 {
		t.Fatalf("job for a workspace deleted mid-job is %+v; want held (owed, unclaimed, attempts=0)", j)
	}
}
