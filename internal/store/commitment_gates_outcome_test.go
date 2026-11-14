package store

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Commitment gates (TASK-4353, 2c): the outcome rule and the read side. A gate
// is held against the blocker occurrence fixed when it was written; it opens
// once, on `done`, and never closes. The ticks below run past 2030, the year
// the fixture schedules sit in.

const farFuture = "2031-01-01T00:00:00Z"

// recurring makes item i's schedule daily at 09:00 UTC from its first day.
func (f *linkFix) recurring(t *testing.T, i int) {
	t.Helper()
	f.exec(t, `DELETE FROM item_schedules WHERE id=?`, f.scheds[i].ID)
	sc, err := f.s.CreateCommitmentSchedule(f.ws.ID, f.items[i].ID, models.ScheduleCreate{Kind: "check", Dtstart: "2030-03-04T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, models.CommitmentRolesInput{})
	if err != nil {
		t.Fatal(err)
	}
	f.scheds[i] = sc
}

// deadline gives schedule i a hard deadline at its occurrence.
func (f *linkFix) deadline(t *testing.T, i int, behavior string) {
	t.Helper()
	f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=0, deadline_secs=0, deadline_behavior=? WHERE id=?`, behavior, f.scheds[i].ID)
}

func (f *linkFix) resolve(t *testing.T, i int, state string) {
	t.Helper()
	sc := f.reload(t, f.items[i].ID)
	if _, err := f.s.ResolveOccurrence(f.ws.ID, f.items[i].ID, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: state, Actor: "user", MaySkipBeforeDeadline: true, EndSchedule: true}); err != nil {
		t.Fatalf("resolve %s: %v", state, err)
	}
}

func (f *linkFix) gates(t *testing.T, i int, acc models.CommitmentAccess) *models.ScheduleGateView {
	t.Helper()
	v, err := f.s.ScheduleGates(f.ws.ID, f.items[i].ID, acc)
	if err != nil {
		t.Fatalf("ScheduleGates: %v", err)
	}
	return v
}

// verdict is the state and the one gate's outcome.
func (f *linkFix) verdict(t *testing.T, i int, acc models.CommitmentAccess) (state, outcome string) {
	t.Helper()
	v := f.gates(t, i, acc)
	if len(v.Gates) == 1 {
		outcome = v.Gates[0].Outcome
	}
	return v.State, outcome
}

func (f *linkFix) openedAt(t *testing.T, gateID string) sql.NullString {
	t.Helper()
	var at sql.NullString
	if err := f.s.db.QueryRow(f.s.q(`SELECT opened_at FROM schedule_gates WHERE id=?`), gateID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func (f *linkFix) expect(t *testing.T, what string, i int, acc models.CommitmentAccess, state, outcome string) {
	t.Helper()
	if gotState, gotOutcome := f.verdict(t, i, acc); gotState != state || gotOutcome != outcome {
		t.Fatalf("%s: state=%s outcome=%s, want %s %s", what, gotState, gotOutcome, state, outcome)
	}
}

// ---- the blocker is a commitment ----

func TestGateOpensOnceWhenItsBlockerOccurrenceIsDone(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	acc := f.allEdit()
	g := f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	f.expect(t, "outstanding blocker", 1, acc, models.GateStateWaiting, models.GateOutcomePending)
	if f.openedAt(t, g.ID).Valid {
		t.Fatal("the gate opened before its blocker did")
	}
	f.resolve(t, 0, "done")
	f.expect(t, "done blocker", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
	opened := f.openedAt(t, g.ID)
	if !opened.Valid {
		t.Fatal("the gate did not record that it opened")
	}
	f.expect(t, "second read", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
	if again := f.openedAt(t, g.ID); again != opened {
		t.Fatalf("opened_at changed from %v to %v", opened, again)
	}
}

// The gate is held against the occurrence outstanding when it was written. A
// recurring blocker moving on to its next occurrence can neither re-block work
// that went ahead nor release work that did not.
func TestARecurringBlockerAdvancingNeitherReblocksNorReleases(t *testing.T) {
	t.Parallel()
	t.Run("done then advanced", func(t *testing.T) {
		t.Parallel()
		f := newLinkFix(t, 2)
		f.recurring(t, 0)
		acc := f.allEdit()
		g := f.gate(t, f.items[1].ID, f.items[0].ID, acc)
		f.resolve(t, 0, "done")
		f.expect(t, "first occurrence done", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
		// The blocker now waits on its next occurrence: still released, same opening.
		opened := f.openedAt(t, g.ID)
		if sc := f.reload(t, f.items[0].ID); sc.NextAt == nil || *sc.NextWall != "2030-03-05T09:00:00" {
			t.Fatalf("blocker did not advance: %+v", sc)
		}
		f.expect(t, "blocker on its next occurrence", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
		f.resolve(t, 0, "skip")
		f.expect(t, "next occurrence skipped", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
		if f.openedAt(t, g.ID) != opened {
			t.Fatal("opened_at moved")
		}
	})
	t.Run("skipped then a later one done", func(t *testing.T) {
		t.Parallel()
		f := newLinkFix(t, 2)
		f.recurring(t, 0)
		acc := f.allEdit()
		g := f.gate(t, f.items[1].ID, f.items[0].ID, acc)
		f.resolve(t, 0, "skip")
		f.expect(t, "fixed occurrence skipped", 1, acc, models.GateStateBlockedNeedsDecision, models.GateOutcomeSkipped)
		f.resolve(t, 0, "done")
		f.expect(t, "a later occurrence done", 1, acc, models.GateStateBlockedNeedsDecision, models.GateOutcomeSkipped)
		if f.openedAt(t, g.ID).Valid {
			t.Fatal("a later occurrence opened the gate")
		}
	})
}

// A recurring gated child is gated by schedule: once open it stays open for its
// later occurrences.
func TestARecurringGatedChildStaysOpenForItsLaterOccurrences(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.recurring(t, 1)
	acc := f.allEdit()
	g := f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	f.resolve(t, 0, "done")
	f.expect(t, "blocker done", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
	opened := f.openedAt(t, g.ID)
	for i := 0; i < 3; i++ {
		f.resolve(t, 1, "done")
		f.expect(t, "child occurrence resolved", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
	}
	if f.openedAt(t, g.ID) != opened {
		t.Fatal("opened_at moved")
	}
}

// `done` wins over a recorded miss, and a miss under stays_owed leaves the gate
// pending: the work is still owed, and a later Done opens it.
func TestADoneBlockerSucceedsEvenAfterARecordedMiss(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.deadline(t, 0, "stays_owed")
	acc := f.allEdit()
	f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	f.eval(t, farFuture, 0)
	if n := f.deadlineEvidence(t); n != 1 {
		t.Fatalf("setup: %d misses recorded", n)
	}
	f.expect(t, "miss recorded, stays owed", 1, acc, models.GateStateWaiting, models.GateOutcomePending)
	f.resolve(t, 0, "done")
	if n := f.deadlineEvidence(t); n != 1 {
		t.Fatalf("the miss did not stay on record: %d", n)
	}
	f.expect(t, "done after the miss", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
}

// A recorded miss under lapses is lapsed: the child stays blocked for a
// decision. A Done that arrives late still wins.
func TestALapsedBlockerLeavesTheChildBlockedAndALateDoneStillWins(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.deadline(t, 0, "lapses")
	acc := f.allEdit()
	g := f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	at := *f.reload(t, f.items[0].ID).NextAt
	f.eval(t, farFuture, 0)
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND state='lapsed'`, f.scheds[0].ID); n != 1 {
		t.Fatalf("setup: %d lapsed occurrences", n)
	}
	f.expect(t, "lapsed blocker", 1, acc, models.GateStateBlockedNeedsDecision, models.GateOutcomeLapsed)
	if f.openedAt(t, g.ID).Valid {
		t.Fatal("a lapsed blocker opened the gate")
	}
	if _, err := f.s.ResolveOccurrence(f.ws.ID, f.items[0].ID, ResolveInput{OccurrenceAt: rt(at), State: "done", Actor: "user"}); err != nil {
		t.Fatalf("late done: %v", err)
	}
	f.expect(t, "late done", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
}

// A miss recorded under lapses with the occurrence not closed (the engine kept
// the miss and left it open) reads as lapsed too.
func TestARecordedMissUnderLapsesIsLapsedEvenWhenTheOccurrenceStayedOpen(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.deadline(t, 0, "lapses")
	acc := f.allEdit()
	f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	key := *f.reload(t, f.items[0].ID).NextAt + "|" + *f.reload(t, f.items[0].ID).NextWall
	f.exec(t, `INSERT INTO schedule_changes (id,workspace_id,item_id,schedule_id,occurrence_key,change_type,role,actor,reason,reason_source,root_change_id,recorded_at)
 VALUES (?,?,?,?,?,'missed_recorded','deadline','system','','system',?,?)`, newID(), f.ws.ID, f.items[0].ID, f.scheds[0].ID, key, "root", nowNano())
	f.expect(t, "miss recorded, occurrence still open", 1, acc, models.GateStateBlockedNeedsDecision, models.GateOutcomeLapsed)
}

func TestASkippedBlockerOccurrenceLeavesTheChildBlockedForADecision(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	acc := f.allEdit()
	g := f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	f.resolve(t, 0, "skip")
	f.expect(t, "skipped blocker", 1, acc, models.GateStateBlockedNeedsDecision, models.GateOutcomeSkipped)
	if f.openedAt(t, g.ID).Valid {
		t.Fatal("a skipped blocker opened the gate")
	}
}

// A passed-over occurrence can no longer succeed.
func TestAPassedOverBlockerOccurrenceLeavesTheChildBlockedForADecision(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	acc := f.allEdit()
	f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	sc := f.reload(t, f.items[0].ID)
	f.exec(t, `INSERT INTO schedule_occurrences (id,schedule_id,occurrence_at,occurrence_wall,state,actor,resolved_at) VALUES (?,?,?,?,'missed','system',?)`,
		newID(), sc.ID, *sc.NextAt, *sc.NextWall, nowNano())
	f.expect(t, "passed over", 1, acc, models.GateStateBlockedNeedsDecision, models.GateOutcomeLapsed)
}

// A move changes the target and not the occurrence's identity, so the gate stays
// held against the same occurrence.
func TestMovingTheBlockerTargetKeepsTheGateOnTheSameOccurrence(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	acc := f.allEdit()
	f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	sc := f.reload(t, f.items[0].ID)
	until := rt("2030-03-06T09:00:00Z")
	if _, err := f.s.resolveOccurrenceAt(f.ws.ID, f.items[0].ID, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "move", Until: &until, Actor: "user"}, rt("2030-03-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "blocker moved", 1, acc, models.GateStateWaiting, models.GateOutcomePending)
	f.resolve(t, 0, "done")
	f.expect(t, "moved blocker done", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
}

// ---- the blocker has no commitment, or none left ----

// buildItem makes an item in a collection whose terminal and abandoned options
// are its own: shipped delivers, discarded closes without delivering.
func (f *linkFix) buildItem(t *testing.T, status string) *models.Item {
	t.Helper()
	cols, err := f.s.ListCollections(f.ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	var col *models.Collection
	for i := range cols {
		if cols[i].Name == "Builds" {
			col = &cols[i]
		}
	}
	if col == nil {
		col, err = f.s.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Builds",
			Schema: `{"fields":[{"key":"status","type":"select","options":["open","shipped","discarded"],"default":"open","terminal_options":["shipped","discarded"],"abandoned_options":["discarded"]}]}`})
		if err != nil {
			t.Fatal(err)
		}
	}
	it, err := f.s.CreateItem(f.ws.ID, col.ID, models.ItemCreate{Title: "build", Fields: `{"status":"` + status + `"}`})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (f *linkFix) setBlockerStatus(t *testing.T, it *models.Item, status string) {
	t.Helper()
	fields := `{"status":"` + status + `"}`
	if _, err := f.s.UpdateItem(it.ID, models.ItemUpdate{Fields: &fields}); err != nil {
		t.Fatal(err)
	}
}

func TestABlockerWithNoCommitmentSucceedsOnATerminalStatusThatIsNotAbandoned(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	build := f.buildItem(t, "open")
	acc := f.gateAccess(build)
	g := f.gate(t, f.items[0].ID, build.ID, acc)
	f.expect(t, "open build", 0, acc, models.GateStateWaiting, models.GateOutcomePending)
	f.setBlockerStatus(t, build, "shipped")
	f.expect(t, "shipped build", 0, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
	if !f.openedAt(t, g.ID).Valid {
		t.Fatal("the gate did not record that it opened")
	}
	// Opens once: a build that is reopened afterwards does not re-block.
	f.setBlockerStatus(t, build, "open")
	f.expect(t, "reopened build", 0, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
}

func TestAnAbandonedBlockerStatusLeavesTheChildBlockedForADecision(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	build := f.buildItem(t, "open")
	acc := f.gateAccess(build)
	g := f.gate(t, f.items[0].ID, build.ID, acc)
	f.setBlockerStatus(t, build, "discarded")
	f.expect(t, "discarded build", 0, acc, models.GateStateBlockedNeedsDecision, models.GateOutcomeCancelled)
	if f.openedAt(t, g.ID).Valid {
		t.Fatal("an abandoned blocker opened the gate")
	}
}

// With no declared options the workspace default terminal and abandoned lists
// decide, as everywhere else.
func TestABlockerStatusUsesTheDefaultOptionsWhenTheCollectionDeclaresNone(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	plain := f.another(t, "plain build")
	acc := f.gateAccess(plain)
	f.gate(t, f.items[0].ID, plain.ID, acc)
	f.expect(t, "open", 0, acc, models.GateStateWaiting, models.GateOutcomePending)
	f.setBlockerStatus(t, plain, "cancelled")
	f.expect(t, "cancelled by default", 0, acc, models.GateStateBlockedNeedsDecision, models.GateOutcomeCancelled)
	other := f.another(t, "other build")
	acc = f.gateAccess(other)
	f.gate(t, f.items[1].ID, other.ID, acc)
	f.setBlockerStatus(t, other, "done")
	f.expect(t, "done by default", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
}

// A blocker whose commitment had already ended when the gate was written has no
// outstanding occurrence to fix: its status decides, as for one with no commitment.
func TestABlockerWhoseCommitmentHadEndedIsJudgedByItsStatus(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		status, state, outcome string
	}{
		{"open", models.GateStateWaiting, models.GateOutcomePending},
		{"shipped", models.GateStateReleased, models.GateOutcomeSucceeded},
		{"discarded", models.GateStateBlockedNeedsDecision, models.GateOutcomeCancelled},
	} {
		c := c
		t.Run(c.status, func(t *testing.T) {
			t.Parallel()
			f := newLinkFix(t, 1)
			build := f.buildItem(t, c.status)
			f.native(t, build, "2030-03-09T09:00", models.CommitmentRolesInput{})
			bsc := f.reload(t, build.ID)
			// A one-off already done: its schedule has ended.
			f.resolve2(t, build, bsc, "done")
			if got := f.reload(t, build.ID); got.EndedAt == nil {
				t.Fatalf("setup: the blocker schedule did not end: %+v", got)
			}
			acc := f.gateAccess(build)
			f.gate(t, f.items[0].ID, build.ID, acc)
			f.expect(t, "ended blocker, status "+c.status, 0, acc, c.state, c.outcome)
		})
	}
}

func (f *linkFix) resolve2(t *testing.T, it *models.Item, sc *models.ItemSchedule, state string) {
	t.Helper()
	if _, err := f.s.ResolveOccurrence(f.ws.ID, it.ID, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: state, Actor: "user", EndSchedule: true}); err != nil {
		t.Fatal(err)
	}
}

// A blocker removed from the workspace cannot succeed: the child stays blocked
// for a decision, and a restore brings the blocker back as it was.
func TestASoftDeletedBlockerLeavesTheChildBlockedForADecision(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	build := f.buildItem(t, "open")
	acc := f.gateAccess(build)
	g := f.gate(t, f.items[0].ID, build.ID, acc)
	if err := f.s.DeleteItem(build.ID); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "deleted blocker", 0, acc, models.GateStateBlockedNeedsDecision, models.GateOutcomeCancelled)
	if f.openedAt(t, g.ID).Valid {
		t.Fatal("a deleted blocker opened the gate")
	}
}

// ---- who sees what ----

func TestAGateWithAnUnreadableSucceededBlockerShowsNothingAndStillOpens(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	hidden := f.buildItem(t, "open")
	all := f.gateAccess(hidden)
	g := f.gate(t, f.items[0].ID, hidden.ID, all)
	f.setBlockerStatus(t, hidden, "shipped") // the hidden blocker succeeded

	reader := f.allEdit() // reads the gated item, not the blocker
	got := f.gates(t, 0, reader)
	noGate := f.gates(t, 2, reader) // an item with no gate at all
	if !reflect.DeepEqual(got, noGate) {
		t.Fatalf("a released gate with a hidden blocker shows: %+v, want what an ungated item shows: %+v", got, noGate)
	}
	b, _ := json.Marshal(got)
	for _, id := range []string{hidden.ID, g.ID} {
		if strings.Contains(string(b), id) {
			t.Fatalf("the view names %s: %s", id, b)
		}
	}
	// Opening does not depend on who reads: the blocker succeeded.
	if !f.openedAt(t, g.ID).Valid {
		t.Fatal("the blocker write did not record the gate opening")
	}
	// A reader who sees both ends gets the state and the detail.
	f.expect(t, "reader of both ends", 0, all, models.GateStateReleased, models.GateOutcomeSucceeded)
	if v := f.gates(t, 0, all); len(v.Gates) != 1 || v.Gates[0].BlockerItemID != hidden.ID {
		t.Fatalf("view=%+v", v)
	}
}

// The state covers every gate, the detail only the gates whose blocker the reader
// can read: a hidden pending blocker holds the child for everyone.
func TestAGateStateCoversEveryGateAndTheDetailOnlyTheReadableOnes(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	hidden := f.another(t, "hidden blocker")
	all := f.gateAccess(hidden)
	f.gate(t, f.items[0].ID, hidden.ID, all)     // pending, hidden from the reader below
	f.gate(t, f.items[0].ID, f.items[1].ID, all) // visible
	f.resolve(t, 1, "done")
	reader := f.allEdit()
	v := f.gates(t, 0, reader)
	if v.State != models.GateStateWaiting || len(v.Gates) != 1 || len(v.Unresolved) != 0 {
		t.Fatalf("reader without the hidden blocker: %+v", v)
	}
	full := f.gates(t, 0, all)
	if full.State != models.GateStateWaiting || len(full.Gates) != 2 || !reflect.DeepEqual(full.Unresolved, []string{hidden.ID}) {
		t.Fatalf("reader of every end: %+v", full)
	}
}

func TestAGateViewOfAnUnreadableItemIsRefused(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.gate(t, f.items[0].ID, f.items[1].ID, f.allEdit())
	acc := f.allEdit()
	delete(acc.Readable, f.items[0].ID)
	if _, err := f.s.ScheduleGates(f.ws.ID, f.items[0].ID, acc); err != ErrScheduleNotFound {
		t.Fatalf("err=%v, want ErrScheduleNotFound", err)
	}
}

// Every gate must succeed; one that ended without success outranks one still pending.
func TestMultipleGatesAllMustSucceed(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	acc := f.allEdit()
	f.gate(t, f.items[0].ID, f.items[1].ID, acc)
	f.gate(t, f.items[0].ID, f.items[2].ID, acc)
	v := f.gates(t, 0, acc)
	if v.State != models.GateStateWaiting || len(v.Unresolved) != 2 {
		t.Fatalf("two pending: %+v", v)
	}
	f.resolve(t, 1, "done")
	v = f.gates(t, 0, acc)
	if v.State != models.GateStateWaiting || !reflect.DeepEqual(v.Unresolved, []string{f.items[2].ID}) {
		t.Fatalf("one done, one pending: %+v", v)
	}
	f.resolve(t, 2, "skip")
	if v = f.gates(t, 0, acc); v.State != models.GateStateBlockedNeedsDecision {
		t.Fatalf("one done, one skipped: %+v", v)
	}
}

func TestGateReadsWorkWithOneConnection(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	build := f.buildItem(t, "shipped")
	acc := f.gateAccess(build)
	f.gate(t, f.items[0].ID, build.ID, acc)
	f.gate(t, f.items[0].ID, f.items[1].ID, acc)
	f.s.db.SetMaxOpenConns(1)
	f.expect(t, "one connection", 0, acc, models.GateStateWaiting, "")
}

// A plain blocks link means what it always meant: a commitment gate neither
// reads nor writes item links, and an item link is no gate.
func TestAPlainBlocksLinkIsUnchangedByGates(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	build := f.buildItem(t, "discarded") // closed without delivering
	if _, err := f.s.CreateItemLink(f.ws.ID, models.ItemLinkCreate{TargetID: f.items[0].ID, LinkType: "blocks"}, build.ID); err != nil {
		t.Fatal(err)
	}
	edgesBefore, err := f.s.GetBlocksEdges(f.ws.ID)
	if err != nil || len(edgesBefore) != 1 {
		t.Fatalf("edges=%v %v", edgesBefore, err)
	}
	if n := f.gateRows(t); n != 0 {
		t.Fatalf("an item link made %d gates", n)
	}
	acc := f.gateAccess(build)
	g := f.gate(t, f.items[0].ID, build.ID, acc)
	f.expect(t, "gate on the same pair", 0, acc, models.GateStateBlockedNeedsDecision, models.GateOutcomeCancelled)
	edgesAfter, err := f.s.GetBlocksEdges(f.ws.ID)
	if err != nil || !reflect.DeepEqual(edgesBefore, edgesAfter) {
		t.Fatalf("a gate changed the blocks edges: %+v -> %+v (%v)", edgesBefore, edgesAfter, err)
	}
	if err := f.s.DeleteScheduleGate(f.ws.ID, g.ID, acc); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_links WHERE link_type='blocks'`); n != 1 {
		t.Fatalf("%d blocks links after the gate was deleted, want 1", n)
	}
}

// ---- an abandoned blocker item ----

// buildWithCommitment is a build item (shipped delivers, discarded abandons) with
// a commitment of its own.
func (f *linkFix) buildWithCommitment(t *testing.T) *models.Item {
	t.Helper()
	build := f.buildItem(t, "open")
	f.native(t, build, "2030-03-09T09:00", models.CommitmentRolesInput{})
	return build
}

// An abandoned blocker item can no longer deliver: it leaves the child blocked
// for a decision even though its commitment's fixed occurrence is still pending.
// A terminal status that is not abandoned does not release the gate by itself:
// the occurrence decides.
func TestAnAbandonedBlockerItemLeavesTheChildBlockedWhileItsOccurrenceIsPending(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	build := f.buildWithCommitment(t)
	acc := f.gateAccess(build)
	g := f.gate(t, f.items[0].ID, build.ID, acc)
	f.expect(t, "open build", 0, acc, models.GateStateWaiting, models.GateOutcomePending)
	f.setBlockerStatus(t, build, "shipped")
	f.expect(t, "shipped, occurrence still pending", 0, acc, models.GateStateWaiting, models.GateOutcomePending)
	f.setBlockerStatus(t, build, "discarded")
	f.expect(t, "discarded, occurrence still pending", 0, acc, models.GateStateBlockedNeedsDecision, models.GateOutcomeCancelled)
	if f.openedAt(t, g.ID).Valid {
		t.Fatal("an abandoned blocker opened the gate")
	}
}

// A recorded `done` occurrence is a stored fact with its own actual time, and a
// gate opens once: abandoning the item afterwards (or before the gate was read)
// does not undo it.
func TestADoneOccurrenceStillWinsOverAnAbandonedBlockerItem(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	build := f.buildWithCommitment(t)
	acc := f.gateAccess(build)
	opened := f.gate(t, f.items[0].ID, build.ID, acc)
	f.resolve2(t, build, f.reload(t, build.ID), "done")
	f.setBlockerStatus(t, build, "discarded")
	f.expect(t, "done, then abandoned, gate never read", 0, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
	if !f.openedAt(t, opened.ID).Valid {
		t.Fatal("the gate did not open")
	}
	// And a gate that was already open stays open.
	other := f.buildWithCommitment(t)
	acc = f.gateAccess(build, other)
	g := f.gate(t, f.items[1].ID, other.ID, acc)
	f.resolve2(t, other, f.reload(t, other.ID), "done")
	f.expect(t, "done", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
	f.setBlockerStatus(t, other, "discarded")
	f.expect(t, "abandoned after the gate opened", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
	if !f.openedAt(t, g.ID).Valid {
		t.Fatal("the gate did not stay open")
	}
}

// A skipped occurrence is already blocked; abandoning does not change what it reads.
func TestAnAbandonedBlockerItemWithASkippedOccurrenceStaysSkipped(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	build := f.buildWithCommitment(t)
	acc := f.gateAccess(build)
	f.gate(t, f.items[0].ID, build.ID, acc)
	f.resolve2(t, build, f.reload(t, build.ID), "skip")
	f.setBlockerStatus(t, build, "discarded")
	f.expect(t, "skipped and abandoned", 0, acc, models.GateStateBlockedNeedsDecision, models.GateOutcomeSkipped)
}
