package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/decision"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// backfillRig is a server with two backfill candidates, decisions off, and a
// hook that parks the FIRST run at chunk 0 (chunk size 1) until it is
// cancelled or released. Later runs pass straight through.
type backfillRig struct {
	srv       *Server
	items     []*models.Item
	entered   chan struct{} // first run reached its first chunk
	cancelled chan struct{} // first run saw its context cancelled
	release   chan struct{}
	runs      atomic.Int32
	hookCalls atomic.Int32
	once      sync.Once
}

func newBackfillRig(t *testing.T) *backfillRig {
	t.Helper()
	clearDecisionEnv(t)
	srv, _ := decisionSettingsServer(t)
	ws, coll := decisionSettingsWorkspace(t, srv)
	g := &backfillRig{srv: srv, entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	for _, title := range []string{"One", "Two"} {
		it, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: title, Fields: `{"status":"open"}`})
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.store.InsertItemDecisions(ws.ID, []models.ItemDecision{{
			ItemID: it.ID, QuestionSet: decision.AttentionSetName, QuestionKey: decision.AttentionNeedsHuman, Kind: "noul",
			Answer: []byte(`{"type":"noul","noul":0.9}`), Provider: "fake", Model: "m",
			StateHash: "h", QuestionHash: "q", ItemSeq: 1,
		}}); err != nil {
			t.Fatal(err)
		}
		g.items = append(g.items, it)
	}
	srv.decisionSettings.backfillChunk = 1
	srv.decisionSettings.backfillHook = func(ctx context.Context, chunk int) error {
		g.hookCalls.Add(1)
		if chunk != 0 || g.runs.Add(1) != 1 {
			return nil
		}
		close(g.entered)
		select {
		case <-ctx.Done():
			close(g.cancelled)
			return ctx.Err()
		case <-g.release:
			return nil
		}
	}
	t.Cleanup(func() { g.once.Do(func() { close(g.release) }) })
	t.Setenv("PAD_DECISION_PROVIDER", decision.ProviderTypesafe)
	t.Setenv("PAD_TYPESAFE_API_KEY", "env-key")
	return g
}

func (g *backfillRig) start(t *testing.T) {
	t.Helper()
	if err := g.srv.ConfigureDecisions(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the backfill never reached its first chunk")
	}
}

func (g *backfillRig) waitCancelled(t *testing.T, what string) {
	t.Helper()
	select {
	case <-g.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not cancel the running backfill", what)
	}
}

func (g *backfillRig) drain(t *testing.T) {
	t.Helper()
	g.once.Do(func() { close(g.release) })
	g.srv.stopDecisionTick() // the tick loop is on bg too
	done := make(chan struct{})
	go func() { g.srv.bg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a background goroutine did not exit")
	}
}

func (g *backfillRig) owedJobs(t *testing.T) int {
	t.Helper()
	n := 0
	for _, it := range g.items {
		j, err := g.srv.store.GetDecisionJob(it.ID, decision.AttentionSetName)
		if err != nil {
			t.Fatal(err)
		}
		if j != nil {
			n++
		}
	}
	return n
}

func (g *backfillRig) marker(t *testing.T) string {
	t.Helper()
	m, err := g.srv.store.GetPlatformSetting(decision.EffortBackfillSetting)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// F1: Stop() while a backfill is parked mid-run cancels it, the goroutine
// exits (bg drains), and the marker is never written.
func TestEffortBackfill_StopCancelsARunningBackfill(t *testing.T) {
	g := newBackfillRig(t)
	g.start(t)
	g.srv.stopDecisionTick()
	g.waitCancelled(t, "stopDecisionTick")
	g.drain(t)
	if m := g.marker(t); m != "" {
		t.Errorf("marker written by a cancelled backfill: %q", m)
	}
	if n := g.owedJobs(t); n != 0 {
		t.Errorf("%d jobs owed by a backfill cancelled before its first chunk", n)
	}
}

// F1: after Stop() a start does nothing: no run, no state left behind.
func TestEffortBackfill_NoStartAfterStop(t *testing.T) {
	g := newBackfillRig(t)
	g.srv.stopDecisionTick()
	if err := g.srv.ConfigureDecisions(); err != nil {
		t.Fatal(err)
	}
	r := g.srv.decisionRunner()
	if r == nil {
		t.Fatal("precondition: no runner, so a start was never attempted")
	}
	g.srv.startEffortBackfill(r)
	g.drain(t)
	g.srv.decisionTick.mu.Lock()
	gen, c := g.srv.decisionTick.backfillGen, g.srv.decisionTick.backfillCancel
	g.srv.decisionTick.mu.Unlock()
	if gen != 0 || c != nil || g.hookCalls.Load() != 0 {
		t.Errorf("a backfill started after Stop: gen=%d cancel=%v hook calls=%d", gen, c != nil, g.hookCalls.Load())
	}
	if m := g.marker(t); m != "" || g.owedJobs(t) != 0 {
		t.Errorf("a backfill ran after Stop: marker %q jobs %d", m, g.owedJobs(t))
	}
}

// F2: disabling decisions while a backfill is parked before its first chunk
// cancels it: no job is owed (rows counted, not just the marker) and no
// marker is written. A chunk already inside OweDecisionJobs when the disable
// lands may still commit (lead ruling bdce2116, docs only); this test does
// not cover that window.
func TestEffortBackfill_DisableCancelsARunningBackfill(t *testing.T) {
	g := newBackfillRig(t)
	g.start(t)
	t.Setenv("PAD_DECISION_PROVIDER", decision.ProviderNone)
	if err := g.srv.ConfigureDecisions(); err != nil {
		t.Fatal(err)
	}
	if g.srv.decisionRunner() != nil {
		t.Fatal("precondition: decisions are still on")
	}
	g.waitCancelled(t, "disabling")
	g.drain(t)
	if n := g.owedJobs(t); n != 0 {
		t.Errorf("%d decision_jobs rows owed after a disable, with no provider", n)
	}
	if m := g.marker(t); m != "" {
		t.Errorf("marker written after a disable: %q", m)
	}
}

// F2: replacing the runner mid-backfill cancels the old run, and the new run
// completes: both items owed, marker set, the old run having written nothing.
func TestEffortBackfill_ReplaceCancelsTheOldRunAndTheNewOneCompletes(t *testing.T) {
	g := newBackfillRig(t)
	g.start(t)
	if err := g.srv.ConfigureDecisions(); err != nil { // new runner, same provider
		t.Fatal(err)
	}
	g.waitCancelled(t, "replacing the runner")
	deadline := time.Now().Add(10 * time.Second)
	for g.marker(t) == "" {
		if time.Now().After(deadline) {
			t.Fatal("the replacement backfill never set the marker")
		}
		time.Sleep(20 * time.Millisecond)
	}
	g.drain(t)
	if n := g.owedJobs(t); n != 2 {
		t.Errorf("owed jobs = %d, want both items", n)
	}
	g.srv.decisionTick.mu.Lock()
	c := g.srv.decisionTick.backfillCancel
	g.srv.decisionTick.mu.Unlock()
	if c != nil {
		t.Error("the finished run left its cancel registered")
	}
}
