package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/redis/go-redis/v9"
)

func TestSessionActivityReorderWindowDerivedFromReportBound(t *testing.T) {
	if want := models.SessionActivityReportBound + reorderSlack; reorderWindow != want {
		t.Fatalf("reorder window = %v, want report bound plus slack %v", reorderWindow, want)
	}
	if reorderWindow%time.Second != 0 {
		t.Fatal("reorder window must be whole seconds for the Lua script")
	}
}

func TestSessionActivityTimestampOrdering(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			now := time.Now().UTC()
			srv := testServerWithPresence(t)
			srv.sessionPresence.(*MemorySessionPresence).now = func() time.Time { return now }
			if backend == "redis" {
				p, _, _ := newRedisPresencePair(t)
				p.now = func() time.Time { return now }
				srv.SetSessionPresence(p)
			}
			_, _, tok, user := setupWatchTestUser(t, srv)
			userB, err := srv.store.CreateUser(models.UserCreate{Email: "order-b@example.com", Name: "B", Password: "pw-test-12345"})
			if err != nil {
				t.Fatal(err)
			}
			tokB, err := srv.store.CreateAPIToken(userB.ID, models.APITokenCreate{Name: "B"}, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			// Nanosecond precision and a skewed client clock must both survive.
			const t1 = "2001-01-01T00:00:00.123456789Z"
			const t2 = "2001-01-01T00:00:00.123456790Z"
			post := func(t *testing.T, token, state, at string, status int) {
				t.Helper()
				body := fmt.Sprintf(`{"state":%q`, state)
				if at != "absent" {
					body += fmt.Sprintf(`,"at":%q`, at)
				}
				body += "}"
				rr := presenceRequest(t, srv, http.MethodPost, token, presenceWorkerA, body)
				if rr.Code != status {
					t.Fatalf("post %s: %d %s, want %d", body, rr.Code, rr.Body.String(), status)
				}
				if status == 204 && rr.Body.Len() != 0 {
					t.Fatal("204 has body")
				}
				if status == 400 && !strings.Contains(rr.Body.String(), `"code":"validation_error"`) {
					t.Fatal(rr.Body.String())
				}
			}
			check := func(t *testing.T, want bool) {
				t.Helper()
				got, err := srv.sessionPresence.BusyWorkers(user.ID, []string{presenceWorkerA})
				if err != nil || got[presenceWorkerA] != want {
					t.Fatalf("busy = %v, %v; want %v", got, err, want)
				}
			}
			t.Run("late busy stays idle", func(t *testing.T) {
				post(t, tok.Token, "idle", t2, 204)
				post(t, tok.Token, "busy", t1, 204)
				check(t, false)
			})
			t.Run("newer idle applies", func(t *testing.T) {
				post(t, tok.Token, "busy", "absent", 204)
				post(t, tok.Token, "busy", t1, 204)
				check(t, true)
				post(t, tok.Token, "idle", t2, 204)
				check(t, false)
			})
			t.Run("equal instant later arrival applies", func(t *testing.T) {
				post(t, tok.Token, "busy", "2001-01-01T01:00:00.123456790+01:00", 204)
				check(t, true)
				post(t, tok.Token, "idle", t2, 204)
				check(t, false)
			})
			t.Run("legacy clears ordering", func(t *testing.T) {
				post(t, tok.Token, "idle", t2, 204)
				post(t, tok.Token, "busy", "absent", 204)
				check(t, true)
				post(t, tok.Token, "idle", t1, 204)
				check(t, false)
				post(t, tok.Token, "busy", "2000-01-01T00:00:00Z", 204)
				check(t, true) // A year-long gap is a clock step, not a reorder.
			})
			t.Run("timestamped after legacy applies", func(t *testing.T) {
				for _, tc := range []struct {
					name   string
					offset time.Duration
				}{
					{"older within reorder window", -2 * time.Second},
					{"newer", 2 * time.Second},
				} {
					t.Run(tc.name, func(t *testing.T) {
						// Seed a legacy idle record at the injected server time T.
						// With no client_at, even T - 2s must change its state.
						if err := srv.sessionPresence.SetActivity(user.ID, presenceWorkerA, false, now, nil); err != nil {
							t.Fatal(err)
						}
						check(t, false)
						post(t, tok.Token, "busy", now.Add(tc.offset).Format(time.RFC3339Nano), 204)
						check(t, true)
					})
				}
			})
			t.Run("malformed timestamp", func(t *testing.T) {
				post(t, tok.Token, "idle", "absent", 204)
				for _, at := range []string{"", "tomorrow", "2001-01-01"} {
					post(t, tok.Token, "busy", at, 400)
				}
				for _, at := range []string{"null", "123", "[]"} {
					rr := presenceRequest(t, srv, http.MethodPost, tok.Token, presenceWorkerA, `{"state":"busy","at":`+at+`}`)
					if rr.Code != 400 || !strings.Contains(rr.Body.String(), `"code":"validation_error"`) {
						t.Fatalf("invalid at %s: %d %s", at, rr.Code, rr.Body.String())
					}
				}
				check(t, false)
			})
			t.Run("cross user same worker", func(t *testing.T) {
				post(t, tok.Token, "idle", "absent", 204)
				post(t, tokB.Token, "busy", "2099-01-01T00:00:00Z", 204)
				check(t, false)
			})
		})
	}
}

func TestSessionActivityReorderWindow(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			srv := testServerWithPresence(t)
			if backend == "redis" {
				p, _, _ := newRedisPresencePair(t)
				srv.SetSessionPresence(p)
			}
			_, _, tok, user := setupWatchTestUser(t, srv)
			at := time.Date(2026, 10, 23, 12, 0, 0, 123456789, time.UTC)
			for _, tc := range []struct {
				name             string
				stored, incoming time.Time
				storedBusy       bool
				wantBusy         bool
			}{
				{"two second reorder", at, at.Add(-2 * time.Second), false, false},
				{"far future clock recovers", at.Add(time.Hour), at, true, false},
				{"clock steps back", at, at.Add(-time.Hour), true, false},
				{"ten second boundary", at, at.Add(-10 * time.Second), false, false},
				{"one nanosecond beyond boundary", at, at.Add(-10*time.Second - time.Nanosecond), false, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					post := func(busy bool, timestamp *time.Time) {
						t.Helper()
						state := "idle"
						if busy {
							state = "busy"
						}
						body := fmt.Sprintf(`{"state":%q`, state)
						if timestamp != nil {
							body += fmt.Sprintf(`,"at":%q`, timestamp.Format(time.RFC3339Nano))
						}
						rr := presenceRequest(t, srv, http.MethodPost, tok.Token, presenceWorkerA, body+"}")
						if rr.Code != http.StatusNoContent {
							t.Fatalf("post %s: %d %s", body, rr.Code, rr.Body.String())
						}
					}
					post(false, nil) // Clear ordering from the preceding case.
					post(tc.storedBusy, &tc.stored)
					post(!tc.storedBusy, &tc.incoming)
					got, err := srv.sessionPresence.BusyWorkers(user.ID, []string{presenceWorkerA})
					if err != nil || got[presenceWorkerA] != tc.wantBusy {
						t.Fatalf("busy = %v, %v; want %v", got, err, tc.wantBusy)
					}
				})
			}
		})
	}
}

func TestSessionActivityReceiptExpiry(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			now := time.Now().UTC()
			var p SessionPresence
			var advance func(time.Duration)
			if backend == "memory" {
				m := NewMemorySessionPresence()
				m.now = func() time.Time { return now }
				p = m
				advance = func(d time.Duration) { now = now.Add(d) }
			} else {
				r, _, mr := newRedisPresencePair(t)
				r.now = func() time.Time { return now }
				p = r
				advance = func(d time.Duration) { now = now.Add(d); mr.FastForward(d) }
			}
			for _, clientAt := range []time.Time{now.Add(-24 * time.Hour), now.Add(24 * time.Hour)} {
				if err := p.SetActivity("A", presenceWorkerA, true, now, &clientAt); err != nil {
					t.Fatal(err)
				}
				for _, step := range []struct {
					after time.Duration
					busy  bool
				}{{29 * time.Minute, true}, {time.Minute, false}} {
					advance(step.after)
					got, err := p.BusyWorkers("A", []string{presenceWorkerA})
					if err != nil || got[presenceWorkerA] != step.busy {
						t.Fatalf("skewed client %s: busy=%v, %v, want %v", clientAt, got, err, step.busy)
					}
				}
			}
			clientAt := now
			if err := p.SetActivity("A", presenceWorkerA, true, now, &clientAt); err != nil {
				t.Fatal(err)
			}
			advance(29 * time.Minute)
			stale := clientAt.Add(-time.Nanosecond)
			if err := p.SetActivity("A", presenceWorkerA, false, now, &stale); err != nil {
				t.Fatal(err)
			}
			got, err := p.BusyWorkers("A", []string{presenceWorkerA})
			if err != nil || !got[presenceWorkerA] {
				t.Fatalf("stale idle replaced busy: %v %v", got, err)
			}
			advance(time.Minute)
			got, err = p.BusyWorkers("A", []string{presenceWorkerA})
			if err != nil || got[presenceWorkerA] {
				t.Fatalf("stale write refreshed busy TTL: %v %v", got, err)
			}
			t.Run("expired record does not order next write", func(t *testing.T) {
				clientAt := now
				if err := p.SetActivity("A", presenceWorkerA, false, now, &clientAt); err != nil {
					t.Fatal(err)
				}
				advance(sessionActivityTTL + time.Second)
				older := clientAt.Add(-time.Nanosecond)
				if err := p.SetActivity("A", presenceWorkerA, true, now, &older); err != nil {
					t.Fatal(err)
				}
				got, err := p.BusyWorkers("A", []string{presenceWorkerA})
				if err != nil || !got[presenceWorkerA] {
					t.Fatalf("expired record blocked older write: %v %v", got, err)
				}
			})
		})
	}
}

type activityScriptOnlyHook struct {
	t       *testing.T
	scripts int
}

func (h *activityScriptOnlyHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *activityScriptOnlyHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.t.Error("activity write used a pipeline")
		return next(ctx, cmds)
	}
}
func (h *activityScriptOnlyHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		switch cmd.Name() {
		case "eval", "evalsha":
			h.scripts++
		default:
			h.t.Errorf("activity write used %s outside Lua", cmd.Name())
		}
		return next(ctx, cmd)
	}
}

func TestSessionActivityRedisWritesOnlyThroughScript(t *testing.T) {
	p, reader, _ := newRedisPresencePair(t)
	now := time.Now().UTC()
	if err := p.SetActivity("A", presenceWorkerA, false, now, &now); err != nil {
		t.Fatal(err)
	}
	hook := &activityScriptOnlyHook{t: t}
	p.client.AddHook(hook)
	stale := now.Add(-time.Nanosecond)
	if err := p.SetActivity("A", presenceWorkerA, true, now, &stale); err != nil {
		t.Fatal(err)
	}
	if got, err := reader.BusyWorkers("A", []string{presenceWorkerA}); err != nil || got[presenceWorkerA] {
		t.Fatalf("older busy overwrote newer idle: %v %v", got, err)
	}
	if err := p.SetActivity("A", presenceWorkerA, true, now, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := reader.BusyWorkers("A", []string{presenceWorkerA}); err != nil || !got[presenceWorkerA] {
		t.Fatalf("legacy write failed: %v %v", got, err)
	}
	if hook.scripts < 2 {
		t.Fatalf("got %d script calls, want at least two", hook.scripts)
	}
}
