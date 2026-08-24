package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
)

// slowEstablishBus stands in for a bus whose establishment is genuinely slow —
// a cold workspace whose Redis subscription has to be dialled and then
// acknowledged. It reproduces the real bus's contract rather than a stub's:
// the wait is cancellable, and cancelling it yields SubscribeCancelled.
//
// The hold is what makes the test discriminate. A handler that passes the
// request's context through returns as soon as the client hangs up; one that
// passes context.Background() cannot, and pays the whole hold with the
// admission slot still reserved.
type slowEstablishBus struct {
	events.EventBus
	hold time.Duration

	once    sync.Once
	entered chan struct{}
}

func (b *slowEstablishBus) wait(ctx context.Context) (events.SubscribeOutcome, bool) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-ctx.Done():
		return events.SubscribeCancelled, false
	case <-time.After(b.hold):
		return events.SubscribeOK, true
	}
}

func (b *slowEstablishBus) SubscribeIfAllowed(ctx context.Context, workspaceID string, maxPerWorkspace int) (chan events.Event, <-chan struct{}, events.SubscribeOutcome) {
	if outcome, proceed := b.wait(ctx); !proceed {
		return nil, nil, outcome
	}
	return b.EventBus.SubscribeIfAllowed(ctx, workspaceID, maxPerWorkspace)
}

func (b *slowEstablishBus) SubscribeAndReplaySince(ctx context.Context, workspaceID string, sinceID int64, maxPerWorkspace int) (chan events.Event, []events.Event, <-chan struct{}, events.SubscribeOutcome) {
	if outcome, proceed := b.wait(ctx); !proceed {
		return nil, nil, nil, outcome
	}
	return b.EventBus.SubscribeAndReplaySince(ctx, workspaceID, sinceID, maxPerWorkspace)
}

// TestDisconnectDuringEstablishmentReleasesTheAdmissionSlot is the BINDING
// assertion for BUG-2749 (CONVE-19): internal/events' own tests vouch for the
// bus honouring a cancelled context, and say nothing about whether the SSE
// handler ever hands it one.
//
// The admission slot is the half of the bug that lives in this package. It is
// reserved before the subscribe and released by a defer, so for as long as
// establishment ran to completion for a departed client, a connection that no
// longer existed went on counting against both the per-instance and the
// per-principal bound.
//
// ASSERTS ITS OWN PREMISES, both of them: that establishment was actually
// entered, and that the slot was actually held at that moment. Without those
// the timing check below would pass against a handler that refused the
// connection outright and never reserved anything.
func TestDisconnectDuringEstablishmentReleasesTheAdmissionSlot(t *testing.T) {
	srv := testServerWithEvents(t)
	bus := &slowEstablishBus{
		EventBus: events.New(),
		// Long enough that a handler ignoring the request context cannot
		// finish inside the release deadline below by luck.
		hold:    5 * time.Second,
		entered: make(chan struct{}),
	}
	srv.SetEventBus(bus)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events?workspace="+slug, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

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
		t.Fatal("establishment was never entered; this test never reached the window it is named for")
	}

	if held := srv.admission().heldTotal(); held != 1 {
		cancel()
		t.Fatalf("admission slots held during establishment = %d, want 1 — the slot this test is about was never reserved", held)
	}

	// The client goes away mid-establishment.
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if srv.admission().heldTotal() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the admission slot was still held %v after the client disconnected, with %v of establishment left to run: the handler is not passing the request's context to the bus", 2*time.Second, bus.hold)
		}
		time.Sleep(2 * time.Millisecond)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("the request never completed")
	}
}
