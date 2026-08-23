package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/metrics"
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
	m := metrics.New()
	srv.SetMetrics(m)
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

	resumeBefore := counterValue(t, m.EventResumeGapsTotal)
	bus.gaps <- struct{}{}

	frame := waitForFrameWithEvent(t, frames, "sync_required")
	assertRetiresCursor(t, frame)

	if got := counterValue(t, m.EventMidstreamResyncsTotal); got != 1 {
		t.Errorf("pad_event_midstream_resyncs_total = %v, want 1", got)
	}
	// The RESUME counter must not move: existing alerts are written against
	// it and this was not a resume (codex round 4).
	if got := counterValue(t, m.EventResumeGapsTotal); got != resumeBefore {
		t.Errorf("a mid-stream gap moved the resume counter: %v -> %v", resumeBefore, got)
	}

	// THE STREAM MUST STAY OPEN (codex round 3). sync_required tells the
	// client to reconcile, not that the connection is over — and a handler
	// that emitted it and then returned would have passed everything above.
	// An ordinary event arriving afterwards is the proof.
	inner.Publish(events.Event{
		Type:        events.ItemCreated,
		WorkspaceID: workspaceIDForSlug(t, srv, slug),
		Collection:  "tasks",
	})
	waitForFrameWithEvent(t, frames, events.ItemCreated)
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

	// Same liveness claim as the activity twin (codex round 3). This stream
	// has no cheap ordinary event to publish — a notification needs a watch
	// predicate to match — so the assertion is that the handler did not
	// return: a closed frames channel is what a returned handler looks like
	// from here.
	assertStreamStillOpen(t, frames, 300*time.Millisecond)
}

// assertStreamStillOpen fails if the frame channel closes within the window,
// which is what a handler returning looks like to a reader.
func assertStreamStillOpen(t *testing.T, frames <-chan string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case _, ok := <-frames:
			if !ok {
				t.Fatal("the stream closed after the mid-stream sync_required; " +
					"the signal tells a client to reconcile, not that the connection is over")
			}
		case <-deadline:
			return
		}
	}
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

// TestRefusedConnectionDoesNotCountASyncRequired is codex round 1's finding,
// pinned. The counter's population is "sync_required signals SENT". Moving the
// cursor parse above the subscribe (which is what closes the duplicate window)
// put the unreadable-cursor increment on the wrong side of the admission
// check, so a connection refused with 429 — which is never sent anything —
// still moved it.
//
// The control leg is the second half: an unreadable cursor on a connection
// that IS admitted must still count, or the fix would read as correct while
// having simply deleted the increment.
//
// SCOPE, recorded because codex round 3 raised it: origin/main also does not
// count a refused connection — it parsed the cursor after admission, so the
// question never arose there. This test therefore does NOT argue for the
// parse-before-subscribe ordering; it is a regression test for the defect that
// ordering introduced, and it fails if the increment moves back.
func TestRefusedConnectionDoesNotCountASyncRequired(t *testing.T) {
	srv := testServerWithEvents(t)
	m := metrics.New()
	srv.SetMetrics(m)
	srv.SetEventBus(events.New())
	srv.SetSSELimits(0, 1, 0) // one connection per workspace
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")
	url := ts.URL + "/api/v1/events?workspace=" + slug

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Holder occupies the workspace's only slot. Its cursor is readable, so
	// it cannot contribute to the counter itself.
	held := readRawSSEFrames(t, ctx, url, "")
	waitForFrameWithEvent(t, held, "connected")

	before := counterValue(t, m.EventResumeGapsTotal)

	// Refused: the slot is taken. The unreadable cursor must not be counted,
	// because nothing was sent to this client but a 429.
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Last-Event-ID", "not-a-number")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("want the connection refused with 429, got %d — the test is not exercising its case", resp.StatusCode)
	}
	if got := counterValue(t, m.EventResumeGapsTotal); got != before {
		t.Errorf("a refused connection incremented the sync_required counter: %v -> %v", before, got)
	}

	// Control: free the slot, reconnect with the same unreadable cursor. This
	// one IS admitted and IS sent sync_required, so it must count.
	cancel()
	waitUntilWorkspaceIdle(t, srv, slug)

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	frames := readRawSSEFrames(t, ctx2, url, "not-a-number")
	waitForFrameWithEvent(t, frames, "sync_required")
	if got := counterValue(t, m.EventResumeGapsTotal); got <= before {
		t.Errorf("an admitted unreadable cursor did not increment the counter: %v -> %v", before, got)
	}
}

// waitUntilWorkspaceIdle waits for the held connection's Unsubscribe to land.
// Cancelling the request context returns from the client side immediately; the
// handler's own teardown is what frees the slot.
func waitUntilWorkspaceIdle(t *testing.T, srv *Server, slug string) {
	t.Helper()
	ws := workspaceIDForSlug(t, srv, slug)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if srv.events.WorkspaceSubscriberCount(ws) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the held connection never released its workspace slot")
}

// The announcer is tested directly in stream_gap_announcer_test.go, which
// vouches for the limiter and says nothing about whether either handler USES
// it (team CONVE-19). A handler that emitted sync_required straight from
// `case <-gaps:` would pass every other test in this file and reopen the
// feedback loop the limiter exists to prevent, so both handlers get this.
func TestActivityStreamRateLimitsGapAnnouncements(t *testing.T) {
	srv := testServerWithEvents(t)
	srv.midStreamGapCooldownOverride = 250 * time.Millisecond
	bus := &gapEventBus{EventBus: events.New(), gaps: make(chan struct{}, 1)}
	srv.SetEventBus(bus)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	frames := readRawSSEFrames(t, ctx, ts.URL+"/api/v1/events?workspace="+slug, "")
	waitForFrameWithEvent(t, frames, "connected")

	assertGapBurstYieldsOneThenOne(t, frames, bus.gaps, 250*time.Millisecond)
}

func TestWatchStreamRateLimitsGapAnnouncements(t *testing.T) {
	srv := testServer(t)
	srv.midStreamGapCooldownOverride = 250 * time.Millisecond
	bus := &gapWatchBus{Bus: watchevents.New(), gaps: make(chan struct{}, 1)}
	srv.SetWatchEventsBus(bus)
	_, _, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	frames := readRawSSEFramesAuthed(t, ctx, ts.URL+"/api/v1/events/stream", "", tok.Token)
	waitForFrameWithEvent(t, frames, "connected")

	assertGapBurstYieldsOneThenOne(t, frames, bus.gaps, 250*time.Millisecond)
}

// assertGapBurstYieldsOneThenOne drives a burst of gaps through one connection
// and asserts both halves of the bound: exactly ONE announcement inside the
// window (a handler that ignored the limiter would send three), and one MORE
// after it (a handler that simply dropped the extras would send none, which is
// this fix's own defect one layer up).
func assertGapBurstYieldsOneThenOne(t *testing.T, frames <-chan string, gaps chan struct{}, cooldown time.Duration) {
	t.Helper()

	// The bus channel is capacity 1 and coalescing, so a burst is delivered by
	// raising it again as soon as the handler has taken the previous one. Three
	// raises means at least two gaps land inside the first window.
	for range 3 {
		gaps <- struct{}{}
	}

	waitForFrameWithEvent(t, frames, "sync_required")

	// Nothing more until the window closes. Deliberately less than the
	// cooldown, so this leg fails for an unlimited handler and cannot pass by
	// simply outlasting the timer.
	assertNoFrameWithEvent(t, frames, "sync_required", cooldown/2)

	// ...and then the latched one, so the burst was bounded rather than
	// silently discarded.
	waitForFrameWithEvent(t, frames, "sync_required")
}
