package events

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
)

// BUG-2799: a SUBSCRIBE that Redis answers with an ERROR REPLY. The write
// succeeds, so BUG-2764's check on it passes, and the rejection arrives as the
// first reply. go-redis's channel goroutine drops a Receive error silently, so
// before the fix the reply never reached the bus: the establisher's
// confirmTimeout ran out and the caller was admitted as an unconfirmed
// subscription to nothing.
//
// Two rows, one for each way go-redis parses an error reply (v9.22.0,
// internal/proto/redis_errors.go): -NOPERM becomes a typed PermissionError,
// and any other -ERR becomes a generic RedisError. The fix keys on the
// redis.Error interface, which both satisfy, and so does every other error
// reply Redis or a proxy can give; MOVED, CLUSTERDOWN and LOADING are argued
// at source on BUG-2799's trail because miniredis cannot produce them.

type rejectingSubscribeServer struct {
	mr       *miniredis.Miniredis
	rejected atomic.Int64
	delay    time.Duration
}

func newRejectingSubscribeServer(t *testing.T, reply string, delay time.Duration) *rejectingSubscribeServer {
	t.Helper()
	s := &rejectingSubscribeServer{mr: miniredis.RunT(t), delay: delay}
	s.mr.Server().SetPreHook(func(p *server.Peer, cmd string, _ ...string) bool {
		if !strings.EqualFold(cmd, "SUBSCRIBE") {
			return false
		}
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		s.rejected.Add(1)
		p.WriteError(reply)
		return true
	})
	return s
}

func newRejectedSubscribeBus(t *testing.T, s *rejectingSubscribeServer) (*RedisBus, *recordingObserver) {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: s.mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	return b, obs
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureDefaultLog routes slog's default logger into a buffer for the rest of
// the test. Safe here because the tests using it are not parallel.
func captureDefaultLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func TestARejectedSubscribeRefusesItsCallerInsteadOfAdmittingIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
	}{
		{"acl denial parses to a typed PermissionError", "NOPERM User default has no permissions to access the 'pad:events:ws-1' channel"},
		{"any other error reply parses to a generic RedisError", "ERR unknown command 'SUBSCRIBE', with args beginning with: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRejectingSubscribeServer(t, tc.reply, 0)
			b, obs := newRejectedSubscribeBus(t, s)
			logs := captureDefaultLog(t)

			start := time.Now()
			ch, _, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
			elapsed := time.Since(start)

			// PREMISE: Redis rejected at least once. Checked before the
			// outcome, and only for ONE rejection, so that against the defect
			// this test fails on the outcome below, which is what the defect
			// gets wrong, rather than on a count the defect also changes.
			if got := s.rejected.Load(); got < 1 {
				t.Fatalf("SUBSCRIBE was never rejected; the instrument never armed")
			}

			// Each assertion is stated as what the defect leaves behind
			// (CONVE-12): SubscribeOK, a channel, a live wsSubs entry, and an
			// unconfirmed admission promised a reconcile that never comes.
			if outcome != SubscribeFailed {
				t.Fatalf("outcome = %v, want failed: the caller was admitted into a subscription Redis rejected", outcome)
			}
			if ch != nil {
				t.Fatal("a refused caller was handed a channel")
			}
			b.mu.Lock()
			_, live := b.wsSubs["ws-1"]
			_, inFlight := b.pendingSubs["ws-1"]
			count := b.wsCounts["ws-1"]
			b.mu.Unlock()
			if live {
				t.Fatal("a subscription Redis rejected is still installed")
			}
			if inFlight {
				t.Fatal("an establishment record was left behind: the next caller would wait on it forever")
			}
			if count != 0 {
				t.Fatalf("workspace subscriber count = %d after a refusal, want 0", count)
			}
			if got := obs.unconfirmedCount(); got != 0 {
				t.Fatalf("SubscriptionUnconfirmed reported %d times, want 0: the caller was admitted when the bound ran out rather than refused on the reply", got)
			}
			// The refusal went through the establish loop's one retry, as a
			// failed SUBSCRIBE write does (BUG-2764), before giving up.
			if got := s.rejected.Load(); got != 2 {
				t.Errorf("SUBSCRIBE rejected %d times, want 2 (initial attempt + the loop's retry)", got)
			}
			// The reply is what refused it, not the bound: two attempts that
			// each sat out confirmTimeout would take 2x it.
			if elapsed >= b.confirmTimeout {
				t.Errorf("refusal took %v, at or past the %v confirmation bound: the rejection was not seen, the wait ran out", elapsed, b.confirmTimeout)
			}
			// Logged as itself, so an operator reading the log learns WHY.
			if first := strings.Fields(tc.reply)[0]; !strings.Contains(logs.String(), first) {
				t.Errorf("the log does not carry the error reply %q:\n%s", first, logs.String())
			}
		})
	}
}

// TestALateRejectionLeavesAdmittedCallersInPlace pins the residual the fix
// deliberately leaves: a rejection that arrives AFTER confirmTimeout, once
// callers have been admitted without an acknowledgement. Taking the
// subscription down under them would wire their channels to nothing, so it
// stays installed and the rejection is only logged. Without this test the
// branch that decides this is unexercised.
func TestALateRejectionLeavesAdmittedCallersInPlace(t *testing.T) {
	s := newRejectingSubscribeServer(t, "NOPERM no permissions to access the channel", 150*time.Millisecond)
	b, obs := newRejectedSubscribeBus(t, s)
	b.confirmTimeout = 30 * time.Millisecond
	logs := captureDefaultLog(t)

	ch, _, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
	if outcome != SubscribeOK || ch == nil {
		t.Fatalf("outcome = %v (channel nil: %v), want an unconfirmed admission: the bound ran out before the reply", outcome, ch == nil)
	}
	if got := obs.unconfirmedCount(); got != 1 {
		t.Fatalf("SubscriptionUnconfirmed reported %d times, want 1", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "already admitted") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "already admitted") || !strings.Contains(logs.String(), "NOPERM") {
		t.Fatalf("the late rejection was not logged as itself:\n%s", logs.String())
	}
	b.mu.Lock()
	_, live := b.wsSubs["ws-1"]
	b.mu.Unlock()
	if !live {
		t.Fatal("a late rejection took down a subscription callers were already admitted into")
	}
}
