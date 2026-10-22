package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/notify"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3939 part B2a: the summary push and the 30/h cap, the retry matrix,
// and the expire and retention sweeps.

// itemsWithRows gives bob n more items with one assigned row each, the oldest
// first, and returns the row ids in that order.
func (f *pdx) itemsWithRows(n int) []string {
	f.t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		it := f.newItem(fmt.Sprintf("Extra %d", i), &f.bob.ID)
		ids = append(ids, f.row("assigned", func(o *rowOpt) { o.item = it; o.age = time.Duration(10-i) * time.Minute }))
	}
	return ids
}

// seedSent writes n rows for u that were pushed ago ago.
func (f *pdx) seedSent(n int, ago time.Duration) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		id := f.row("assigned")
		f.exec(`UPDATE user_notifications SET push_state = 'sent', sent_at = ? WHERE id = ?`, f.ts(ago), id)
	}
}

func (f *pdx) states(ids []string) []string {
	var out []string
	for _, id := range ids {
		st := f.state(id)
		out = append(out, st.State+"/"+st.Reason)
	}
	return out
}

func (f *pdx) wantAll(ids []string, state, reason string) {
	f.t.Helper()
	for _, id := range ids {
		if st := f.state(id); st.State != state || st.Reason != reason {
			f.t.Fatalf("row states = %v, want all %s/%s", f.states(ids), state, reason)
		}
	}
}

// --- summary ---

func TestPushDispatch_SummaryAtFourGroupsNotThree(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		ids := f.itemsWithRows(3)
		f.tick()
		f.wantAll(ids, "sent", "")
		for _, m := range f.fake.all() {
			if m.Data.Kind != "assigned" {
				t.Fatalf("three groups sent a %s push, want one push per group", m.Data.Kind)
			}
		}
		if n := len(f.fake.all()); n != 3 {
			t.Fatalf("%d messages for three groups, want 3", n)
		}
	})
}

func TestPushDispatch_SummaryMessageShape(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		f.device(f.bob, "tok-bob-2", 4*time.Hour)
		ids := f.itemsWithRows(3)
		// Group 4 holds two rows: the summary counts ROWS (5), not groups (4).
		newest := f.row("comment", func(o *rowOpt) { o.age = 30 * time.Second })
		older := f.row("comment", func(o *rowOpt) { o.age = 45 * time.Second })
		all := append(ids, newest, older)
		f.tick()
		f.wantAll(all, "sent", "")
		msgs := f.fake.all()
		if len(msgs) != 2 {
			t.Fatalf("%d messages, want one summary per device (2)", len(msgs))
		}
		cids := map[string]bool{}
		for _, m := range msgs {
			if m.Data.Kind != notify.KindSummary || m.Data.Count != 5 || m.Body != "You have 5 new notifications" {
				t.Fatalf("message = %+v, want a summary of 5 rows", m)
			}
			if m.Data.NID != newest {
				t.Fatalf("nid = %s, want the newest row %s", m.Data.NID, newest)
			}
			cids[m.Data.CID] = true
		}
		if len(cids) != 2 {
			t.Fatalf("connection ids = %v, want one per device", cids)
		}
	})
}

func TestPushDispatch_SummaryCountsOnlyVisible(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		var ids []string
		for i := 0; i < 5; i++ {
			it := f.newItem(fmt.Sprintf("Vis %d", i), &f.bob.ID)
			ids = append(ids, f.row("assigned", func(o *rowOpt) { o.item = it }))
			if i >= 2 {
				must(t, srv.store.DeleteItem(it.ID))
			}
		}
		f.tick()
		for _, m := range f.fake.all() {
			if m.Data.Kind == notify.KindSummary {
				t.Fatalf("a summary was sent for 2 visible groups: %+v", m)
			}
		}
		if n := len(f.fake.all()); n != 2 {
			t.Fatalf("%d messages, want 2 individual pushes", n)
		}
		if st := f.state(ids[4]); st.State != "skipped" || st.Reason != skipItemGone {
			t.Fatalf("invisible row = %+v", st)
		}
	})
}

// --- the cap ---

func TestPushDispatch_RateCap(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		for _, tc := range []struct {
			name     string
			sent     int
			ago      time.Duration
			wantSent bool
		}{
			{"29 in the hour admits", 29, 10 * time.Minute, true},
			{"30 in the hour limits", 30, 10 * time.Minute, false},
			{"30 just inside the hour limits", 30, 59 * time.Minute, false},
			{"30 just outside the hour do not count", 30, 61 * time.Minute, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newPdx(t, srv)
				f.seedSent(tc.sent, tc.ago)
				id := f.row("assigned")
				f.tick()
				st := f.state(id)
				if tc.wantSent && st.State != "sent" || !tc.wantSent && (st.State != "skipped" || st.Reason != skipRateLimited) {
					t.Fatalf("row = %+v, wantSent=%v", st, tc.wantSent)
				}
				if n := len(f.fake.all()); (n == 1) != tc.wantSent {
					t.Fatalf("%d messages, wantSent=%v", n, tc.wantSent)
				}
			})
		}
	})
}

// Rows admitted earlier in the same tick count against the cap.
func TestPushDispatch_RateCapCountsRowsAdmittedThisTick(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		f.seedSent(28, 10*time.Minute)
		ids := f.itemsWithRows(3)
		f.tick()
		if got := f.states(ids); got[0] != "sent/" || got[1] != "sent/" || got[2] != "skipped/"+skipRateLimited {
			t.Fatalf("states = %v, want the third unit limited after 28+1+1", got)
		}
	})
}

// The cap is decided on the summary as a unit, after the collapse: 29 sent
// plus a 4-group summary is admitted whole, not capped per group.
func TestPushDispatch_RateCapAppliesToTheSummary(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		f.seedSent(28, 10*time.Minute)
		ids := f.itemsWithRows(4)
		f.tick()
		f.wantAll(ids, "sent", "")
		if msgs := f.fake.all(); len(msgs) != 1 || msgs[0].Data.Kind != notify.KindSummary {
			t.Fatalf("messages = %+v, want one summary", msgs)
		}
	})
}

func TestPushDispatch_RateCapIsPerUser(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		carol := f.user("carol")
		must(t, srv.store.AddWorkspaceMember(f.ws.ID, carol.ID, "editor"))
		citem := f.newItem("Carol's", &carol.ID)
		f.device(carol, "tok-carol", 5*time.Hour)
		for i := 0; i < 30; i++ {
			id := f.row("assigned", func(o *rowOpt) { o.user = carol; o.item = citem })
			f.exec(`UPDATE user_notifications SET push_state = 'sent', sent_at = ? WHERE id = ?`, f.ts(5*time.Minute), id)
		}
		cRow := f.row("assigned", func(o *rowOpt) { o.user = carol; o.item = citem })
		bRow := f.row("assigned")
		f.tick()
		if st := f.state(cRow); st.State != "skipped" || st.Reason != skipRateLimited {
			t.Fatalf("carol's row = %+v, want rate_limited", st)
		}
		if st := f.state(bRow); st.State != "sent" {
			t.Fatalf("bob's row = %+v: carol's cap must not touch him", st)
		}
	})
}

// --- the retry matrix ---

func resultsFor(statuses ...string) func(context.Context, []notify.Message) ([]notify.Result, error) {
	return func(_ context.Context, msgs []notify.Message) ([]notify.Result, error) {
		out := make([]notify.Result, len(msgs))
		for i, m := range msgs {
			out[i] = notify.Result{ID: m.ID, Status: statuses[i%len(statuses)]}
		}
		return out, nil
	}
}

func (f *pdx) deviceCount() int {
	f.t.Helper()
	var n int
	must(f.t, f.srv.store.DB().QueryRow(f.srv.store.D().Rebind(`SELECT COUNT(*) FROM push_devices WHERE user_id = ?`), f.bob.ID).Scan(&n))
	return n
}

func TestPushDispatch_ResultMatrix(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		for _, tc := range []struct {
			name     string
			fn       func(context.Context, []notify.Message) ([]notify.Result, error)
			devices  int // extra devices beyond the fixture's one
			state    string
			reason   string
			attempts int
			left     int // devices left
		}{
			{"sent", resultsFor("sent"), 0, "sent", "", 0, 1},
			{"retry", resultsFor("retry"), 0, "pending", "", 1, 1},
			{"unknown status", resultsFor("bogus"), 0, "pending", "", 1, 1},
			{"rejected", resultsFor("rejected"), 0, "skipped", skipGatewayRejected, 0, 1},
			{"invalid_token", resultsFor("invalid_token"), 0, "skipped", skipNoDevice, 0, 0},
			{"bad request", func(context.Context, []notify.Message) ([]notify.Result, error) {
				return nil, fmt.Errorf("%w: x", notify.ErrBadRequest)
			}, 0, "skipped", skipGatewayRejected, 0, 1},
			{"unavailable", func(context.Context, []notify.Message) ([]notify.Result, error) {
				return nil, notify.ErrUnavailable
			}, 0, "pending", "", 1, 1},
			{"sent+retry is sent", resultsFor("sent", "retry"), 1, "sent", "", 0, 2},
			{"rejected+retry retries", resultsFor("rejected", "retry"), 1, "pending", "", 1, 2},
			{"invalid+retry retries, device gone", resultsFor("invalid_token", "retry"), 1, "pending", "", 1, 1},
			{"invalid+rejected is rejected, device gone", resultsFor("invalid_token", "rejected"), 1, "skipped", skipGatewayRejected, 0, 1},
			{"invalid+invalid is no_device", resultsFor("invalid_token"), 1, "skipped", skipNoDevice, 0, 0},
			{"invalid+sent is sent, device gone", resultsFor("invalid_token", "sent"), 1, "sent", "", 0, 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newPdx(t, srv)
				for i := 0; i < tc.devices; i++ {
					f.device(f.bob, fmt.Sprintf("tok-extra-%d", i), 4*time.Hour)
				}
				f.fake.fn = tc.fn
				id := f.row("comment")
				f.tick()
				if st := f.state(id); st.State != tc.state || st.Reason != tc.reason || st.Attempts != tc.attempts || (tc.state != "skipped" && st.Claim != "") {
					t.Fatalf("row = %+v, want %s/%s attempts %d", st, tc.state, tc.reason, tc.attempts)
				}
				if n := f.deviceCount(); n != tc.left {
					t.Fatalf("%d devices left, want %d", n, tc.left)
				}
				// A rejected or terminal row is never sent again.
				if tc.state == "skipped" || tc.state == "sent" {
					calls := f.fake.callCount()
					f.tick()
					if f.fake.callCount() != calls {
						t.Fatal("a settled row was sent again")
					}
				}
			})
		}
	})
}

// A retry release holds the row for one lease (TASK-3939 B2a): not reclaimable
// before it has run out, reclaimable after.
func TestPushDispatch_RetryDelayEqualsTheLease(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		f.fake.fn = resultsFor("retry")
		clk := f.clock(time.Now())
		id := f.row("comment")
		f.tick()
		if f.fake.callCount() != 1 {
			t.Fatal("first tick did not send")
		}
		clk.advance(defaultPushLease - 5*time.Second)
		f.tick()
		if f.fake.callCount() != 1 {
			t.Fatal("a released row was reclaimed before its lease had elapsed")
		}
		clk.advance(10 * time.Second)
		f.tick()
		if f.fake.callCount() != 2 || f.state(id).Attempts != 2 {
			t.Fatalf("calls = %d, row %+v: the row was not reclaimable once the lease elapsed", f.fake.callCount(), f.state(id))
		}
	})
}

func TestPushDispatch_FifthRetryIsGatewayFailed(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		f.fake.fn = resultsFor("retry")
		clk := f.clock(time.Now())
		id := f.row("comment")
		for i := 1; i <= store.MaxNotificationAttempts; i++ {
			if st := f.state(id); st.State != "pending" {
				t.Fatalf("before attempt %d: %+v", i, st)
			}
			f.tick()
			clk.advance(2 * defaultPushLease)
		}
		if st := f.state(id); st.State != "skipped" || st.Reason != store.SkipReasonGatewayFailed || st.Attempts != 5 {
			t.Fatalf("after 5 retries: %+v, want skipped/gateway_failed", st)
		}
		calls := f.fake.callCount()
		f.tick()
		if f.fake.callCount() != calls || calls != 5 {
			t.Fatalf("%d calls (was %d): want exactly 5 attempts", f.fake.callCount(), calls)
		}
	})
}

// A late invalid_token is a verdict on the registration that was SENT. It must
// delete only a row that is still that registration: same id, same token AND
// the same reg_seq. A row whose token changed under its id is not (no real path
// does that: the replacing register mints a new id, so this pins the token
// predicate directly), and neither is a same-user, same-token re-register,
// which updates the row in place, keeping id and token, and advances reg_seq
// (TASK-4027, TASK-4031).
func TestPushDispatch_InvalidTokenKeepsARowWhoseTokenChanged(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		f.fake.fn = func(_ context.Context, msgs []notify.Message) ([]notify.Result, error) {
			f.exec(`UPDATE push_devices SET token = 'tok-fresh' WHERE id = ?`, f.devID)
			return allStatus(msgs, notify.StatusInvalidToken), nil
		}
		f.row("comment")
		f.tick()
		if n := f.deviceCount(); n != 1 {
			t.Fatalf("%d devices, want the changed-token one kept", n)
		}
	})
}

// Kills: dropping the reg_seq predicate; deleting by id (or id and token) only.
func TestPushDispatch_InvalidTokenKeepsASameTokenReRegistration(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		var reregErr error
		f.fake.fn = func(_ context.Context, msgs []notify.Message) ([]notify.Result, error) {
			_, reregErr = srv.store.RegisterPushDevice(f.bob.ID, f.devSess, "conn-again", "android", "tok-bob-"+fmt.Sprint(f.n), "2")
			return allStatus(msgs, notify.StatusInvalidToken), nil
		}
		f.row("comment")
		f.tick()
		must(t, reregErr)
		if n := f.deviceCount(); n != 1 {
			t.Fatalf("%d devices, want the same-token re-registration kept", n)
		}
	})
}

// Positive control: with no re-register the same verdict deletes the device.
func TestPushDispatch_InvalidTokenDeletesAnUntouchedDevice(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		f.fake.fn = func(_ context.Context, msgs []notify.Message) ([]notify.Result, error) {
			return allStatus(msgs, notify.StatusInvalidToken), nil
		}
		f.row("comment")
		f.tick()
		if n := f.deviceCount(); n != 0 {
			t.Fatalf("%d devices, want the invalid one deleted", n)
		}
	})
}

func TestPushDispatch_GatewayFailuresWarnOncePerTickWithoutSecrets(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		f.fake.fn = func(context.Context, []notify.Message) ([]notify.Result, error) {
			return nil, fmt.Errorf("%w: boom tok-many-secret", notify.ErrUnavailable)
		}
		id, id2 := manyUnits(f) // two chunks, so a per-chunk warning would log twice
		logs := pdCaptureLogs(t)
		f.tick()
		out := logs.String()
		if c := strings.Count(out, "gateway unavailable"); c != 1 {
			t.Fatalf("%d unavailable warnings, want 1: %q", c, out)
		}
		for _, leak := range []string{id, id2, f.bob.ID, "tok-many", "boom"} {
			if strings.Contains(out, leak) {
				t.Fatalf("log leaks %q: %q", leak, out)
			}
		}
	})
}

// A tick that stops at the deadline after an unavailable chunk still warns
// once for it (TASK-3939 B2a codex r3).
func TestPushDispatch_UnavailableThenDeadlineStillWarns(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		clk := f.clock(time.Now())
		calls := 0
		f.fake.fn = func(context.Context, []notify.Message) ([]notify.Result, error) {
			calls++
			clk.advance(time.Hour) // the next chunk finds no time left
			return nil, fmt.Errorf("%w: boom", notify.ErrUnavailable)
		}
		manyUnits(f)
		logs := pdCaptureLogs(t)
		f.tick()
		if calls != 1 {
			t.Fatalf("%d sends, want 1: the second chunk must stop at the deadline", calls)
		}
		if c := strings.Count(logs.String(), "gateway unavailable"); c != 1 {
			t.Fatalf("%d unavailable warnings, want 1: %q", c, logs.String())
		}
	})
}

// --- expire and retention ---

func TestPushDispatch_ExpiresPendingOlderThanAnHour(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		fresh := f.row("comment", func(o *rowOpt) { o.age = 59 * time.Minute })
		stale := f.row("comment", func(o *rowOpt) { o.age = 61 * time.Minute })
		f.tick()
		if st := f.state(fresh); st.State != "sent" {
			t.Fatalf("59m row = %+v", st)
		}
		if st := f.state(stale); st.State != "expired" {
			t.Fatalf("61m row = %+v, want expired", st)
		}
		for _, m := range f.fake.all() {
			if m.Data.NID == stale {
				t.Fatal("an expired row was pushed")
			}
		}
	})
}

func TestPushDispatch_ExpireAndPruneLeaveALiveClaim(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment", func(o *rowOpt) { o.age = 8 * 24 * time.Hour })
		f.exec(`UPDATE user_notifications SET claim_token = 'other', claimed_at = ? WHERE id = ?`, f.ts(5*time.Second), id)
		f.tick()
		if st := f.state(id); st.State != "pending" || st.Claim != "other" {
			t.Fatalf("a row under a live claim = %+v, want untouched", st)
		}
	})
}

func TestPushDispatch_RetentionSevenDays(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		exists := func(id string) bool {
			var n int
			must(t, srv.store.DB().QueryRow(srv.store.D().Rebind(`SELECT COUNT(*) FROM user_notifications WHERE id = ?`), id).Scan(&n))
			return n == 1
		}
		kept, gone := map[string]bool{}, map[string]bool{}
		for _, state := range []string{"sent", "skipped", "expired"} {
			for _, age := range []time.Duration{6 * 24 * time.Hour, 8 * 24 * time.Hour} {
				id := f.row("assigned", func(o *rowOpt) { o.age = age })
				f.exec(`UPDATE user_notifications SET push_state = ? WHERE id = ?`, state, id)
				if age < 7*24*time.Hour {
					kept[id] = true
				} else {
					gone[id] = true
				}
			}
		}
		pend := f.row("assigned", func(o *rowOpt) { o.age = 6 * 24 * time.Hour })
		f.tick()
		for id := range kept {
			if !exists(id) {
				t.Fatalf("a 6-day-old settled row %s was pruned", id)
			}
		}
		for id := range gone {
			if exists(id) {
				t.Fatalf("an 8-day-old settled row %s survived", id)
			}
		}
		if st := f.state(pend); st.State != "expired" {
			t.Fatalf("6-day pending = %+v, want expired and kept", st)
		}
	})
}

func TestPushDispatch_PruneRunsAtMostHourly(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		clk := f.clock(time.Now())
		f.tick() // prunes (nothing yet)
		old := f.row("assigned", func(o *rowOpt) { o.age = 8 * 24 * time.Hour })
		f.exec(`UPDATE user_notifications SET push_state = 'sent' WHERE id = ?`, old)
		exists := func() bool {
			var n int
			must(t, srv.store.DB().QueryRow(srv.store.D().Rebind(`SELECT COUNT(*) FROM user_notifications WHERE id = ?`), old).Scan(&n))
			return n == 1
		}
		clk.advance(30 * time.Minute)
		f.tick()
		if !exists() {
			t.Fatal("the prune ran twice within the hour")
		}
		clk.advance(31 * time.Minute)
		f.tick()
		if exists() {
			t.Fatal("the prune did not run after the hour")
		}
	})
}

func TestPushDispatch_ExpireRunsWithoutASender(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment", func(o *rowOpt) { o.age = 2 * time.Hour })
		srv.SetPushSender(nil)
		f.tick()
		if st := f.state(id); st.State != "expired" {
			t.Fatalf("row = %+v, want expired even with no gateway configured", st)
		}
	})
}
