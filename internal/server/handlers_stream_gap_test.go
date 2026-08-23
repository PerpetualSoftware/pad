package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// The two buses' gap detection is tested where it lives (internal/events,
// internal/watchevents). What CANNOT be tested there, and is a separate claim
// per team CONVE-19, is the WIRING: that the signal a bus raises actually
// reaches the wire as a sync_required frame. A bus tested at the bus vouches
// for the component, not for its binding to the handler.
//
// So these drive the real handlers through ServeHTTP over a real HTTP server,
// and fake only the bus — specifically only its gap channel, so the handler's
// select, the SSE writer, the empty-id cursor retirement and the flush are all
// the production ones.
//
// Faking rather than provoking a genuine drop is deliberate and worth stating:
// a real slow-subscriber drop needs the handler to stop draining its channel,
// which means blocking the HTTP write, which means filling a TCP window with
// keepalives — an amount of arrangement that would make the test's failures
// say more about socket buffers than about this code.

// gapEventBus hands the handler a gap channel the test controls. Everything
// else is the real in-process bus.
type gapEventBus struct {
	events.EventBus
	gaps chan struct{}
}

func (b *gapEventBus) SubscribeIfAllowed(workspaceID string, maxPerWorkspace int) (chan events.Event, <-chan struct{}, bool) {
	ch, _, ok := b.EventBus.SubscribeIfAllowed(workspaceID, maxPerWorkspace)
	return ch, b.gaps, ok
}

func (b *gapEventBus) SubscribeAndReplaySince(workspaceID string, sinceID int64, maxPerWorkspace int) (chan events.Event, []events.Event, <-chan struct{}, bool) {
	ch, missed, _, ok := b.EventBus.SubscribeAndReplaySince(workspaceID, sinceID, maxPerWorkspace)
	return ch, missed, b.gaps, ok
}

// gapWatchBus is the same seam for the user-scoped watch stream.
type gapWatchBus struct {
	watchevents.Bus
	gaps chan struct{}
}

func (b *gapWatchBus) Subscribe() (chan watchevents.Notification, <-chan struct{}) {
	ch, _ := b.Bus.Subscribe()
	return ch, b.gaps
}

func (b *gapWatchBus) SubscribeAndReplaySince(sinceID int64) (chan watchevents.Notification, []watchevents.Notification, <-chan struct{}) {
	ch, missed, _ := b.Bus.SubscribeAndReplaySince(sinceID)
	return ch, missed, b.gaps
}

// TestActivityStreamAnnouncesAGapMidStream is the binding, asserted from the
// consuming side: a gap raised on an OPEN connection reaches the client as
// sync_required, with the empty id: that retires a cursor we have just said we
// cannot vouch for.
func TestActivityStreamAnnouncesAGapMidStream(t *testing.T) {
	srv := testServerWithEvents(t)
	bus := &gapEventBus{EventBus: events.New(), gaps: make(chan struct{}, 1)}
	srv.SetEventBus(bus)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	frames := readRawSSEFrames(t, ctx, ts.URL+"/api/v1/events?workspace="+slug, "")
	waitForFrameWithEvent(t, frames, "connected")

	bus.gaps <- struct{}{}

	frame := waitForFrameWithEvent(t, frames, "sync_required")
	assertRetiresCursor(t, frame)
}

// TestActivityStreamStaysQuietWithoutAGap is the control. Without it the test
// above would pass for a handler that sends sync_required unconditionally,
// which would be a worse bug than the one being fixed — every connection would
// trigger a delta sync.
//
// It proves the harness CAN see a frame (an ordinary event arrives) before
// concluding that the absent one is absent.
func TestActivityStreamStaysQuietWithoutAGap(t *testing.T) {
	srv := testServerWithEvents(t)
	inner := events.New()
	bus := &gapEventBus{EventBus: inner, gaps: make(chan struct{}, 1)}
	srv.SetEventBus(bus)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	frames := readRawSSEFrames(t, ctx, ts.URL+"/api/v1/events?workspace="+slug, "")
	waitForFrameWithEvent(t, frames, "connected")

	// FIRST, before anything else is published: a handler that announces a
	// gap unconditionally at connect must fail here. Asserting this only
	// after publishing would let such a frame slip past while the loop was
	// looking for the event — which is exactly what happened when this leg
	// was mutation-tested, so the ordering is load-bearing, not stylistic.
	assertNoFrameWithEvent(t, frames, "sync_required", 300*time.Millisecond)

	inner.Publish(events.Event{
		Type:        events.ItemCreated,
		WorkspaceID: workspaceIDForSlug(t, srv, slug),
		Collection:  "tasks",
	})

	// The ordinary event must arrive — that is the proof this loop CAN see a
	// frame, so the absence asserted above is an absence and not a blind
	// spot — and no gap may be announced alongside it.
	frame := waitForFrameRefusing(t, frames, events.ItemCreated, "sync_required")
	if strings.Contains(frame, "sync_required") {
		t.Fatalf("an ordinary event was announced as a gap:\n%s", frame)
	}
}

// waitForFrameRefusing is waitForFrameWithEvent with a forbidden type: it
// fails if `refuse` arrives while waiting for `want`, instead of reading past
// it. The plain version reads past everything, which makes it unusable for
// asserting that something did NOT happen in a window that also contains
// something that did.
func waitForFrameRefusing(t *testing.T, frames <-chan string, want, refuse string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatalf("stream closed before a %q frame arrived", want)
			}
			if strings.Contains(f, "event: "+refuse) {
				t.Fatalf("unexpected %q frame while waiting for %q:\n%s", refuse, want, f)
			}
			if strings.Contains(f, "event: "+want) {
				return f
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %q frame", want)
		}
	}
}

// TestWatchStreamAnnouncesAGapMidStream is the same binding on the user-scoped
// stream, whose handler is a different function with its own select loop.
func TestWatchStreamAnnouncesAGapMidStream(t *testing.T) {
	srv := testServer(t)
	bus := &gapWatchBus{Bus: watchevents.New(), gaps: make(chan struct{}, 1)}
	srv.SetWatchEventsBus(bus)
	_, _, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	frames := readRawSSEFramesAuthed(t, ctx, ts.URL+"/api/v1/events/stream", "", tok.Token)
	waitForFrameWithEvent(t, frames, "connected")

	bus.gaps <- struct{}{}

	frame := waitForFrameWithEvent(t, frames, "sync_required")
	assertRetiresCursor(t, frame)
}

// TestWatchStreamStaysQuietWithoutAGap is that handler's control leg.
func TestWatchStreamStaysQuietWithoutAGap(t *testing.T) {
	srv := testServer(t)
	bus := &gapWatchBus{Bus: watchevents.New(), gaps: make(chan struct{}, 1)}
	srv.SetWatchEventsBus(bus)
	_, _, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	frames := readRawSSEFramesAuthed(t, ctx, ts.URL+"/api/v1/events/stream", "", tok.Token)
	waitForFrameWithEvent(t, frames, "connected")

	assertNoFrameWithEvent(t, frames, "sync_required", 300*time.Millisecond)
}

// assertRetiresCursor checks the frame carries an id: line whose value is
// empty. A mid-stream sync_required that left the cursor standing would have
// the client keep resending a position the server has just disclaimed.
func assertRetiresCursor(t *testing.T, frame string) {
	t.Helper()
	sawID := false
	for _, line := range strings.Split(frame, "\n") {
		if !strings.HasPrefix(line, "id:") {
			continue
		}
		sawID = true
		if v := strings.TrimSpace(strings.TrimPrefix(line, "id:")); v != "" {
			t.Fatalf("mid-stream sync_required must retire the cursor with an EMPTY id:, got %q", line)
		}
	}
	if !sawID {
		t.Fatalf("mid-stream sync_required carried no id: field at all, so the cursor is never retired:\n%s", frame)
	}
}

// assertNoFrameWithEvent fails if a frame of the given type arrives within the
// window. Frames of other types are read past, not treated as the absence.
func assertNoFrameWithEvent(t *testing.T, frames <-chan string, eventType string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return
			}
			if strings.Contains(f, "event: "+eventType) {
				t.Fatalf("unexpected %q frame:\n%s", eventType, f)
			}
		case <-deadline:
			return
		}
	}
}
