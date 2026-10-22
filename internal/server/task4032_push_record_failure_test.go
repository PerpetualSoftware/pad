package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/notify"
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

func TestPushDispatch_UnrecordedSetIsCapped(t *testing.T) {
	srv := testServer(t)
	var ids []string
	for i := 0; i < pushUnrecordedCap+1; i++ {
		ids = append(ids, fmt.Sprintf("id-%d", i))
	}
	srv.parkUnrecorded(ids, nil, time.Now())
	d := &srv.pushDispatch
	if len(d.unrecorded) != pushUnrecordedCap {
		t.Fatalf("%d parked, want the cap %d", len(d.unrecorded), pushUnrecordedCap)
	}
	if _, ok := d.unrecorded[ids[0]]; ok {
		t.Fatal("the oldest entry survived the overflow")
	}
	if _, ok := d.unrecorded[ids[len(ids)-1]]; !ok {
		t.Fatal("the newest entry was dropped")
	}
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
