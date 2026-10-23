package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/notify"
)

// TASK-3939 part B1: the dispatch core. Gate cases live in
// task3939b_push_gates_test.go.

func TestPushDispatch_SendsOnePerUnitAndSettlesSent(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		f.device(f.bob, "tok-bob-2", 4*time.Hour)
		other := f.newItem("Other", &f.bob.ID)
		a1, a2 := f.row("comment"), f.row("comment", func(o *rowOpt) { o.age = 3 * time.Minute })
		b := f.row("assigned", func(o *rowOpt) { o.item = other })
		f.tick()
		for _, id := range []string{a1, a2, b} {
			if st := f.state(id); st.State != "sent" || st.SentAt == "" || st.Claim != "" {
				t.Fatalf("row %s = %+v, want sent, stamped, unclaimed", id, st)
			}
		}
		// two units (one per item) x two devices, in one request
		if n := f.fake.callCount(); n != 1 {
			t.Fatalf("%d Send calls, want 1", n)
		}
		msgs := f.fake.all()
		if len(msgs) != 4 {
			t.Fatalf("%d messages, want 2 units x 2 devices", len(msgs))
		}
		seen := map[string]bool{}
		for _, m := range msgs {
			if seen[m.ID] {
				t.Fatalf("duplicate message id %s", m.ID)
			}
			seen[m.ID] = true
		}
		nids := map[string]int{}
		for _, m := range msgs {
			nids[m.Data.NID]++
		}
		if nids[a1] != 2 || nids[b] != 2 || nids[a2] != 0 {
			t.Fatalf("nids = %v: the item's two rows are one push carrying the newest row's id", nids)
		}
	})
}

func TestPushDispatch_NonSentResultsReleaseWithAnAttempt(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		f.fake.fn = func(_ context.Context, msgs []notify.Message) ([]notify.Result, error) {
			return allStatus(msgs, notify.StatusRetry), nil
		}
		id := f.row("comment")
		clk := f.clock(time.Now())
		f.tick()
		if st := f.state(id); st.State != "pending" || st.Attempts != 1 || st.Claim != "" {
			t.Fatalf("after retry: %+v, want pending, 1 attempt, unclaimed", st)
		}
		f.fake.fn = func(context.Context, []notify.Message) ([]notify.Result, error) { return nil, notify.ErrUnavailable }
		clk.advance(2 * defaultPushLease)
		f.tick()
		if st := f.state(id); st.State != "pending" || st.Attempts != 2 {
			t.Fatalf("after a failed request: %+v, want 2 attempts", st)
		}
	})
}

// A unit with one device sent and another retried is sent: re-pushing would
// duplicate to the device that already has it.
func TestPushDispatch_OneDeviceSentIsSent(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		f.device(f.bob, "tok-bob-2", 4*time.Hour)
		f.fake.fn = func(_ context.Context, msgs []notify.Message) ([]notify.Result, error) {
			out := allStatus(msgs, notify.StatusRetry)
			out[0].Status = notify.StatusSent
			return out, nil
		}
		id := f.row("comment")
		f.tick()
		if st := f.state(id); st.State != "sent" || st.Attempts != 0 {
			t.Fatalf("row = %+v, want sent", st)
		}
	})
}

// --- lease invariant (r6-Q1) ---

func TestPushDispatch_LeaseExceedsSendTimeout(t *testing.T) {
	srv := testServer(t)
	cfg := srv.resolvePushDispatchSettings()
	if cfg.sendTimeout != notify.SendTimeout {
		t.Fatalf("send timeout = %v, want notify.SendTimeout", cfg.sendTimeout)
	}
	if cfg.lease < notify.SendTimeout+cfg.leaseMargin {
		t.Fatalf("default lease %v < send timeout %v + margin %v", cfg.lease, notify.SendTimeout, cfg.leaseMargin)
	}
	if got := notify.NewGateway("http://x.test", "s").HTTP.Timeout; got != notify.SendTimeout {
		t.Fatalf("gateway client timeout = %v, want notify.SendTimeout", got)
	}
	for _, lease := range []time.Duration{20 * time.Second, notify.SendTimeout, cfg.sendTimeout + cfg.leaseMargin - time.Second} {
		if err := srv.SetPushDispatchConfig(0, 0, lease, 0, 0); !errors.Is(err, errPushLeaseTooShort) {
			t.Fatalf("lease %v: err = %v, want refused", lease, err)
		}
	}
	if got := srv.resolvePushDispatchSettings().lease; got != cfg.lease {
		t.Fatalf("a refused config changed the lease to %v", got)
	}
	if err := srv.SetPushDispatchConfig(0, 0, cfg.sendTimeout+cfg.leaseMargin, 0, 0); err != nil {
		t.Fatalf("a lease exactly at the invariant was refused: %v", err)
	}
	// A margin near MaxInt64 would wrap sendTimeout+margin negative and pass
	// the comparison; it must refuse and change nothing (TASK-3939 part C,
	// ruling 37e3eafa).
	for _, margin := range []time.Duration{2562047 * time.Hour, time.Duration(math.MaxInt64) - cfg.sendTimeout + 1, time.Duration(math.MaxInt64)} {
		if err := srv.SetPushDispatchConfig(0, 0, 0, margin, 0); !errors.Is(err, errPushLeaseTooShort) {
			t.Fatalf("margin %v: err = %v, want refused", margin, err)
		}
	}
	// The error states the resolved values.
	if err := srv.SetPushDispatchConfig(0, 0, 30*time.Second, 0, 0); err == nil || !strings.Contains(err.Error(), "lease 30s") || !strings.Contains(err.Error(), "margin 15s") {
		t.Fatalf("err = %v, want the resolved lease and margin", err)
	}
	// Raising the send timeout past the lease is the same violation.
	if err := srv.SetPushDispatchConfig(0, 0, 0, 0, time.Hour); !errors.Is(err, errPushLeaseTooShort) {
		t.Fatalf("send timeout raised past the lease: err = %v", err)
	}
}

// The Send runs under the tick's deadline (start + lease - margin), so it ends
// before the claim lapses; while it runs, and while the tick still holds the
// row after the deadline fired, no other claimer can take the row. The clock
// is the dispatcher's seam: the tick "started" an hour ago, so its deadline is
// already past and the Send's context must already be done.
func TestPushDispatch_SlowSendNotReclaimedMidFlight(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		must(t, srv.SetPushDispatchConfig(0, 0, 3*time.Second, time.Second, time.Second))
		clk := f.clock(time.Now().Add(-time.Hour))
		id := f.row("comment", func(o *rowOpt) { o.age = 90 * time.Minute })
		compete := func(when string) {
			c, err := srv.store.ClaimPendingNotifications("rival", 10, time.Now().Add(-10*time.Second), time.Now().Add(-3*time.Second), time.Time{}, nil)
			if err != nil || len(c.Rows) != 0 {
				t.Errorf("%s: a rival claimed %d rows (err %v)", when, len(c.Rows), err)
			}
		}
		f.fake.fn = func(ctx context.Context, _ []notify.Message) ([]notify.Result, error) {
			compete("mid-flight")
			dl, ok := ctx.Deadline()
			if !ok || !dl.Equal(clk.start.Add(2*time.Second)) {
				t.Errorf("Send deadline = %v (set %v), want tick start + lease - margin = %v", dl, ok, clk.start.Add(2*time.Second))
			}
			if ctx.Err() == nil {
				t.Error("the deadline had passed but the Send context is live: no deadline on the gateway call")
			}
			compete("after the deadline fired, tick still holding")
			return nil, notify.ErrUnavailable
		}
		f.tick()
		if n := f.fake.callCount(); n != 1 {
			t.Errorf("%d Send calls while the row was held", n)
		}
		if st := f.state(id); st.State != "pending" || st.Attempts != 1 || st.Claim != "" {
			t.Fatalf("released row = %+v", st)
		}
		// A retry release keeps the row unavailable for the lease (B2a), so
		// the rival gets it once the lease has run out.
		c, err := srv.store.ClaimPendingNotifications("rival", 10, time.Now().Add(-10*time.Second), time.Now().Add(time.Minute), time.Time{}, nil)
		if err != nil || len(c.Rows) != 1 {
			t.Fatalf("after release the rival claimed %d rows (err %v)", len(c.Rows), err)
		}
	})
}

// manyUnits gives bob 51 devices and two items with a row each: 102 messages,
// so two chunks of one unit each.
func manyUnits(f *pdx) (first, second string) {
	for i := 0; i < 50; i++ {
		f.device(f.bob, fmt.Sprintf("tok-many-%d-%d", f.n, i), 4*time.Hour)
	}
	other := f.newItem("Second", &f.bob.ID)
	first = f.row("assigned", func(o *rowOpt) { o.age = 5 * time.Minute })
	second = f.row("assigned", func(o *rowOpt) { o.item = other })
	return
}

// A chunk starts only if a whole send timeout is left before the deadline;
// otherwise its rows are unclaimed with no attempt burned.
func TestPushDispatch_ChunkNotStartedWithoutTimeLeft(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		must(t, srv.SetPushDispatchConfig(0, 0, 3*time.Second, time.Second, time.Second))
		first, second := manyUnits(f)
		clk := f.clock(time.Now())
		f.fake.fn = func(_ context.Context, msgs []notify.Message) ([]notify.Result, error) {
			clk.advance(1200 * time.Millisecond) // deadline (2s) minus this is under the 1s send timeout
			return allStatus(msgs, notify.StatusSent), nil
		}
		f.tick()
		if n := f.fake.callCount(); n != 1 {
			t.Fatalf("%d Send calls, want only the first chunk", n)
		}
		if st := f.state(first); st.State != "sent" {
			t.Fatalf("first = %+v", st)
		}
		if st := f.state(second); st.State != "pending" || st.Attempts != 0 || st.Claim != "" {
			t.Fatalf("second = %+v, want pending, unclaimed, no attempt", st)
		}
	})
}

// --- fresh token, settle under the claim token ---

func TestPushDispatch_FreshClaimTokenPerTick(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		var prefixes []string
		f.fake.fn = func(_ context.Context, msgs []notify.Message) ([]notify.Result, error) {
			var tok string
			row := srv.store.DB().QueryRow(`SELECT claim_token FROM user_notifications WHERE claim_token <> '' LIMIT 1`)
			if err := row.Scan(&tok); err != nil {
				t.Errorf("no claimed row mid-send: %v", err)
			}
			prefixes = append(prefixes, tok[:strings.LastIndex(tok, ".")])
			return allStatus(msgs, notify.StatusRetry), nil
		}
		id := f.row("comment")
		clk := f.clock(time.Now())
		f.tick()
		clk.advance(2 * defaultPushLease) // a retry release waits out the lease
		f.tick()
		if len(prefixes) != 2 || prefixes[0] == prefixes[1] || !strings.HasPrefix(prefixes[0], "pd-") {
			t.Fatalf("caller tokens = %v, want two different pd-... tokens", prefixes)
		}
		if st := f.state(id); st.Attempts != 2 {
			t.Fatalf("row = %+v, want both ticks to have settled their release", st)
		}
	})
}

// --- r4-L4: a send that outlives its lease ---

func parkedSend(f *pdx) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.fake.fn = func(_ context.Context, msgs []notify.Message) ([]notify.Result, error) {
		once.Do(func() { close(entered) })
		<-release
		return allStatus(msgs, notify.StatusSent), nil
	}
	return
}

func (f *pdx) lapse(ids ...string) {
	f.t.Helper()
	for _, id := range ids {
		f.exec(`UPDATE user_notifications SET claimed_at = ? WHERE id = ?`, f.ts(2*time.Minute), id)
	}
}

func TestPushDispatch_LateSentRecordedWhenReclaimed(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment")
		entered, release := parkedSend(f)
		done := make(chan struct{})
		go func() { f.tick(); close(done) }()
		<-entered
		f.lapse(id)
		b, err := srv.store.ClaimPendingNotifications("B", 10, time.Now().Add(-10*time.Second), time.Now().Add(-time.Minute), time.Time{}, nil)
		if err != nil || len(b.Rows) != 1 {
			t.Fatalf("B reclaimed %d rows (%v)", len(b.Rows), err)
		}
		close(release)
		<-done
		if st := f.state(id); st.State != "sent" || st.SentAt == "" {
			t.Fatalf("row = %+v: A's delivery must be recorded although B holds the row", st)
		}
		if n, _ := srv.store.MarkNotificationsSkipped(b.Token, []string{id}, "late"); n != 0 {
			t.Fatal("B's settle changed a row recorded sent")
		}
	})
}

func TestPushDispatch_LateSentRecordedWhenExpired(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment", func(o *rowOpt) { o.age = 2 * time.Minute })
		entered, release := parkedSend(f)
		done := make(chan struct{})
		go func() { f.tick(); close(done) }()
		<-entered
		f.exec(`UPDATE user_notifications SET created_at = ? WHERE id = ?`, f.ts(2*time.Hour), id)
		f.lapse(id)
		if n, err := srv.store.ExpireStaleNotifications(time.Now().Add(-time.Hour), time.Now().Add(-time.Minute)); err != nil || n != 1 {
			t.Fatalf("expire = %d, %v", n, err)
		}
		close(release)
		<-done
		if st := f.state(id); st.State != "sent" {
			t.Fatalf("row = %+v: a delivered row must not read expired", st)
		}
	})
}

// The pre-send re-check: rows taken by another instance while an earlier
// chunk was in flight are dropped from the next one.
func TestPushDispatch_HeldRecheckDropsRowsTakenMidTick(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		first, second := manyUnits(f)
		entered, release := parkedSend(f)
		done := make(chan struct{})
		go func() { f.tick(); close(done) }()
		<-entered
		f.lapse(second)
		b, err := srv.store.ClaimPendingNotifications("B", 10, time.Now().Add(-10*time.Second), time.Now().Add(-time.Minute), time.Time{}, nil)
		if err != nil || len(b.Rows) != 1 || b.Rows[0].ID != second {
			t.Fatalf("B claimed %v (%v), want the second row only", b.Rows, err)
		}
		close(release)
		<-done
		if n := f.fake.callCount(); n != 1 {
			t.Fatalf("%d Send calls: the row B holds was sent again", n)
		}
		if st := f.state(first); st.State != "sent" {
			t.Fatalf("first = %+v", st)
		}
		if st := f.state(second); st.State != "pending" || st.Claim != b.Token {
			t.Fatalf("second = %+v, want still B's", st)
		}
	})
}

// 3946's supersede can DELETE a pending row the dispatcher has claimed. The
// settle that finds it gone is a no-op, not an error and not a log line.
func TestPushDispatch_RowDeletedMidSendIsNotAnError(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment")
		logs := pdCaptureLogs(t)
		entered, release := parkedSend(f)
		done := make(chan struct{})
		go func() { f.tick(); close(done) }()
		<-entered
		f.exec(`DELETE FROM user_notifications WHERE id = ?`, id)
		close(release)
		<-done
		if out := logs.String(); strings.Contains(out, "level=WARN") || strings.Contains(out, "level=ERROR") {
			t.Fatalf("a deleted row logged: %s", out)
		}
	})
}

// --- shutdown ---

func TestPushDispatch_ShutdownUnclaimsWithoutAnAttempt(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment")
		ctx, cancel := context.WithCancel(context.Background())
		entered := make(chan struct{})
		f.fake.fn = func(c context.Context, _ []notify.Message) ([]notify.Result, error) {
			close(entered)
			<-c.Done()
			return nil, notify.ErrUnavailable
		}
		done := make(chan struct{})
		go func() { srv.runPushDispatchTick(ctx); close(done) }()
		<-entered
		cancel()
		<-done
		if st := f.state(id); st.State != "pending" || st.Attempts != 0 || st.Claim != "" {
			t.Fatalf("after shutdown mid-send: %+v, want pending, unclaimed, no attempt", st)
		}

		// and a tick that begins cancelled sends nothing
		f.fake.fn = nil
		before := f.fake.callCount()
		srv.runPushDispatchTick(ctx)
		if f.fake.callCount() != before {
			t.Fatal("a cancelled tick sent")
		}
		if st := f.state(id); st.State != "pending" || st.Claim != "" {
			t.Fatalf("cancelled tick left %+v", st)
		}
	})
}

func TestPushDispatch_LoopRunsTicksAndStops(t *testing.T) {
	srv := testServer(t)
	f := newPdx(t, srv)
	tick := make(chan time.Time)
	srv.pushDispatch.tick = tick
	id := f.row("comment")
	srv.StartPushDispatch()
	srv.StartPushDispatch() // idempotent
	tick <- time.Now()
	deadline := time.Now().Add(5 * time.Second)
	for f.state(id).State != "sent" {
		if time.Now().After(deadline) {
			t.Fatal("the loop never dispatched the row")
		}
		time.Sleep(20 * time.Millisecond)
	}
	srv.stopPushDispatch()
	srv.stopPushDispatch() // safe twice
}

// --- logging ---

func pdCaptureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&pdLogWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

type pdLogWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *pdLogWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// A failed claim logs a fixed line: never the store's error text, which can
// echo a value, and never an id.
func TestPushDispatch_FailedClaimLogsNoStoreError(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment")
		logs := pdCaptureLogs(t)
		f.exec(`ALTER TABLE user_notifications RENAME TO user_notifications_broken`)
		defer f.exec(`ALTER TABLE user_notifications_broken RENAME TO user_notifications`)
		f.tick()
		out := logs.String()
		if !strings.Contains(out, "claim failed") {
			t.Fatalf("no claim failure logged: %q", out)
		}
		for _, leak := range []string{id, f.bob.ID, "user_notifications", "no such table", "does not exist", "claim notifications"} {
			if strings.Contains(out, leak) {
				t.Fatalf("log leaks %q: %s", leak, out)
			}
		}
	})
}
