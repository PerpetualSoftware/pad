package store

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Typed-decision queue and storage (TASK-3117). Runs on both backends via
// testStore (PAD_TEST_POSTGRES_URL selects Postgres).

func decisionFixture(t *testing.T) (*Store, *models.Workspace, *models.Collection) {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Decisions")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	return s, ws, col
}

func countDecisionJobs(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM decision_jobs`).Scan(&n); err != nil {
		t.Fatalf("count decision_jobs: %v", err)
	}
	return n
}

func mustJob(t *testing.T, s *Store, itemID, set string) *DecisionJobState {
	t.Helper()
	j, err := s.GetDecisionJob(itemID, set)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if j == nil {
		t.Fatalf("no decision job owed for %s/%s", itemID, set)
	}
	return j
}

// The nil-provider case: no resolver installed means every door writes nothing.
// The doors are exercised for real, so a pass is not a door that never ran.
func TestDecisionJobs_NoResolverEnqueuesNothing(t *testing.T) {
	s, ws, col := decisionFixture(t)
	item := createTestItem(t, s, ws.ID, col.ID, "Quiet", "body")
	title := "Quiet, renamed"
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Author: "a", Body: "hi"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if n := countDecisionJobs(t, s); n != 0 {
		t.Fatalf("decision_jobs has %d rows with no resolver installed; want 0", n)
	}

	// Positive control on the same fixture: installing a resolver makes the
	// very next write enqueue, so the zero above is about the resolver.
	s.SetDecisionSetResolver(func(string) []string { return []string{"triage"} })
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if n := countDecisionJobs(t, s); n != 1 {
		t.Fatalf("after installing a resolver, decision_jobs has %d rows; want 1", n)
	}
}

// Each of the three doors enqueues, and repeats coalesce into ONE row whose
// generation counts the writes.
func TestDecisionJobs_EachDoorEnqueuesAndCoalesces(t *testing.T) {
	s, ws, col := decisionFixture(t)
	s.SetDecisionSetResolver(func(slug string) []string {
		if slug != col.Slug {
			t.Errorf("resolver got collection slug %q, want %q", slug, col.Slug)
		}
		return []string{"triage"}
	})

	item := createTestItem(t, s, ws.ID, col.ID, "Door", "body")
	if g := mustJob(t, s, item.ID, "triage").Generation; g != 1 {
		t.Fatalf("after create, generation = %d; want 1", g)
	}

	title := "Door 2"
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if g := mustJob(t, s, item.ID, "triage").Generation; g != 2 {
		t.Fatalf("after update, generation = %d; want 2", g)
	}

	if _, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Author: "a", Body: "note"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if g := mustJob(t, s, item.ID, "triage").Generation; g != 3 {
		t.Fatalf("after comment, generation = %d; want 3", g)
	}
	if n := countDecisionJobs(t, s); n != 1 {
		t.Fatalf("three writes left %d job rows; want 1 (coalesced)", n)
	}
}

func TestDecisionJobs_ResolverScopesByCollection(t *testing.T) {
	s, ws, col := decisionFixture(t)
	other := createTestCollection(t, s, ws.ID, "Ideas")
	s.SetDecisionSetResolver(func(slug string) []string {
		if slug == col.Slug {
			return []string{"triage"}
		}
		return nil
	})
	createTestItem(t, s, ws.ID, other.ID, "Out of scope", "")
	if n := countDecisionJobs(t, s); n != 0 {
		t.Fatalf("an item in an unscoped collection enqueued %d jobs; want 0", n)
	}
	createTestItem(t, s, ws.ID, col.ID, "In scope", "")
	if n := countDecisionJobs(t, s); n != 1 {
		t.Fatalf("an item in the scoped collection enqueued %d jobs; want 1", n)
	}
}

// A claim is exclusive until its lease lapses, and then any runner may take it.
func TestDecisionJobs_ClaimIsExclusiveUntilLeaseLapses(t *testing.T) {
	s, ws, col := decisionFixture(t)
	s.SetDecisionSetResolver(func(string) []string { return []string{"triage"} })
	item := createTestItem(t, s, ws.ID, col.ID, "Claim", "")

	a, err := s.ClaimDecisionJobs("runner-a", 10, time.Hour)
	if err != nil || len(a) != 1 {
		t.Fatalf("runner-a claimed %d (err %v); want 1", len(a), err)
	}
	b, err := s.ClaimDecisionJobs("runner-b", 10, time.Hour)
	if err != nil || len(b) != 0 {
		t.Fatalf("runner-b claimed %d under a live lease (err %v); want 0", len(b), err)
	}

	// A dead runner: its lease is already in the past.
	if _, err := s.db.Exec(s.q(`UPDATE decision_jobs SET lease_expires_at = ? WHERE item_id = ?`),
		decisionTime(time.Now().Add(-time.Minute)), item.ID); err != nil {
		t.Fatal(err)
	}
	b, err = s.ClaimDecisionJobs("runner-b", 10, time.Hour)
	if err != nil || len(b) != 1 {
		t.Fatalf("runner-b claimed %d after the lease lapsed (err %v); want 1", len(b), err)
	}
	// The dead runner's late completion must not delete runner-b's job.
	if err := s.CompleteDecisionJob(a[0]); err != nil {
		t.Fatal(err)
	}
	if j := mustJob(t, s, item.ID, "triage"); j.ClaimedBy != "runner-b" {
		t.Fatalf("after the stale completion the job is claimed by %q; want runner-b", j.ClaimedBy)
	}
}

// A write that lands mid-evaluation leaves the job owed: completion of the
// older generation releases rather than deletes.
func TestDecisionJobs_ReenqueueDuringEvaluationSurvivesCompletion(t *testing.T) {
	s, ws, col := decisionFixture(t)
	s.SetDecisionSetResolver(func(string) []string { return []string{"triage"} })
	item := createTestItem(t, s, ws.ID, col.ID, "Race", "")

	claimed, err := s.ClaimDecisionJobs("r", 10, time.Hour)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %d, %v", len(claimed), err)
	}
	title := "Race, edited mid-evaluation"
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteDecisionJob(claimed[0]); err != nil {
		t.Fatal(err)
	}
	j := mustJob(t, s, item.ID, "triage")
	if j.Generation != 2 || j.ClaimedBy != "" {
		t.Fatalf("after completing gen 1 under a gen-2 enqueue: generation=%d claimed_by=%q; want 2, unclaimed",
			j.Generation, j.ClaimedBy)
	}
	again, err := s.ClaimDecisionJobs("r", 10, time.Hour)
	if err != nil || len(again) != 1 || again[0].Generation != 2 {
		t.Fatalf("the newer generation is not claimable at once: %+v, %v", again, err)
	}

	// Control: with no intervening write, completion deletes.
	if err := s.CompleteDecisionJob(again[0]); err != nil {
		t.Fatal(err)
	}
	if n := countDecisionJobs(t, s); n != 0 {
		t.Fatalf("completing the current generation left %d rows; want 0", n)
	}
}

func TestDecisionJobs_FailureBacksOff(t *testing.T) {
	s, ws, col := decisionFixture(t)
	s.SetDecisionSetResolver(func(string) []string { return []string{"triage"} })
	item := createTestItem(t, s, ws.ID, col.ID, "Fail", "")

	claimed, _ := s.ClaimDecisionJobs("r", 10, time.Hour)
	if err := s.FailDecisionJob(claimed[0], errors.New("provider 503"), time.Hour); err != nil {
		t.Fatal(err)
	}
	j := mustJob(t, s, item.ID, "triage")
	if j.Attempts != 1 || j.LastError != "provider 503" {
		t.Fatalf("after one failure: attempts=%d last_error=%q", j.Attempts, j.LastError)
	}
	if again, _ := s.ClaimDecisionJobs("r2", 10, time.Hour); len(again) != 0 {
		t.Fatalf("a job inside its backoff was claimed")
	}

	// A new write is new work: it resets the failure history.
	title := "Fail, edited"
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if j := mustJob(t, s, item.ID, "triage"); j.Attempts != 0 || j.LastError != "" {
		t.Fatalf("an enqueue did not reset failure history: attempts=%d last_error=%q", j.Attempts, j.LastError)
	}
}

// The read returns one row per question — the newest — and keeps the rest.
func TestItemDecisions_LatestHidesOlderStates(t *testing.T) {
	s, ws, col := decisionFixture(t)
	item := createTestItem(t, s, ws.ID, col.ID, "Read", "")

	row := func(key, hash, at string) models.ItemDecision {
		return models.ItemDecision{
			ItemID: item.ID, QuestionSet: "triage", QuestionKey: key, Kind: "noul",
			Answer: []byte(`{"type":"noul","noul":0.5}`), Provider: "fake", Model: "m",
			StateHash: hash, ItemSeq: 1, EvaluatedAt: at,
		}
	}
	t0 := time.Now().UTC()
	// Two evaluations inside the SAME second: the fixed-width nanosecond
	// format is what orders them.
	older := decisionTime(t0)
	newer := decisionTime(t0.Add(time.Millisecond))
	if err := s.InsertItemDecisions(ws.ID, []models.ItemDecision{row("urgent", "h1", older), row("stale", "h1", older)}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertItemDecisions(ws.ID, []models.ItemDecision{row("urgent", "h2", newer)}); err != nil {
		t.Fatal(err)
	}
	// A duplicate of an existing state is kept, not replaced.
	if err := s.InsertItemDecisions(ws.ID, []models.ItemDecision{row("urgent", "h2", decisionTime(t0.Add(time.Second)))}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LatestItemDecisions(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("latest read returned %d rows; want 2 (one per key): %+v", len(got), got)
	}
	byKey := map[string]models.ItemDecision{}
	for _, d := range got {
		byKey[d.QuestionKey] = d
	}
	if byKey["urgent"].StateHash != "h2" || byKey["urgent"].EvaluatedAt != newer {
		t.Fatalf("urgent: got hash %q at %q; want h2 at %q", byKey["urgent"].StateHash, byKey["urgent"].EvaluatedAt, newer)
	}
	if byKey["stale"].StateHash != "h1" {
		t.Fatalf("stale: got hash %q; want h1", byKey["stale"].StateHash)
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM item_decisions`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("item_decisions holds %d rows; want 3 (older state kept for the audit, duplicate refused)", total)
	}
	if ok, _ := s.HasItemDecisionsAtState(item.ID, "triage", "h2", map[string]string{"urgent": "", "stale": ""}); ok {
		t.Fatal("HasItemDecisionsAtState reported a complete set at h2, which has only one of two keys")
	}
	if ok, _ := s.HasItemDecisionsAtState(item.ID, "triage", "h1", map[string]string{"urgent": "", "stale": ""}); !ok {
		t.Fatal("HasItemDecisionsAtState missed the complete set at h1")
	}
}

// Move is a door (TASK-3117 ruling 3), keyed on the TARGET collection's sets:
// a move INTO a scoped collection makes an evaluation owed where the item's
// old collection owed none.
func TestDecisionJobs_MoveEnqueuesForTargetCollection(t *testing.T) {
	s, ws, col := decisionFixture(t)
	other := createTestCollection(t, s, ws.ID, "Ideas")
	s.SetDecisionSetResolver(func(slug string) []string {
		if slug == col.Slug {
			return []string{"triage"}
		}
		return nil
	})
	item := createTestItem(t, s, ws.ID, other.ID, "Mover", "")
	if n := countDecisionJobs(t, s); n != 0 {
		t.Fatalf("precondition: item in an unscoped collection enqueued %d jobs", n)
	}
	if _, err := s.MoveItem(item.ID, col.ID, item.Fields); err != nil {
		t.Fatalf("move: %v", err)
	}
	if j := mustJob(t, s, item.ID, "triage"); j.Generation != 1 {
		t.Fatalf("after move into the scoped collection, generation = %d; want 1", j.Generation)
	}
}

// Comment edit and delete change the trail, so they are doors too (ruling 3's
// rule applied). An edit that leaves the body unchanged is not.
func TestDecisionJobs_CommentEditAndDeleteEnqueue(t *testing.T) {
	s, ws, col := decisionFixture(t)
	item := createTestItem(t, s, ws.ID, col.ID, "Trail", "")
	c, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Author: "a", Body: "first"})
	if err != nil {
		t.Fatal(err)
	}
	s.SetDecisionSetResolver(func(string) []string { return []string{"triage"} })

	if _, err := s.UpdateComment(c.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.GetDecisionJob(item.ID, "triage"); j != nil {
		t.Fatalf("a no-op comment edit enqueued: %+v", j)
	}
	if _, err := s.UpdateComment(c.ID, "first, corrected"); err != nil {
		t.Fatal(err)
	}
	if g := mustJob(t, s, item.ID, "triage").Generation; g != 1 {
		t.Fatalf("after a comment edit, generation = %d; want 1", g)
	}
	if err := s.DeleteComment(c.ID); err != nil {
		t.Fatal(err)
	}
	if g := mustJob(t, s, item.ID, "triage").Generation; g != 2 {
		t.Fatalf("after a comment delete, generation = %d; want 2", g)
	}
}

// Codex round 2 P1: dropping a claim (retry limit, gone item) must not take a
// NEWER generation with it — a write that landed mid-evaluation is still owed.
func TestDecisionJobs_DropPreservesANewerGeneration(t *testing.T) {
	s, ws, col := decisionFixture(t)
	s.SetDecisionSetResolver(func(string) []string { return []string{"triage"} })
	item := createTestItem(t, s, ws.ID, col.ID, "Drop", "")
	claimed, err := s.ClaimDecisionJobs("r", 10, time.Hour)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %d %v", len(claimed), err)
	}
	title := "Drop, edited mid-evaluation"
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if err := s.DropDecisionJob(claimed[0]); err != nil {
		t.Fatal(err)
	}
	j := mustJob(t, s, item.ID, "triage")
	if j.Generation != 2 || j.ClaimedBy != "" {
		t.Fatalf("after dropping gen 1 under a gen-2 enqueue: %+v; want generation 2, unclaimed", j)
	}

	// Control: with no intervening write the drop removes the row.
	again, _ := s.ClaimDecisionJobs("r", 10, time.Hour)
	if err := s.DropDecisionJob(again[0]); err != nil {
		t.Fatal(err)
	}
	if n := countDecisionJobs(t, s); n != 0 {
		t.Fatalf("dropping the current generation left %d rows", n)
	}
}

// Codex round 3: a set whose keys CHANGED at an unchanged state is not
// complete. A rename keeps the count equal (old key stored, new key asked),
// so a count over every stored key would report it complete and the new key
// would never be answered.
func TestItemDecisions_HasStateCountsOnlyTheAskedKeys(t *testing.T) {
	s, ws, col := decisionFixture(t)
	item := createTestItem(t, s, ws.ID, col.ID, "Keys", "")
	row := func(key string) models.ItemDecision {
		return models.ItemDecision{ItemID: item.ID, QuestionSet: "triage", QuestionKey: key, Kind: "noul",
			Answer: []byte(`{"type":"noul","noul":0.5}`), Provider: "fake", Model: "m", StateHash: "h", ItemSeq: 1}
	}
	if err := s.InsertItemDecisions(ws.ID, []models.ItemDecision{row("urgent"), row("old_name")}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasItemDecisionsAtState(item.ID, "triage", "h", map[string]string{"urgent": "", "new_name": ""}); err != nil || ok {
		t.Fatalf("renamed key: HasItemDecisionsAtState = %v, %v; want false (new_name has no answer)", ok, err)
	}
	if ok, _ := s.HasItemDecisionsAtState(item.ID, "triage", "h", map[string]string{"urgent": ""}); !ok {
		t.Fatal("control: a set whose every asked key is stored must read complete")
	}
	// Codex round 6: the same key under a different question hash (a reworded
	// question, or a new model) is not an answer.
	if ok, _ := s.HasItemDecisionsAtState(item.ID, "triage", "h", map[string]string{"urgent": "reworded"}); ok {
		t.Fatal("an answer to an older question definition counted as an answer to the new one")
	}
}

// Codex round 4: restore is a door. A job owed at delete time is dropped as
// gone; without an enqueue on restore the item would come back with stale
// answers and nothing scheduled.
func TestDecisionJobs_RestoreEnqueues(t *testing.T) {
	s, ws, col := decisionFixture(t)
	s.SetDecisionSetResolver(func(string) []string { return []string{"triage"} })
	item := createTestItem(t, s, ws.ID, col.ID, "Phoenix", "")
	claimed, _ := s.ClaimDecisionJobs("r", 10, time.Hour)
	if err := s.DropDecisionJob(claimed[0]); err != nil { // as the runner does for a gone item
		t.Fatal(err)
	}
	if err := s.DeleteItem(item.ID); err != nil {
		t.Fatal(err)
	}
	if n := countDecisionJobs(t, s); n != 0 {
		t.Fatalf("precondition: %d jobs before restore", n)
	}
	if _, err := s.RestoreItem(item.ID); err != nil {
		t.Fatal(err)
	}
	if j := mustJob(t, s, item.ID, "triage"); j.Generation != 1 {
		t.Fatalf("after restore, generation = %d; want 1", j.Generation)
	}
}

// failOneAttentionJob owes, claims and fails one attention job with this
// message, and returns the job's stored state (BUG-4018).
func failOneAttentionJob(t *testing.T, s *Store, wsID, itemID, msg string) *DecisionJobState {
	t.Helper()
	if err := s.OweDecisionJobs(wsID, "attention", []string{itemID}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimDecisionJobs("r", 10, time.Hour)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %d, %v", len(claimed), err)
	}
	if err := s.FailDecisionJob(claimed[0], errors.New(msg), time.Hour); err != nil {
		t.Fatalf("FailDecisionJob must record the failure, got: %v", err)
	}
	return mustJob(t, s, itemID, "attention")
}

// A failure message is arbitrary provider bytes. Whatever it holds, the
// failure is recorded: attempts counted, the set reads as failing, and the
// job is no longer owed (BUG-4018; Postgres TEXT refuses invalid UTF-8).
func TestFailDecisionJob_RecordsAnyMessage(t *testing.T) {
	cases := []struct {
		name, msg string
		want      func(string) bool
	}{
		{"straddle 2-byte", strings.Repeat("a", 1023) + "é" + "tail", func(g string) bool { return g == strings.Repeat("a", 1023) }},
		{"straddle 3-byte", strings.Repeat("a", 1022) + "€" + "tail", func(g string) bool { return g == strings.Repeat("a", 1022) }},
		{"invalid utf8", "bad \xff\xfe bytes", func(g string) bool { return g == "bad \uFFFD bytes" }},
		{"nul", "has \x00 nul", func(g string) bool { return g == "has \uFFFD nul" }},
		{"short ascii", "provider 503", func(g string) bool { return g == "provider 503" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, ws, col := decisionFixture(t)
			s.SetDecisionSetResolver(func(string) []string { return nil })
			item := createTestItem(t, s, ws.ID, col.ID, "Fail", "")
			j := failOneAttentionJob(t, s, ws.ID, item.ID, c.msg)
			if !utf8.ValidString(j.LastError) || len(j.LastError) > maxDecisionErrorBytes || strings.ContainsRune(j.LastError, 0) {
				t.Fatalf("last_error not clean: len=%d valid=%v", len(j.LastError), utf8.ValidString(j.LastError))
			}
			if !c.want(j.LastError) {
				t.Fatalf("last_error = %q", j.LastError)
			}
			if j.Attempts != 1 {
				t.Fatalf("attempts = %d, want 1", j.Attempts)
			}
			if failing, err := s.DecisionJobsFailing(ws.ID, "attention"); err != nil || !failing {
				t.Fatalf("DecisionJobsFailing = %v, %v; want true", failing, err)
			}
			if got, err := s.OwedDecisionJobs(ws.ID, []string{item.ID}, "attention"); err != nil || got[item.ID] {
				t.Fatalf("a failed job still reads as owed: %v %v", got, err)
			}
		})
	}
}

// lastRuneStartAtOrBelow returns the largest rune start <= limit in a string
// built as pad ASCII bytes followed by n runes of width w. Computed from the
// construction, not from the function under test.
func lastRuneStartAtOrBelow(pad, w, n, limit int) int {
	best := 0
	for k := 0; k <= n; k++ {
		if p := pad + k*w; p <= limit {
			best = p
		}
	}
	return best
}

func TestTruncateDecisionError_Boundary(t *testing.T) {
	for _, r := range []string{"é", "€", "😀"} {
		w := len(r)
		for pad := 1010; pad <= 1024; pad++ {
			in := strings.Repeat("a", pad) + r + r
			want := in
			if len(in) > maxDecisionErrorBytes {
				want = in[:lastRuneStartAtOrBelow(pad, w, 2, maxDecisionErrorBytes)]
			}
			if got := truncateDecisionError(in); got != want {
				t.Fatalf("pad=%d rune=%q: len(got)=%d len(want)=%d", pad, r, len(got), len(want))
			}
		}
	}
	exact := strings.Repeat("a", maxDecisionErrorBytes)
	if got := truncateDecisionError(exact); got != exact {
		t.Fatalf("exact-limit ASCII changed: len=%d", len(got))
	}
	// Plain ASCII just over the limit pins the limit check directly: a check
	// off by one byte (len(s) <= max+1) returns all 1025 bytes here. The
	// rune cases above catch it too, at pad 1021 with a 2-byte rune (1025
	// bytes), but only as a side effect of their own cut (BUG-4018 r2).
	for _, n := range []int{maxDecisionErrorBytes + 1, 2000} {
		over := strings.Repeat("a", n)
		if got := truncateDecisionError(over); got != over[:maxDecisionErrorBytes] {
			t.Fatalf("%d ASCII bytes: got len=%d, want exactly %d", n, len(got), maxDecisionErrorBytes)
		}
	}
}

// Sanitising can grow a message (NUL or an invalid byte becomes a 3-byte
// U+FFFD), so it must run BEFORE the cut or the stored value can exceed the
// limit (BUG-4018).
func TestFailDecisionJob_SanitiseBeforeCut(t *testing.T) {
	interleaved := strings.Repeat("a\xff", 400)
	cases := []struct {
		name, msg, want string
		pre             func(t *testing.T)
	}{
		{
			name: "under limit raw, over after sanitise",
			msg:  interleaved,
			want: strings.Repeat("a\uFFFD", 256), // 256*4 = 1024
			pre: func(t *testing.T) {
				if len(interleaved) >= maxDecisionErrorBytes || len(strings.ToValidUTF8(interleaved, "\uFFFD")) <= maxDecisionErrorBytes {
					t.Fatal("precondition: raw < 1024 and sanitised > 1024")
				}
			},
		},
		{"nul straddling the boundary", strings.Repeat("a", 1023) + "\x00tail", strings.Repeat("a", 1023), nil},
		{"nul just below the boundary", strings.Repeat("a", 1022) + "\x00tail", strings.Repeat("a", 1022), nil},
		{"4-byte rune straddling", strings.Repeat("a", 1022) + "😀tail", strings.Repeat("a", 1022), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.pre != nil {
				c.pre(t)
			}
			s, ws, col := decisionFixture(t)
			s.SetDecisionSetResolver(func(string) []string { return nil })
			item := createTestItem(t, s, ws.ID, col.ID, "Fail", "")
			j := failOneAttentionJob(t, s, ws.ID, item.ID, c.msg)
			if !utf8.ValidString(j.LastError) || strings.ContainsRune(j.LastError, 0) || len(j.LastError) > maxDecisionErrorBytes {
				t.Fatalf("last_error not clean: len=%d valid=%v", len(j.LastError), utf8.ValidString(j.LastError))
			}
			if j.LastError != c.want {
				t.Fatalf("last_error len=%d, want len=%d", len(j.LastError), len(c.want))
			}
			if j.Attempts != 1 {
				t.Fatalf("attempts = %d, want 1", j.Attempts)
			}
			if failing, err := s.DecisionJobsFailing(ws.ID, "attention"); err != nil || !failing {
				t.Fatalf("DecisionJobsFailing = %v, %v; want true", failing, err)
			}
			if got, err := s.OwedDecisionJobs(ws.ID, []string{item.ID}, "attention"); err != nil || got[item.ID] {
				t.Fatalf("a failed job still reads as owed: %v %v", got, err)
			}
		})
	}
}
