package server

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/notify"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4032: a push the gateway accepted whose 'sent' record then fails must
// not be sent again every lease. The fault is a test-only DB trigger that
// aborts UPDATEs setting push_state='sent' (claim, skip, release and expire
// still work); there is no production seam.

// failSent installs (on) or drops (off) the trigger.
func (f *pdx) failSent(on bool) {
	f.t.Helper()
	switch {
	case on && f.pg():
		f.exec(`CREATE OR REPLACE FUNCTION pd_fail_sent() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'pd fault'; END $$`)
		f.exec(`CREATE TRIGGER pd_fail BEFORE UPDATE ON user_notifications FOR EACH ROW WHEN (NEW.push_state = 'sent') EXECUTE FUNCTION pd_fail_sent()`)
	case on:
		f.exec(`CREATE TRIGGER pd_fail BEFORE UPDATE ON user_notifications WHEN NEW.push_state = 'sent' BEGIN SELECT RAISE(ABORT, 'pd fault'); END`)
	case f.pg():
		f.exec(`DROP TRIGGER pd_fail ON user_notifications`)
	default:
		f.exec(`DROP TRIGGER pd_fail`)
	}
}

func (f *pdx) parkedCount() int {
	d := &f.srv.pushDispatch
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.unrecorded)
}

// tickLeases runs n ticks, each a full lease (plus a second) after the last.
func (f *pdx) tickLeases(clk *pdClock, n int) {
	for i := 0; i < n; i++ {
		clk.advance(defaultPushLease + time.Second)
		f.tick()
	}
}

func TestPushDispatch_RecordFailureBoundsResends(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment")
		clk := f.clock(time.Now())
		f.failSent(true)
		f.tick()
		f.tickLeases(clk, 3)
		if n := f.fake.callCount(); n != 1 {
			t.Fatalf("%d Send calls over 3 lapsed leases, want 1", n)
		}
		if st := f.state(id); st.State != "pending" {
			t.Fatalf("row = %+v, want pending while the record fails", st)
		}
	})
}

func TestPushDispatch_RecordFailureRecovers(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment")
		clk := f.clock(time.Now())
		f.failSent(true)
		f.tick()
		f.tickLeases(clk, 1)
		f.failSent(false)
		f.tick()
		if st := f.state(id); st.State != "sent" || st.SentAt == "" || st.Claim != "" {
			t.Fatalf("row = %+v, want sent, stamped, unclaimed", st)
		}
		if n := f.fake.callCount(); n != 1 {
			t.Fatalf("%d Send calls, want 1: recovery must not re-send", n)
		}
		if f.parkedCount() != 0 {
			t.Fatal("the recorded row is still parked")
		}
	})
}

// With no gateway configured the tick still records what it delivered.
func TestPushDispatch_RecordRetriedWithoutAGateway(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment")
		f.clock(time.Now())
		f.failSent(true)
		f.tick()
		f.failSent(false)
		srv.SetPushSender(nil)
		f.tick()
		if st := f.state(id); st.State != "sent" {
			t.Fatalf("row = %+v, want sent with no gateway", st)
		}
	})
}

// The parked set is bounded by the notification's expiry: an outage cannot
// grow it, and an expired row is never sent again.
func TestPushDispatch_ExpiredParkedEntriesAreDropped(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		var ids []string
		for i := 0; i < 3; i++ {
			ids = append(ids, f.row("comment"))
		}
		clk := f.clock(time.Now())
		f.failSent(true)
		f.tick()
		if n := f.parkedCount(); n != 3 {
			t.Fatalf("%d parked after the failed record, want 3", n)
		}
		clk.advance(pushExpireAfter + time.Minute)
		f.tick()
		if n := f.parkedCount(); n != 0 {
			t.Fatalf("%d entries parked past the expiry age, want 0", n)
		}
		f.failSent(false)
		clk.advance(defaultPushLease + time.Second)
		f.tick()
		for _, id := range ids {
			if st := f.state(id); st.State != "expired" {
				t.Fatalf("row = %+v, want expired and left so", st)
			}
		}
		if n := f.fake.callCount(); n != 1 {
			t.Fatalf("%d Send calls, want 1: an expired row must not be sent again", n)
		}
	})
}

func TestPushDispatch_RecordFailureMixedChunkStillSettles(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		itemR := f.newItem("R", &f.bob.ID)
		itemT := f.newItem("T", &f.bob.ID)
		sent := f.row("comment")
		rejected := f.row("assigned", func(o *rowOpt) { o.item = itemR })
		retried := f.row("assigned", func(o *rowOpt) { o.item = itemT })
		f.fake.fn = func(_ context.Context, msgs []notify.Message) ([]notify.Result, error) {
			out := allStatus(msgs, notify.StatusSent)
			for i, m := range msgs {
				switch m.Data.NID {
				case rejected:
					out[i].Status = notify.StatusRejected
				case retried:
					out[i].Status = notify.StatusRetry
				}
			}
			return out, nil
		}
		f.clock(time.Now())
		f.failSent(true)
		f.tick()
		if st := f.state(rejected); st.State != "skipped" || st.Reason != skipGatewayRejected {
			t.Fatalf("rejected row = %+v, want skipped/gateway_rejected", st)
		}
		if st := f.state(retried); st.State != "pending" || st.Attempts != 1 || st.Claim != "" {
			t.Fatalf("retried row = %+v, want pending, 1 attempt, released", st)
		}
		if st := f.state(sent); st.State != "pending" || f.parkedCount() != 1 {
			t.Fatalf("sent row = %+v with %d parked, want pending and parked", st, f.parkedCount())
		}
		f.failSent(false)
		f.tick()
		if st := f.state(sent); st.State != "sent" {
			t.Fatalf("sent row = %+v after recovery, want sent", st)
		}
	})
}

// capture routes slog to a buffer for the test.
func captureTask4032Logs(t *testing.T) *syncBuffer {
	t.Helper()
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// cleanItem is an item assigned to bob whose assignment row (enqueued once a
// device exists) is deleted, so a test controls every row on it.
func (f *pdx) cleanItem(title string) *models.Item {
	f.t.Helper()
	it := f.newItem(title, &f.bob.ID)
	f.exec(`DELETE FROM user_notifications WHERE item_id = ?`, it.ID)
	return it
}

// freshRows gives bob n new single-row groups, each on its own clean item.
func (f *pdx) freshRows(n int) []string {
	f.t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		it := f.cleanItem(fmt.Sprintf("Fresh %d", i))
		ids = append(ids, f.row("assigned", func(o *rowOpt) { o.item = it; o.age = time.Minute }))
	}
	return ids
}

func (f *pdx) setUnrecordedCap(n int) {
	d := &f.srv.pushDispatch
	d.mu.Lock()
	d.unrecordedCapOverride = n
	d.mu.Unlock()
}

// While the parked set is at its cap a tick claims and sends nothing, so no
// parked row is ever re-sent (nothing is evicted), and the full state logs
// once on entry and once on exit. Recording the rows drains the set and
// sending resumes.
func TestPushDispatch_FullUnrecordedSetHoldsClaimsAndSends(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		logs := captureTask4032Logs(t)
		f := newPdx(t, srv)
		f.setUnrecordedCap(2)
		// Three rows on one item are one push; parking all three overshoots the
		// cap of 2, so an evicting implementation would free one for a re-send.
		var parked []string
		for i := 0; i < 3; i++ {
			parked = append(parked, f.row("comment"))
		}
		clk := f.clock(time.Now())
		f.failSent(true)
		f.tick()
		fresh := f.freshRows(1)[0]
		f.tickLeases(clk, 4)
		// The re-send check comes first so an evicting implementation fails
		// on the re-send itself, not on the parked count.
		if n := f.fake.callCount(); n != 1 {
			t.Fatalf("%d Send calls while the set was full, want 1", n)
		}
		if n := f.parkedCount(); n != 3 {
			t.Fatalf("%d parked, want 3 (the set is never trimmed)", n)
		}
		if st := f.state(fresh); st.State != "pending" || st.Claim != "" || st.Attempts != 0 {
			t.Fatalf("fresh row = %+v, want pending, never claimed, held", st)
		}
		for _, id := range parked {
			if st := f.state(id); st.State != "pending" {
				t.Fatalf("parked row = %+v, want pending", st)
			}
		}
		if n := strings.Count(logs.String(), "at the cap, holding"); n != 1 {
			t.Fatalf("%d entering warnings over several full ticks, want 1:\n%s", n, logs.String())
		}
		if strings.Contains(logs.String(), "resuming") {
			t.Fatal("left the full state while still full")
		}

		f.failSent(false)
		f.tick()
		if n := f.parkedCount(); n != 0 {
			t.Fatalf("%d parked after the record landed, want 0", n)
		}
		for _, id := range append(parked, fresh) {
			if st := f.state(id); st.State != "sent" {
				t.Fatalf("row %s = %+v after recovery, want sent", id, st)
			}
		}
		if n := f.fake.callCount(); n != 2 {
			t.Fatalf("%d Send calls, want 2: the parked push once, the fresh row once", n)
		}
		f.tick()
		if n := strings.Count(logs.String(), "below the cap, resuming"); n != 1 {
			t.Fatalf("%d leaving warnings, want 1:\n%s", n, logs.String())
		}
		if n := strings.Count(logs.String(), "at the cap, holding"); n != 1 {
			t.Fatalf("%d entering warnings after the drain, want 1", n)
		}
	})
}

// Parked rows are never claimed: with more of them than one claim batch, a
// fresh pending row is still claimed and sent.
func TestPushDispatch_ParkedRowsDoNotStarveTheClaimBatch(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		var ids []string
		for i := 0; i < notify.MaxBatch+50; i++ {
			ids = append(ids, f.row("comment", func(o *rowOpt) { o.age = 5 * time.Minute }))
		}
		// Parked directly (no user, so the 30/h cap is not involved); the
		// trigger keeps the tick's record retry from clearing them.
		f.clock(time.Now())
		f.failSent(true)
		srv.parkUnrecorded(ids, nil, time.Now())
		fresh := f.freshRows(1)[0]
		f.tick()
		msgs := f.fake.all()
		if len(msgs) != 1 || msgs[0].Data.NID != fresh {
			t.Fatalf("pushes = %+v, want exactly one, for the fresh row %s", msgs, fresh)
		}
		if st := f.state(fresh); st.State != "pending" || f.parkedCount() != len(ids)+1 {
			t.Fatalf("fresh row = %+v with %d parked, want pending (record failing) and parked", st, f.parkedCount())
		}
	})
}

// Parked rows are not part of the summary: it counts only the new rows. They
// still count toward the 30/h cap.
func TestPushDispatch_SummaryCountsOnlyNewRowsParkedStillCap(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		f.row("comment")
		f.row("comment")
		clk := f.clock(time.Now())
		f.failSent(true)
		f.tick()
		if n := f.parkedCount(); n != 2 {
			t.Fatalf("%d parked, want 2", n)
		}
		fresh := f.freshRows(pushSummaryAbove + 1)
		f.tickLeases(clk, 1)
		msgs := f.fake.all()
		if len(msgs) != 2 {
			t.Fatalf("%d messages, want the first push and one summary", len(msgs))
		}
		if m := msgs[1]; m.Data.Kind != notify.KindSummary || m.Data.Count != len(fresh) {
			t.Fatalf("second message = %+v, want a summary of %d new rows (not the parked)", m, len(fresh))
		}
	})
}

func TestPushDispatch_ParkedRowsCountTowardTheCapForASummary(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		for i := 0; i < pushRateCap; i++ {
			f.row("comment")
		}
		clk := f.clock(time.Now())
		f.failSent(true)
		f.tick()
		fresh := f.freshRows(pushSummaryAbove + 1)
		f.tickLeases(clk, 1)
		for _, id := range fresh {
			if st := f.state(id); st.State != "skipped" || st.Reason != skipRateLimited {
				t.Fatalf("row = %+v, want rate_limited behind 30 parked rows", st)
			}
		}
		if n := f.fake.callCount(); n != 1 {
			t.Fatalf("%d Send calls, want 1: no summary past the cap", n)
		}
	})
}

// A parked row on an item must not pull a sibling row into its own push.
func TestPushDispatch_ParkedRowDoesNotRideWithASibling(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		r1 := f.row("comment", func(o *rowOpt) { o.age = 2 * time.Minute })
		clk := f.clock(time.Now())
		f.failSent(true)
		f.tick()
		// Inserted later but created earlier, and r1 is the newer row: were r1
		// grouped with it, the push would carry r1's id.
		r2 := f.row("comment", func(o *rowOpt) { o.age = 10 * time.Minute })
		clk.advance(defaultPushLease + time.Second)
		f.tick()
		if n := f.fake.callCount(); n != 2 {
			t.Fatalf("%d Send calls, want 2 (r1 once, r2 once)", n)
		}
		msgs := f.fake.all()
		if len(msgs) != 2 || msgs[1].Data.NID != r2 {
			t.Fatalf("second push = %+v, want a single push for %s (not %s)", msgs, r2, r1)
		}
	})
}

// Delivered-but-unrecorded rows count toward the user's 30/h cap.
func TestPushDispatch_ParkedRowsCountTowardTheCap(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		for i := 0; i < pushRateCap; i++ {
			f.row("comment")
		}
		clk := f.clock(time.Now())
		f.failSent(true)
		f.tick()
		if n := f.parkedCount(); n != pushRateCap {
			t.Fatalf("%d parked, want %d", n, pushRateCap)
		}
		other := f.newItem("Other", &f.bob.ID)
		x := f.row("assigned", func(o *rowOpt) { o.item = other })
		clk.advance(defaultPushLease + time.Second)
		f.tick()
		if st := f.state(x); st.State != "skipped" || st.Reason != skipRateLimited {
			t.Fatalf("row after the cap's worth of parked pushes = %+v, want rate_limited", st)
		}
		if n := f.fake.callCount(); n != 1 {
			t.Fatalf("%d Send calls, want 1", n)
		}
	})
}

// A parked row that was settled some other way (here skipped) records nothing
// when retried; the entry must still go, or it is retried forever.
func TestPushDispatch_ParkedEntryClearsWhenTheRecordAffectsNoRow(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment")
		f.clock(time.Now())
		f.failSent(true)
		f.tick()
		if f.parkedCount() != 1 {
			t.Fatalf("%d parked, want 1", f.parkedCount())
		}
		f.failSent(false)
		f.exec(`UPDATE user_notifications SET push_state = 'skipped', skip_reason = 'x', claim_token = '', claimed_at = NULL WHERE id = ?`, id)
		f.tick()
		if f.parkedCount() != 0 {
			t.Fatal("the entry outlived a retry that settled nothing")
		}
	})
}

// Stop does not wait for the tick in flight, so Start can run a second loop
// beside it. Ticks are serialized: while one is blocked inside a gateway send,
// another does not claim or send, and runs only once the first finishes.
func TestTask4032_TicksNeverOverlapAcrossStopStart(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		first := f.row("comment")
		entered, release := parkedSend(f)
		done1 := make(chan struct{})
		go func() { f.tick(); close(done1) }()
		<-entered

		srv.stopPushDispatch()
		srv.StartPushDispatch()
		defer srv.stopPushDispatch()

		second := f.row("comment")
		done2 := make(chan struct{})
		go func() { f.tick(); close(done2) }()
		select {
		case <-done2:
			t.Fatal("a second tick ran to completion while the first was blocked in Send")
		case <-time.After(300 * time.Millisecond):
		}
		if n := f.fake.callCount(); n != 1 {
			t.Fatalf("%d Send calls while the first tick was blocked, want 1", n)
		}
		if st := f.state(second); st.Claim != "" {
			t.Fatalf("second row = %+v: a second tick claimed while the first was in flight", st)
		}
		close(release)
		<-done1
		<-done2
		for _, id := range []string{first, second} {
			if st := f.state(id); st.State != "sent" {
				t.Fatalf("row %s = %+v, want sent once the ticks ran in turn", id, st)
			}
		}
	})
}

// TASK-4032 codex r3 M1. A retry whose record fails in a later chunk has
// already committed the earlier ones; those ids must leave the parked set, or
// each counts twice against the rate cap (once as sent, once as parked). The
// fault hits one row in the second 500-id chunk (store.notificationIDChunk).
// Kills: clearing nothing on error (all ids stay parked); clearing everything
// on error (the failed chunk's ids leave the set while still pending).
func TestPushDispatch_PartialRecordClearsTheChunksThatLanded(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		const chunk = 500
		var ids []string
		for i := 0; i < chunk+5; i++ {
			ids = append(ids, f.row("comment"))
		}
		sorted := slices.Clone(ids)
		sort.Strings(sorted)
		bad := sorted[chunk+2]
		if f.pg() {
			f.exec(`CREATE OR REPLACE FUNCTION pd_fail_one() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'pd fault'; END $$`)
			f.exec(`CREATE TRIGGER pd_fail_one BEFORE UPDATE ON user_notifications FOR EACH ROW WHEN (NEW.push_state = 'sent' AND NEW.id = '` + bad + `') EXECUTE FUNCTION pd_fail_one()`)
		} else {
			f.exec(`CREATE TRIGGER pd_fail_one BEFORE UPDATE ON user_notifications WHEN NEW.push_state = 'sent' AND NEW.id = '` + bad + `' BEGIN SELECT RAISE(ABORT, 'pd fault'); END`)
		}
		now := time.Now()
		f.srv.parkUnrecorded(ids, nil, now)
		f.srv.retryUnrecorded(now)

		d := &f.srv.pushDispatch
		d.mu.Lock()
		parked := maps.Clone(d.unrecorded)
		d.mu.Unlock()
		if len(parked) != 5 {
			t.Fatalf("%d parked after a second-chunk failure, want 5 (the failed chunk)", len(parked))
		}
		for i, id := range sorted {
			_, stillParked := parked[id]
			st := f.state(id).State
			if i < chunk && (stillParked || st != "sent") {
				t.Fatalf("committed-chunk id %d: parked=%v state=%s, want unparked and sent", i, stillParked, st)
			}
			if i >= chunk && (!stillParked || st != "pending") {
				t.Fatalf("failed-chunk id %d: parked=%v state=%s, want parked and pending", i, stillParked, st)
			}
		}
	})
}

// failExpire installs (on) or drops (off) a trigger that aborts the expiry
// sweep's UPDATE (push_state='expired'), leaving every other write working.
func (f *pdx) failExpire(on bool) {
	f.t.Helper()
	switch {
	case on && f.pg():
		f.exec(`CREATE OR REPLACE FUNCTION pd_fail_expire() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'pd fault'; END $$`)
		f.exec(`CREATE TRIGGER pd_fail_expire BEFORE UPDATE ON user_notifications FOR EACH ROW WHEN (NEW.push_state = 'expired') EXECUTE FUNCTION pd_fail_expire()`)
	case on:
		f.exec(`CREATE TRIGGER pd_fail_expire BEFORE UPDATE ON user_notifications WHEN NEW.push_state = 'expired' BEGIN SELECT RAISE(ABORT, 'pd fault'); END`)
	case f.pg():
		f.exec(`DROP TRIGGER pd_fail_expire ON user_notifications`)
	default:
		f.exec(`DROP TRIGGER pd_fail_expire`)
	}
}

// TASK-4032 r5 H-Q1. A parked row past the expiry age is dropped from the set
// even when its record fails again; if the expiry sweep failed in the same
// tick, the row is still pending with a lapsed lease. The claim's expiry
// bound is what keeps it from being pushed a second time. Kills: removing the
// bound from the claim (2 Send calls).
func TestPushDispatch_DroppedParkedRowIsNotResentWhenExpireFails(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment")
		clk := f.clock(time.Now())
		f.failSent(true)
		f.tick()
		if f.fake.callCount() != 1 || f.parkedCount() != 1 {
			t.Fatalf("setup: %d Send calls, %d parked; want 1 and 1", f.fake.callCount(), f.parkedCount())
		}
		// Keep bob's device live across the jump, so a re-claimed row would be
		// SENT (and counted) rather than skipped as no_device.
		f.exec(`UPDATE sessions SET expires_at = ? WHERE user_id = ?`, f.ts(-48*time.Hour), f.bob.ID)
		f.failExpire(true)
		clk.advance(pushExpireAfter + defaultPushLease + time.Minute)
		f.tick()
		// The send count first: a re-sent row is parked again, so the parked
		// count alone would hide which failure happened.
		if n := f.fake.callCount(); n != 1 {
			t.Fatalf("%d Send calls, want 1: the dropped row was pushed again", n)
		}
		if n := f.parkedCount(); n != 0 {
			t.Fatalf("%d parked after the row passed the expiry age, want 0 (dropped)", n)
		}
		if st := f.state(id); st.State != "pending" {
			t.Fatalf("row = %+v, want pending (record and expire both failed)", st)
		}
	})
}

// TASK-4032 r5 H-Q1. The drop rule compares against the same second-truncated
// cutoff the claim uses, so a row created exactly on the cutoff second is
// claimable AND kept parked (excluded from the claim), never claimable and
// dropped. The tick starts half a second past that second. Kills: comparing
// against the untruncated cutoff (the entry is dropped).
func TestPushDispatch_ParkedEntryAtTheCutoffSecondIsKept(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment")
		created := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
		f.failSent(true)
		f.srv.parkUnrecorded([]string{id}, map[string]store.Notification{id: {ID: id, UserID: f.bob.ID, CreatedAt: created.Format(time.RFC3339)}}, created)
		f.srv.retryUnrecorded(created.Add(pushExpireAfter + 500*time.Millisecond))
		if n := f.parkedCount(); n != 1 {
			t.Fatalf("%d parked, want 1: a row on the cutoff second is still claimable, so its entry must stay", n)
		}
	})
}
