package decision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3921: the staged attention questions and their one-shot backfill.

const evalTimeFormat = "2006-01-02T15:04:05.000000000Z"

// seedAnswers stores a Noul-shaped row per key for the item in the attention
// set, evaluated at `at`, as the runner would have left them.
func (fx *attentionFixture) seedAnswers(t *testing.T, it *models.Item, at time.Time, keys ...string) {
	t.Helper()
	rows := make([]models.ItemDecision, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, models.ItemDecision{
			ItemID: it.ID, QuestionSet: AttentionSetName, QuestionKey: k, Kind: "noul",
			Answer: []byte(`{"type":"noul","noul":0.9}`), Provider: "fake", Model: "m",
			StateHash: "h", QuestionHash: "q-" + k, ItemSeq: 1, EvaluatedAt: at.UTC().Format(evalTimeFormat),
		})
	}
	if err := fx.s.InsertItemDecisions(fx.ws.ID, rows); err != nil {
		t.Fatal(err)
	}
}

// clearJobs drops the jobs the item creations owed, so a test sees only what
// the backfill owes.
func (fx *attentionFixture) clearJobs(t *testing.T) {
	t.Helper()
	if _, err := fx.s.DB().Exec(`DELETE FROM decision_jobs`); err != nil {
		t.Fatal(err)
	}
}

// jobItems returns item_id -> number of rows, for the attention set.
func (fx *attentionFixture) jobItems(t *testing.T) map[string]int {
	t.Helper()
	rows, err := fx.s.DB().Query(`SELECT item_id, question_set FROM decision_jobs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id, set string
		if err := rows.Scan(&id, &set); err != nil {
			t.Fatal(err)
		}
		if set != AttentionSetName {
			t.Fatalf("the backfill owed a job for set %q; it must owe the attention set only", set)
		}
		out[id]++
	}
	return out
}

func (fx *attentionFixture) marker(t *testing.T) string {
	t.Helper()
	v, err := fx.s.GetPlatformSetting(EffortBackfillSetting)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var allAttentionKeys = []string{AttentionNeedsHuman, AttentionEffort, AttentionEffortMedium, AttentionHighStakes}

func TestEffortBackfill_RunsOnceAndSkipsUnasked(t *testing.T) {
	fx := newAttentionFixture(t)
	now := time.Now().Add(-3 * time.Hour)
	sys, err := fx.s.CreateCollection(fx.ws.ID, models.CollectionCreate{Name: "Conventions", Schema: statusSchema, IsSystem: true})
	if err != nil {
		t.Fatal(err)
	}
	a := fx.item(t, fx.tasks.ID, "open") // needs_human only: owed
	b := fx.item(t, fx.tasks.ID, "open") // already has effort: skipped
	c := fx.item(t, fx.tasks.ID, "open") // never answered: skipped
	d := fx.item(t, fx.tasks.ID, "done") // terminal: skipped
	e := fx.item(t, sys.ID, "open")      // system collection: skipped
	f := fx.item(t, fx.tasks.ID, "open") // soft-deleted: skipped
	fx.seedAnswers(t, a, now, AttentionNeedsHuman)
	fx.seedAnswers(t, b, now, allAttentionKeys...)
	fx.seedAnswers(t, d, now, AttentionNeedsHuman)
	fx.seedAnswers(t, e, now, AttentionNeedsHuman)
	fx.seedAnswers(t, f, now, AttentionNeedsHuman)
	_ = c
	if err := fx.s.DeleteItem(f.ID); err != nil {
		t.Fatal(err)
	}
	fx.clearJobs(t)

	// The dry run counts and writes nothing.
	n, err := fx.r.CountEffortBackfill(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("dry run = %d, %v; want 1", n, err)
	}
	if got := fx.jobItems(t); len(got) != 0 || fx.marker(t) != "" {
		t.Fatalf("the dry run wrote: jobs %v marker %q", got, fx.marker(t))
	}

	owed, err := fx.r.BackfillEffort(context.Background())
	if err != nil || owed != 1 {
		t.Fatalf("BackfillEffort = %d, %v; want 1", owed, err)
	}
	if got := fx.jobItems(t); len(got) != 1 || got[a.ID] != 1 {
		t.Fatalf("jobs = %v; want only item a", got)
	}
	if fx.marker(t) == "" {
		t.Fatal("the marker was not written")
	}

	// Second run: the marker stops it, even with a's job gone and a still a
	// candidate (it has no effort answer yet).
	fx.clearJobs(t)
	owed, err = fx.r.BackfillEffort(context.Background())
	if err != nil || owed != 0 {
		t.Fatalf("second run = %d, %v; want 0", owed, err)
	}
	if got := fx.jobItems(t); len(got) != 0 {
		t.Fatalf("second run owed %v", got)
	}
}

func TestEffortBackfill_FailureLeavesNoMarker(t *testing.T) {
	fx := newAttentionFixture(t)
	a := fx.item(t, fx.tasks.ID, "open")
	fx.seedAnswers(t, a, time.Now().Add(-3*time.Hour), AttentionNeedsHuman)
	fx.clearJobs(t)
	boom := errors.New("boom")
	fx.r.beforeBackfillChunk = func(context.Context, int) error { return boom }
	if _, err := fx.r.BackfillEffort(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v; want the injected failure", err)
	}
	if fx.marker(t) != "" {
		t.Fatal("a failed backfill wrote the marker")
	}
}

// A crash between chunks: the second chunk fails, no marker; a re-run owes
// every candidate, one row each, and writes the marker.
func TestEffortBackfill_FailingSecondChunkThenRerun(t *testing.T) {
	fx := newAttentionFixture(t)
	var ids []string
	for i := 0; i < 3; i++ {
		it := fx.item(t, fx.tasks.ID, "open")
		fx.seedAnswers(t, it, time.Now().Add(-3*time.Hour), AttentionNeedsHuman)
		ids = append(ids, it.ID)
	}
	fx.clearJobs(t)
	fx.r.backfillChunk = 1
	boom := errors.New("crash")
	fx.r.beforeBackfillChunk = func(_ context.Context, chunk int) error {
		if chunk == 1 {
			return boom
		}
		return nil
	}
	if _, err := fx.r.BackfillEffort(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v; want the injected failure", err)
	}
	if fx.marker(t) != "" {
		t.Fatal("marker written after a failed second chunk")
	}
	if got := fx.jobItems(t); len(got) != 1 {
		t.Fatalf("after the crash jobs = %v; want the first chunk's one", got)
	}

	fx.r.beforeBackfillChunk = nil
	owed, err := fx.r.BackfillEffort(context.Background())
	if err != nil || owed != 3 {
		t.Fatalf("re-run = %d, %v; want 3", owed, err)
	}
	got := fx.jobItems(t)
	sort.Strings(ids)
	if len(got) != 3 {
		t.Fatalf("jobs = %v; want one per candidate", got)
	}
	for _, id := range ids {
		if got[id] != 1 {
			t.Fatalf("item %s has %d job rows; want exactly 1", id, got[id])
		}
	}
	if fx.marker(t) == "" {
		t.Fatal("marker not written after the completed re-run")
	}
}

// The backfill's jobs go through the installed debounce: an item evaluated
// 10 minutes ago waits out the hour, one evaluated 2 hours ago does not.
func TestEffortBackfill_RespectsDebounce(t *testing.T) {
	fx := newAttentionFixture(t)
	fx.r.Install(fx.s) // the fixture turned the debounce off
	recent := fx.item(t, fx.tasks.ID, "open")
	old := fx.item(t, fx.tasks.ID, "open")
	fx.seedAnswers(t, recent, time.Now().Add(-10*time.Minute), AttentionNeedsHuman)
	fx.seedAnswers(t, old, time.Now().Add(-2*time.Hour), AttentionNeedsHuman)
	fx.clearJobs(t)
	if owed, err := fx.r.BackfillEffort(context.Background()); err != nil || owed != 2 {
		t.Fatalf("BackfillEffort = %d, %v", owed, err)
	}
	jr, err := fx.s.GetDecisionJob(recent.ID, AttentionSetName)
	if err != nil || jr == nil {
		t.Fatalf("recent job: %+v %v", jr, err)
	}
	if jr.NotBefore == "" || jr.NotBefore <= time.Now().UTC().Format(evalTimeFormat) {
		t.Fatalf("recent job not_before = %q; want a future time", jr.NotBefore)
	}
	jo, err := fx.s.GetDecisionJob(old.ID, AttentionSetName)
	if err != nil || jo == nil {
		t.Fatalf("old job: %+v %v", jo, err)
	}
	if jo.NotBefore != "" {
		t.Fatalf("old job not_before = %q; want none", jo.NotBefore)
	}
}

func TestEffortBackfill_NoCallsOnlyJobs(t *testing.T) {
	fx := newAttentionFixture(t)
	a := fx.item(t, fx.tasks.ID, "open")
	fx.seedAnswers(t, a, time.Now().Add(-3*time.Hour), AttentionNeedsHuman)
	fx.clearJobs(t)
	before := len(fx.f.requests)
	if _, err := fx.r.BackfillEffort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(fx.f.requests) - before; got != 0 {
		t.Fatalf("the backfill made %d provider calls; want 0", got)
	}
	if len(fx.jobItems(t)) != 1 {
		t.Fatal("precondition: the backfill owed no job")
	}
	// And the tick then answers the owed job with ONE call carrying all four.
	fx.tick(t)
	if len(fx.f.requests) != before+1 || len(fx.f.requests[before].Questions) != 4 {
		t.Fatalf("tick requests = %d (questions %v)", len(fx.f.requests)-before, fx.f.requests)
	}
}

// Readers: one test per reader that consults decisions, each of which fails
// when that reader's Unsurfaced line is dropped.

// stagedFixture leaves an item whose only above-threshold answer is the staged
// high_stakes (0.95); needs_human_decision is below threshold.
func stagedFixture(t *testing.T, needsHuman float64) (*attentionFixture, *models.Item) {
	t.Helper()
	fx := newAttentionFixture(t)
	fx.set(AttentionNeedsHuman, needsHuman)
	fx.set(AttentionHighStakes, 0.95)
	it := fx.item(t, fx.tasks.ID, "open")
	fx.tick(t)
	if ds, err := fx.r.Decisions(it.ID); err != nil || len(ds) != 4 {
		t.Fatalf("precondition: Decisions returned %d rows, %v; want all four", len(ds), err)
	}
	return fx, it
}

func TestWorkspaceFlags_IgnoresStagedKeys(t *testing.T) {
	fx, it := stagedFixture(t, 0.3)
	flags, _, err := fx.r.WorkspaceFlags(fx.ws.ID, AttentionSetName, AttentionThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := flags[it.ID]; ok {
		t.Fatalf("high_stakes at .95 flagged the item: %v", got)
	}
}

func TestSurfacedDecisions_HidesStagedKeys(t *testing.T) {
	fx, it := stagedFixture(t, 0.9)
	got, _, _, err := fx.r.SurfacedDecisions(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if keys := surfacedKeys(got); len(keys) != 1 || keys[0] != AttentionNeedsHuman {
		t.Fatalf("surfaced = %v; want only needs_human_decision", keys)
	}
}

// The staged keys are not counted anywhere: the score and choice rows would
// each read as below threshold, and the low high_stakes with them.
func TestSurfacedDecisions_StagedKeysAreNotCounted(t *testing.T) {
	fx := newAttentionFixture(t)
	fx.set(AttentionNeedsHuman, 0.9)
	fx.set(AttentionHighStakes, 0.1)
	it := fx.item(t, fx.tasks.ID, "open")
	fx.tick(t)
	_, below, stale, err := fx.r.SurfacedDecisions(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if below != 0 || stale != 0 {
		t.Fatalf("below=%d stale=%d; the staged keys must count in neither", below, stale)
	}
}

// Dry run against a database FILE (a copy of the live one), for measuring the
// backfill before it ships. It calls no provider and writes nothing but the
// migrations store.New applies to the file, so run it on a COPY:
//
//	PAD_EFFORT_DRYRUN_DB=/path/to/copy.db go test ./internal/decision/ -run TestEffortBackfillDryRunOnFile -v -count=1
func TestEffortBackfillDryRunOnFile(t *testing.T) {
	path := os.Getenv("PAD_EFFORT_DRYRUN_DB")
	if path == "" {
		t.Skip("set PAD_EFFORT_DRYRUN_DB to a COPY of a pad database")
	}
	if err := refuseLiveDatabase(path, os.Getenv("HOME")); err != nil {
		t.Fatalf("%v", err)
	}
	s, err := store.New(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer s.Close()
	reg, err := ProductionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	// The provider is never called; the runner only needs one to exist.
	r := NewRunner(s, newTypesafe("unused", "unused"), reg)
	byWS, order, err := r.EffortBackfillCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, ws := range order {
		t.Logf("workspace %s: %d items", ws, len(byWS[ws]))
		total += len(byWS[ws])
	}
	t.Logf("EFFORT BACKFILL WOULD OWE %d JOBS", total)
}

// refuseLiveDatabase guards the dry run: store.New MIGRATES the file it opens,
// so a live database must never be pointed at. A -wal or -shm sibling means it
// is open (or was not closed cleanly), and anything under $HOME/.pad is the
// real data directory.
func refuseLiveDatabase(path, home string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	recipe := fmt.Sprintf("take a copy first: sqlite3 'file:%s?mode=ro' \".backup '<copy>'\"", abs)
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(abs + suffix); err == nil {
			return fmt.Errorf("refusing %s: %s%s exists, so it looks live; %s", abs, abs, suffix, recipe)
		}
	}
	if home != "" {
		padDir := filepath.Join(filepath.Clean(home), ".pad") + string(filepath.Separator)
		if strings.HasPrefix(abs, padDir) {
			return fmt.Errorf("refusing %s: it is under %s; %s", abs, padDir, recipe)
		}
	}
	return nil
}

func TestRefuseLiveDatabase(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "copy.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := refuseLiveDatabase(db, dir+"-other"); err != nil {
		t.Fatalf("a clean copy was refused: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		sib := db + suffix
		if err := os.WriteFile(sib, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := refuseLiveDatabase(db, dir+"-other"); err == nil || !strings.Contains(err.Error(), ".backup") {
			t.Errorf("a %s sibling was not refused with the recipe: %v", suffix, err)
		}
		os.Remove(sib)
	}
	home := t.TempDir()
	padDB := filepath.Join(home, ".pad", "pad.db")
	if err := refuseLiveDatabase(padDB, home); err == nil {
		t.Error("a path under $HOME/.pad was not refused")
	}
}

// A cancel between chunks stops the run with ctx.Err(), owes nothing more and
// never writes the marker; a pre-cancelled context reads no candidate either.
func TestEffortBackfill_CancelStopsBetweenChunksAndWritesNoMarker(t *testing.T) {
	fx := newAttentionFixture(t)
	for i := 0; i < 3; i++ {
		it := fx.item(t, fx.tasks.ID, "open")
		fx.seedAnswers(t, it, time.Now().Add(-3*time.Hour), AttentionNeedsHuman)
	}
	fx.clearJobs(t)

	dead, cancelDead := context.WithCancel(context.Background())
	cancelDead()
	if _, _, err := fx.r.EffortBackfillCandidates(dead); !errors.Is(err, context.Canceled) {
		t.Fatalf("candidate read on a cancelled ctx: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx.r.backfillChunk = 1
	fx.r.beforeBackfillChunk = func(_ context.Context, chunk int) error {
		if chunk == 1 {
			cancel() // lands inside the hook; the chunk-boundary check must see it
		}
		return nil
	}
	owed, err := fx.r.BackfillEffort(ctx)
	if !errors.Is(err, context.Canceled) || owed != 1 {
		t.Fatalf("owed=%d err=%v; want 1 and context.Canceled", owed, err)
	}
	if got := fx.jobItems(t); len(got) != 1 {
		t.Fatalf("jobs after cancel = %v; want only the first chunk's", got)
	}
	if fx.marker(t) != "" {
		t.Fatal("marker written by a cancelled run")
	}
}
