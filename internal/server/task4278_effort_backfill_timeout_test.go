package server

import (
	"context"
	"crypto/rand"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/decision"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// logCapture records every slog record while installed as the default logger.
type logCapture struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	c.recs = append(c.recs, r.Clone())
	c.mu.Unlock()
	return nil
}
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }
func (c *logCapture) count(msg string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.recs {
		if r.Message == msg {
			n++
		}
	}
	return n
}

// TASK-4278. BackfillEffort stays on the runtime pool, with the
// candidate read under its own raised statement bound. If that read ever hits
// the bound: BackfillEffort returns the error, the marker stays UNSET,
// startEffortBackfill logs exactly one error line, nothing loops (a second run
// happens only on a reconfigure or restart), and a later run, with the lock
// released, completes and sets the marker.
func TestTASK4278_EffortBackfill_StatementTimeoutIsLoggedOnceAndRetriedOnReconfigure(t *testing.T) {
	clearDecisionEnv(t)
	st := storetest.NewPostgresBounded(t, store.RuntimeLimits{Statement: 30 * time.Second, Lock: 10 * time.Second, IdleInTx: time.Minute})
	srv := New(st)
	t.Cleanup(func() { srv.Stop() })
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	srv.store.SetEncryptionKey(key)
	srv.SetDecisionTickChannel(make(chan time.Time))
	reg, err := decision.ProductionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	srv.SetDecisionBase(decision.Config{}, reg)
	if err := srv.ConfigureDecisions(); err != nil { // decisions off: no runner, no backfill
		t.Fatal(err)
	}
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	ws, coll := decisionSettingsWorkspace(t, srv)
	it, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "One", Fields: `{"status":"open"}`})
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
	marker := func() string {
		m, err := srv.store.GetPlatformSetting(decision.EffortBackfillSetting)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	// The candidate read's statement bound is lowered to 300 ms (the lock bound
	// stays long, so it is the STATEMENT timeout, 57014, that fires).
	restore := store.SetEffortCandidatesBoundsForTesting(300*time.Millisecond, 30*time.Second)
	defer restore()

	// Hold item_decisions from a separate pooled connection: the read waits.
	holder, err := srv.store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	released := false
	releaseLock := func() {
		if !released {
			released = true
			holder.Rollback()
		}
	}
	defer releaseLock()
	if _, err := holder.Exec(`LOCK TABLE item_decisions IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	cap := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	defer slog.SetDefault(prev)

	const failedMsg = "effort backfill failed; it will be retried on the next restart or settings change"
	t.Setenv("PAD_DECISION_PROVIDER", decision.ProviderTypesafe)
	t.Setenv("PAD_TYPESAFE_API_KEY", "env-key")
	if err := srv.ConfigureDecisions(); err != nil { // enables decisions: starts ONE backfill
		t.Fatal(err)
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("the failure log line", func() bool { return cap.count(failedMsg) >= 1 })

	// Direct call: the error comes back (a statement timeout), no marker.
	runner := srv.decisionRunner()
	if runner == nil {
		t.Fatal("no runner")
	}
	_, err = runner.BackfillEffort(context.Background())
	if err == nil || !store.IsDBBusy(err) || !strings.Contains(err.Error(), "57014") {
		t.Fatalf("BackfillEffort behind a held lock = %v, want the 57014 statement timeout", err)
	}
	if m := marker(); m != "" {
		t.Fatalf("marker = %q after a failed read; it must stay unset", m)
	}

	// Nothing loops: wait well past several statement bounds, still exactly
	// one error line and no goroutine left running.
	time.Sleep(1500 * time.Millisecond)
	if n := cap.count(failedMsg); n != 1 {
		t.Fatalf("error logged %d times, want exactly 1 (a retry loop would log more)", n)
	}
	srv.decisionTick.mu.Lock()
	gen, running := srv.decisionTick.backfillGen, srv.decisionTick.backfillCancel != nil
	srv.decisionTick.mu.Unlock()
	if gen != 1 || running {
		t.Errorf("backfill generation = %d, running = %v; want 1 run, finished", gen, running)
	}
	if m := marker(); m != "" {
		t.Fatalf("marker = %q while the read is still blocked", m)
	}

	// Lock released; the NEXT reconfigure (the stand-in for a restart) runs it
	// again and completes: the item is owed a job and the marker is set.
	releaseLock()
	if err := srv.ConfigureDecisions(); err != nil {
		t.Fatal(err)
	}
	waitFor("the marker", func() bool { return marker() != "" })
	j, err := srv.store.GetDecisionJob(it.ID, decision.AttentionSetName)
	if err != nil || j == nil {
		t.Fatalf("the completed backfill owed no job: %v %v", j, err)
	}
	if n := cap.count(failedMsg); n != 1 {
		t.Errorf("error lines after the successful run = %d, want still 1", n)
	}
}
