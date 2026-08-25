package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// slowSettleWatchBus stands in for the Redis watch bus during its resume
// settle window: a wait that is cancellable, on the caller's context.
//
// It reproduces the real contract rather than a stub's. RedisBus's
// resumeOutrunsLocalView does a Redis GET, waits `settleWindow`, then does a
// second GET — and until BUG-2751 that wait was bounded by the BUS's lifetime,
// so a departed client kept paying for it.
type slowSettleWatchBus struct {
	watchevents.Bus
	hold time.Duration

	once    sync.Once
	entered chan struct{}
}

func (b *slowSettleWatchBus) SubscribeAndReplaySince(ctx context.Context, sinceID int64) (chan watchevents.Notification, []watchevents.Notification, <-chan struct{}) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-ctx.Done():
		// What the real bus does on a cancelled settle: stop waiting and
		// answer from what it already has. The handler is unwinding anyway.
		return b.Bus.SubscribeAndReplaySince(ctx, sinceID)
	case <-time.After(b.hold):
		return b.Bus.SubscribeAndReplaySince(ctx, sinceID)
	}
}

// TestDisconnectDuringTheResumeSettleReleasesTheAdmissionSlot is BUG-2751's
// BINDING assertion, and the half of the bug that lives in this package.
//
// internal/watchevents' own tests vouch for the bus honouring a cancelled
// context; they say nothing about whether this handler ever hands it one. That
// is the same gap CONVE-19 names, and the reason BUG-2749 needed a twin of this
// test for the other stream.
//
// The slot is the cost. It is reserved before the subscribe and released by
// defer, so for as long as the settle window ran to completion for a departed
// client, a connection that no longer existed counted against both the
// per-instance and the per-principal bound (BUG-2726).
//
// ASSERTS ITS OWN PREMISES, both of them: that the settle was actually entered,
// and that the slot was actually held at that moment. Without those the timing
// check below would pass against a handler that refused the connection outright
// and never reserved anything.
func TestDisconnectDuringTheResumeSettleReleasesTheAdmissionSlot(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	_, _, tok, _ := setupWatchTestUser(t, srv)

	bus := &slowSettleWatchBus{
		Bus: watchevents.New(),
		// Long enough that a handler ignoring the request context cannot
		// finish inside the release deadline below by luck.
		hold:    5 * time.Second,
		entered: make(chan struct{}),
	}
	srv.SetWatchEventsBus(bus)

	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	// A resume, which is the only path that reaches the settle window at all.
	// Without this header the handler takes plain Subscribe and this test would
	// pass against every implementation.
	req.Header.Set("Last-Event-ID", "1")

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-bus.entered:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the resume path was never entered; this test never reached the window it is named for")
	}

	if held := srv.admission().heldTotal(); held != 1 {
		cancel()
		t.Fatalf("admission slots held during the settle = %d, want 1 — the slot this test is about was never reserved", held)
	}

	// The client goes away mid-settle.
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if srv.admission().heldTotal() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the admission slot was still held %v after the client disconnected, with %v of the bus hold left to run: "+
				"capacity is reserved for a connection that no longer exists",
				2*time.Second, bus.hold)
		}
		time.Sleep(5 * time.Millisecond)
	}
	<-done
}
