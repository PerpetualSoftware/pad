package store

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Postgres lock-order tests for the linked move (TASK-4353, 5b). Red first: the
// naive pin (every item pinned by pinScheduleTx, root first) deadlocks in both
// shapes below, and the real pinCommitmentTx does not.
//
// The harness holds the workspace row from a side transaction, queues the
// transactions under test behind it (each already holding its root item), checks
// in pg_stat_activity that they are all waiting, and releases the workspace. That
// is the interleaving a lock-order inversion needs and the one a sleep would only
// hit by luck.

type linkPG struct {
	s     *Store
	ws    *models.Workspace
	items []*models.Item
	scs   []*models.ItemSchedule
}

func newLinkPG(t *testing.T, n int) *linkPG {
	t.Helper()
	url := os.Getenv("PAD_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set: lock order is a Postgres property")
	}
	s := testStorePostgres(t, url)
	ws := createTestWorkspace(t, s, "LinkPG")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	f := &linkPG{s: s, ws: ws}
	for i := 0; i < n; i++ {
		it := createTestItem(t, s, ws.ID, col.ID, "pg item", "")
		sc, err := s.CreateCommitmentSchedule(ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2030-03-04T09:00", Tz: "UTC"}, models.CommitmentRolesInput{})
		if err != nil {
			t.Fatal(err)
		}
		f.items, f.scs = append(f.items, it), append(f.scs, sc)
	}
	return f
}

func (f *linkPG) access() models.CommitmentAccess {
	a := models.CommitmentAccess{Readable: map[string]bool{}, Editable: map[string]bool{}}
	for _, it := range f.items {
		a.Readable[it.ID], a.Editable[it.ID] = true, true
	}
	return a
}

func (f *linkPG) link(t *testing.T, parent, child int) {
	t.Helper()
	if _, err := f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[parent].ID, ParentRole: "target", ChildItemID: f.items[child].ID, ChildRole: "target", Offset: oneDay}, f.access()); err != nil {
		t.Fatal(err)
	}
}

func (f *linkPG) move(i int, to string) error {
	_, err := f.s.MoveLinkedDate(f.ws.ID, models.LinkedMoveInput{ItemID: f.items[i].ID, Role: "target", To: to, Actor: "user", Access: f.access()})
	return err
}

// holdWorkspace locks the workspace row from a side transaction until release.
func (f *linkPG) holdWorkspace(t *testing.T) (release func()) {
	t.Helper()
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := tx.QueryRow(`SELECT id FROM workspaces WHERE id=$1 FOR NO KEY UPDATE`, f.ws.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return func() { tx.Rollback() }
}

// waitForWaiters waits until n backends of this database wait on a lock.
func (f *linkPG) waitForWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got int
		if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND datname = current_database() AND state = 'active' AND wait_event_type = 'Lock'`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d backends waiting on a lock, want %d", got, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// naivePin is the per-item pin a cascade must NOT use: each item through
// pinScheduleTx (item, workspace, schedule), the root first.
func (f *linkPG) naivePin(items ...string) error {
	tx, err := f.s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range items {
		if err := f.s.pinScheduleTx(tx, f.ws.ID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// run starts fns one by one, waiting for each to be queued on the held workspace, then releases.
func (f *linkPG) run(t *testing.T, fns ...func() error) []error {
	t.Helper()
	release := f.holdWorkspace(t)
	defer release()
	errs := make([]error, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = fn() }()
		f.waitForWaiters(t, i+1)
	}
	release()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the transactions did not finish: a lock wait that never ends")
	}
	return errs
}

func deadlocked(errs []error) int {
	n := 0
	for _, err := range errs {
		var d bool
		if err != nil {
			d = isDeadlockError(err)
		}
		if d {
			n++
		}
	}
	return n
}

func TestNaiveRootFirstPinDeadlocksTwoCrossingCascades(t *testing.T) {
	f := newLinkPG(t, 2)
	// Cascade X roots at 0 and reaches 1; cascade Y roots at 1 and reaches 0.
	errs := f.run(t,
		func() error { return f.naivePin(f.items[0].ID, f.items[1].ID) },
		func() error { return f.naivePin(f.items[1].ID, f.items[0].ID) })
	if deadlocked(errs) != 1 {
		t.Fatalf("errs=%v, want exactly one deadlock: the harness must catch the naive pin", errs)
	}
}

func TestNaiveChildPinDeadlocksAgainstAResolveOfThatChild(t *testing.T) {
	f := newLinkPG(t, 2)
	sc := f.scs[1]
	errs := f.run(t,
		func() error { return f.naivePin(f.items[0].ID, f.items[1].ID) },
		func() error {
			_, err := f.s.ResolveOccurrence(f.ws.ID, f.items[1].ID, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"})
			return err
		})
	if deadlocked(errs) != 1 {
		t.Fatalf("errs=%v, want exactly one deadlock", errs)
	}
}

func TestTwoCrossingCascadesDoNotDeadlock(t *testing.T) {
	f := newLinkPG(t, 3)
	f.link(t, 0, 1)
	f.link(t, 1, 2)
	errs := f.run(t,
		func() error { return f.move(0, "2030-03-10T09:00:00Z") },
		func() error { return f.move(1, "2030-03-20T09:00:00Z") })
	for _, err := range errs {
		if err != nil {
			t.Fatalf("errs=%v", errs)
		}
	}
	c, err := f.s.GetCommitment(f.ws.ID, f.items[2].ID)
	if err != nil || c.Roles.Target == nil {
		t.Fatalf("%v", err)
	}
	b, _ := f.s.GetCommitment(f.ws.ID, f.items[1].ID)
	if *c.Roles.Target != "2030-03-21T09:00:00Z" || *b.Roles.Target != "2030-03-20T09:00:00Z" {
		t.Fatalf("b=%s c=%s: the later cascade must leave the chain consistent", *b.Roles.Target, *c.Roles.Target)
	}
}

func TestCascadeAgainstAResolveOfOneOfItsChildrenDoesNotDeadlock(t *testing.T) {
	f := newLinkPG(t, 2)
	f.link(t, 0, 1)
	sc := f.scs[1]
	errs := f.run(t,
		func() error { return f.move(0, "2030-03-10T09:00:00Z") },
		func() error {
			_, err := f.s.ResolveOccurrence(f.ws.ID, f.items[1].ID, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"})
			return err
		})
	for _, err := range errs {
		if err != nil {
			t.Fatalf("errs=%v", errs)
		}
	}
	if deadlocked(errs) != 0 {
		t.Fatal("deadlock")
	}
}

// A link write takes the same pin as a cascade, so two writes that point at each
// other cannot deadlock: one stores its link, the other is refused as a cycle.
func TestTwoCrossingLinkWritesDoNotDeadlock(t *testing.T) {
	f := newLinkPG(t, 2)
	write := func(parent, child int) func() error {
		return func() error {
			_, err := f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[parent].ID, ParentRole: "target", ChildItemID: f.items[child].ID, ChildRole: "target", Offset: oneDay}, f.access())
			return err
		}
	}
	errs := f.run(t, write(0, 1), write(1, 0))
	if deadlocked(errs) != 0 {
		t.Fatalf("errs=%v", errs)
	}
	refused := 0
	for _, err := range errs {
		var lr *LinkRefusedError
		if errors.As(err, &lr) && lr.Code == "link_cycle" {
			refused++
		} else if err != nil {
			t.Fatalf("errs=%v", errs)
		}
	}
	if refused != 1 {
		t.Fatalf("errs=%v, want one link stored and one refused as a cycle", errs)
	}
}

// A link write moves its child while a resolve of that child waits for the
// workspace: the write never asks for the child's item row.
func TestLinkWriteAgainstAResolveOfItsChildDoesNotDeadlock(t *testing.T) {
	f := newLinkPG(t, 2)
	sc := f.scs[1]
	errs := f.run(t,
		func() error {
			_, err := f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[0].ID, ParentRole: "target", ChildItemID: f.items[1].ID, ChildRole: "target", Offset: oneDay}, f.access())
			return err
		},
		func() error {
			_, err := f.s.ResolveOccurrence(f.ws.ID, f.items[1].ID, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"})
			return err
		})
	for _, err := range errs {
		if err != nil {
			t.Fatalf("errs=%v", errs)
		}
	}
}

// A link delete whose root item is soft-deleted still takes the root item
// first, then the workspace, and never asks for another item row, so it queues
// with a cascade rooted at the live end and neither deadlocks.
func TestLinkDeleteWithASoftDeletedRootAgainstACascadeDoesNotDeadlock(t *testing.T) {
	f := newLinkPG(t, 3)
	f.link(t, 0, 1)
	f.link(t, 1, 2)
	var id string
	if err := f.s.db.QueryRow(`SELECT id FROM schedule_links WHERE parent_schedule_id=$1`, f.scs[0].ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteItem(f.items[0].ID); err != nil {
		t.Fatal(err)
	}
	errs := f.run(t,
		func() error { return f.s.DeleteScheduleLink(f.ws.ID, id, f.access()) },
		func() error { return f.move(1, "2030-03-20T09:00:00Z") })
	for _, err := range errs {
		if err != nil {
			t.Fatalf("errs=%v", errs)
		}
	}
	var n int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM schedule_links`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("links=%d %v", n, err)
	}
}

// beginMoveOf is a cross-workspace move of item i that has not committed: it
// holds the item's schedule row lock and has archived the item, ended the
// schedule and deleted its links. The move does not take the workspace row, so
// a link write that has passed its own pin queues behind it on the schedule lock
// and resumes after the commit, with everything it read before the lock stale.
func (f *linkPG) beginMoveOf(t *testing.T, i int) (commit func()) {
	t.Helper()
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := tx.QueryRow(`SELECT id FROM item_schedules WHERE id=$1 FOR NO KEY UPDATE`, f.scs[i].ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, st := range []struct {
		q    string
		args []any
	}{
		{`DELETE FROM schedule_links WHERE parent_schedule_id=$1 OR child_schedule_id=$1`, []any{f.scs[i].ID}},
		{`UPDATE item_schedules SET ended_at=$1, next_at=NULL, next_wall=NULL WHERE id=$2`, []any{nowNano(), f.scs[i].ID}},
		{`UPDATE items SET deleted_at=$1 WHERE id=$2`, []any{nowNano(), f.items[i].ID}},
	} {
		if _, err := tx.Exec(st.q, st.args...); err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		if err := tx.Commit(); err != nil {
			t.Error(err)
		}
	}
}

// queueBehindMove runs fn once it is waiting on the move's lock, then commits the move.
func (f *linkPG) queueBehindMove(t *testing.T, commit func(), fn func() error) error {
	t.Helper()
	res := make(chan error, 1)
	go func() { res <- fn() }()
	f.waitForWaiters(t, 1)
	commit()
	select {
	case err := <-res:
		return err
	case <-time.After(60 * time.Second):
		t.Fatal("the write never resumed")
		return nil
	}
}

func TestLinkCreateQueuedBehindAMoveOfItsChildWritesNothing(t *testing.T) {
	f := newLinkPG(t, 2)
	commit := f.beginMoveOf(t, 1)
	err := f.queueBehindMove(t, commit, func() error {
		_, err := f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[0].ID, ParentRole: "target", ChildItemID: f.items[1].ID, ChildRole: "target", Offset: oneDay}, f.access())
		return err
	})
	if err != ErrScheduleLinkNotFound {
		t.Fatalf("err=%v, want the ErrScheduleLinkNotFound value", err)
	}
	var n int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM schedule_links`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("links=%d %v", n, err)
	}
}

func TestLinkUpdateAndDeleteQueuedBehindAMoveOfTheChildAreNotFound(t *testing.T) {
	for _, op := range []string{"update", "delete"} {
		op := op
		t.Run(op, func(t *testing.T) {
			f := newLinkPG(t, 2)
			f.link(t, 0, 1)
			var id string
			if err := f.s.db.QueryRow(`SELECT id FROM schedule_links`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			commit := f.beginMoveOf(t, 1)
			err := f.queueBehindMove(t, commit, func() error {
				if op == "delete" {
					return f.s.DeleteScheduleLink(f.ws.ID, id, f.access())
				}
				_, err := f.s.UpdateScheduleLink(f.ws.ID, id, models.ScheduleLinkUpdate{CancelPolicy: sp(models.LinkCancelChildOccurrence)}, f.access())
				return err
			})
			if err != ErrScheduleLinkNotFound {
				t.Fatalf("%s: err=%v, want the ErrScheduleLinkNotFound value", op, err)
			}
		})
	}
}
