package server

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/metrics"
)

// readRawSSEFrames reads whole SSE frames verbatim, unlike connectSSE which
// keeps only the event type and data. The `id:` line is the subject here, so
// it cannot be parsed away.
func readRawSSEFrames(t *testing.T, ctx context.Context, url, lastEventID string) <-chan string {
	t.Helper()
	return readRawSSEFramesAuthed(t, ctx, url, lastEventID, "")
}

func readRawSSEFramesAuthed(t *testing.T, ctx context.Context, url, lastEventID, token string) <-chan string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}

	frames := make(chan string, 16)
	go func() {
		defer resp.Body.Close()
		defer close(frames)
		scanner := bufio.NewScanner(resp.Body)
		var frame []string
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if len(frame) > 0 {
					frames <- strings.Join(frame, "\n")
					frame = nil
				}
				continue
			}
			frame = append(frame, line)
		}
	}()
	return frames
}

func waitForFrameWithEvent(t *testing.T, frames <-chan string, eventType string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatalf("stream closed before a %q frame arrived", eventType)
			}
			if strings.Contains(f, "event: "+eventType) {
				return f
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %q frame", eventType)
		}
	}
}

// BUG-2731, codex round 3. A sync_required tells the client its cursor is
// unservable — and must RETIRE that cursor, or the client keeps sending it and
// every later reconnect on a quiet workspace is answered with sync_required
// again, re-running a full delta sync each time. Survivable when the response
// was rare (buffer eviction only); the coverage check makes it common.
func TestSyncRequiredClearsTheClientCursor(t *testing.T) {
	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")
	url := ts.URL + "/api/v1/events?workspace=" + slug

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A cursor this instance cannot vouch for: nothing has been published
	// here, so it belongs to a previous incarnation.
	frames := readRawSSEFrames(t, ctx, url, "4200")
	frame := waitForFrameWithEvent(t, frames, "sync_required")

	if !strings.Contains(frame, "id: \n") && !strings.HasPrefix(frame, "id:") {
		t.Fatalf("sync_required must carry an empty id: field to retire the cursor; frame was:\n%s", frame)
	}
	for _, line := range strings.Split(frame, "\n") {
		if strings.HasPrefix(line, "id:") && strings.TrimSpace(strings.TrimPrefix(line, "id:")) != "" {
			t.Fatalf("sync_required's id: field must be EMPTY, got %q", line)
		}
	}
}

// The control: an ordinary event still carries its real id, or clients would
// never advance their cursor at all.
func TestOrdinaryEventsStillCarryTheirID(t *testing.T) {
	srv := testServerWithEvents(t)
	bus := events.New()
	srv.SetEventBus(bus)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")
	url := ts.URL + "/api/v1/events?workspace=" + slug

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	frames := readRawSSEFrames(t, ctx, url, "")
	waitForFrameWithEvent(t, frames, "connected")

	ws := workspaceIDForSlug(t, srv, slug)
	bus.Publish(events.Event{Type: events.ItemCreated, WorkspaceID: ws, Collection: "tasks"})

	frame := waitForFrameWithEvent(t, frames, events.ItemCreated)
	var sawID bool
	for _, line := range strings.Split(frame, "\n") {
		if strings.HasPrefix(line, "id:") {
			sawID = true
			if strings.TrimSpace(strings.TrimPrefix(line, "id:")) == "" {
				t.Fatalf("an ordinary event must carry a real id, got %q", line)
			}
		}
	}
	if !sawID {
		t.Fatalf("an ordinary event must carry an id: field; frame was:\n%s", frame)
	}
}

// codex round 9 #7. writeSSEResetCursorEvent was exercised only through a
// happy-path handler. Its two failure branches differ in KIND and the
// difference is load-bearing: a marshal error is local to one event and must
// not tear down a healthy stream, while a write error means the peer is gone
// and the caller must exit so the bus subscription is released.
func TestWriteSSEResetCursorEventErrorPaths(t *testing.T) {
	t.Run("write failure is reported so the caller can exit", func(t *testing.T) {
		err := writeSSEResetCursorEvent(&failingWriter{err: errWriteFailed}, "sync_required", map[string]string{"reason": "x"})
		if err == nil {
			t.Fatal("a failed write must be reported; swallowing it leaves the handler pulling events for a dead peer")
		}
	})

	t.Run("marshal failure does not tear down the stream", func(t *testing.T) {
		rec := httptest.NewRecorder()
		// A channel cannot be marshalled to JSON.
		if err := writeSSEResetCursorEvent(rec, "sync_required", make(chan int)); err != nil {
			t.Fatalf("a marshal error is local to one event and must not be reported as a stream failure: %v", err)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("nothing should have been written, got %q", rec.Body.String())
		}
	})
}

// Reuses handlers_events_test.go's failingWriter (BUG-1532), which already
// exists for exactly this purpose on the sibling function.
var errWriteFailed = errors.New("connection reset by peer")

// The sync_required payload's reason text, asserted because it was CHANGED by
// this unit (it used to name buffer eviction, which is now one cause among
// several) and because nothing else looks at it — the client dispatches on the
// event name, so a silent regression here would never surface.
func TestSyncRequiredReasonDescribesTheActualCondition(t *testing.T) {
	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	frames := readRawSSEFrames(t, ctx, ts.URL+"/api/v1/events?workspace="+slug, "4200")
	frame := waitForFrameWithEvent(t, frames, "sync_required")

	if strings.Contains(frame, "buffer exceeded") {
		t.Fatalf("the reason still names buffer eviction, which is now one cause among several:\n%s", frame)
	}
	if !strings.Contains(frame, "cannot vouch") {
		t.Fatalf("the reason should describe the actual condition; frame was:\n%s", frame)
	}
}

// codex round 10 #4. A client that sends Last-Event-ID at all believes it has
// a position. If the value is unreadable, treating it as a FRESH connection
// silently drops everything published before this subscription — the same
// class of lie this unit exists to end, arriving through the parser rather
// than the buffer.
func TestAnUnreadableLastEventIDIsAGapNotAFreshConnection(t *testing.T) {
	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")
	url := ts.URL + "/api/v1/events?workspace=" + slug

	// NOT in this list: a whitespace-only value. HTTP strips optional
	// whitespace from header values, so the server sees an empty string —
	// which the EventSource spec defines as "no position", the same as an
	// absent header. Measured rather than assumed: a client sending "  "
	// reaches the handler as `Last-Event-ID: ""`. Treating that as fresh is
	// correct, so there is nothing here to assert.
	for _, cursor := range []string{"-1", "0", "not-a-number", `"42"`, "99999999999999999999999"} {
		t.Run(cursor, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			frames := readRawSSEFrames(t, ctx, url, cursor)
			frame := waitForFrameWithEvent(t, frames, "sync_required")
			// And it retires the bad cursor, or the client sends it again on
			// every reconnect forever.
			for _, line := range strings.Split(frame, "\n") {
				if strings.HasPrefix(line, "id:") && strings.TrimSpace(strings.TrimPrefix(line, "id:")) != "" {
					t.Fatalf("the unreadable cursor must be retired, got %q", line)
				}
			}
		})
	}
}

// The control: a genuinely fresh client sends NO header and must NOT be told
// to resync. Without this leg the fix above would be indistinguishable from
// resyncing everyone on connect.
func TestAFreshConnectionIsNotToldToResync(t *testing.T) {
	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	frames := readRawSSEFrames(t, ctx, ts.URL+"/api/v1/events?workspace="+slug, "")
	waitForFrameWithEvent(t, frames, "connected")

	select {
	case f, ok := <-frames:
		if ok && strings.Contains(f, "event: sync_required") {
			t.Fatalf("a fresh connection must not be told to resync:\n%s", f)
		}
	case <-time.After(500 * time.Millisecond):
	}
}

// The watch stream's half of the two contracts this unit aligned (codex round
// 11). Both were introduced on the activity stream first, which left the two
// SSE handlers silently disagreeing — the CLI masks it by clearing its own
// cursor, so the consumer this would bite is a generic SSE client, which is
// exactly the one nobody tests.
func TestWatchStreamSharesTheActivityCursorContracts(t *testing.T) {
	srv := testServerWithWatchEvents(t)
	_, _, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	token := tok.Token
	url := ts.URL + "/api/v1/events/stream"

	t.Run("an unreadable cursor is a gap", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		frames := readRawSSEFramesAuthed(t, ctx, url, "not-a-number", token)
		frame := waitForFrameWithEvent(t, frames, "sync_required")
		assertCursorRetired(t, frame)
	})

	t.Run("a cold resume is a gap and retires the cursor", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		frames := readRawSSEFramesAuthed(t, ctx, url, "4200", token)
		frame := waitForFrameWithEvent(t, frames, "sync_required")
		assertCursorRetired(t, frame)
	})

	t.Run("a fresh connection is not told to resync", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		frames := readRawSSEFramesAuthed(t, ctx, url, "", token)
		waitForFrameWithEvent(t, frames, "connected")
		select {
		case f, ok := <-frames:
			if ok && strings.Contains(f, "event: sync_required") {
				t.Fatalf("a fresh watch connection must not be told to resync:\n%s", f)
			}
		case <-time.After(500 * time.Millisecond):
		}
	})
}

func assertCursorRetired(t *testing.T, frame string) {
	t.Helper()
	var sawID bool
	for _, line := range strings.Split(frame, "\n") {
		if strings.HasPrefix(line, "id:") {
			sawID = true
			if strings.TrimSpace(strings.TrimPrefix(line, "id:")) != "" {
				t.Fatalf("sync_required must retire the cursor with an EMPTY id, got %q", line)
			}
		}
	}
	if !sawID {
		t.Fatalf("sync_required must carry an empty id: field to retire the cursor; frame was:\n%s", frame)
	}
}

// codex round 12. The handlers now answer sync_required for a cursor they
// could not parse — a decision the BUS never sees, so without counting it here
// the resume-gap counters undercount exactly the resyncs an operator is most
// likely to be asked about: a client looping because it keeps sending a cursor
// nobody can read.
func TestHandlerLevelResumeGapsAreCounted(t *testing.T) {
	t.Run("activity", func(t *testing.T) {
		srv := testServerWithEvents(t)
		m := metrics.New()
		srv.SetMetrics(m)
		ts := httptest.NewServer(srv)
		defer ts.Close()

		slug := createTestWorkspace(t, ts.URL, "Test")
		url := ts.URL + "/api/v1/events?workspace=" + slug

		// Control first: a fresh connection is not a gap, so a non-zero
		// reading below cannot be this test's own setup.
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			frames := readRawSSEFrames(t, ctx, url, "")
			waitForFrameWithEvent(t, frames, "connected")
		}()
		assertGapCount(t, m, "pad_event_resume_gaps_total", 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		frames := readRawSSEFrames(t, ctx, url, "not-a-number")
		waitForFrameWithEvent(t, frames, "sync_required")
		assertGapCount(t, m, "pad_event_resume_gaps_total", 1)
	})

	t.Run("watch", func(t *testing.T) {
		srv := testServerWithWatchEvents(t)
		_, _, tok, _ := setupWatchTestUser(t, srv)
		m := metrics.New()
		srv.SetMetrics(m)
		ts := httptest.NewServer(srv)
		defer ts.Close()

		url := ts.URL + "/api/v1/events/stream"
		assertGapCount(t, m, "pad_watchevents_resume_gaps_total", 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		frames := readRawSSEFramesAuthed(t, ctx, url, "not-a-number", tok.Token)
		waitForFrameWithEvent(t, frames, "sync_required")
		assertGapCount(t, m, "pad_watchevents_resume_gaps_total", 1)
	})
}

func assertGapCount(t *testing.T, m *metrics.Metrics, name string, want float64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var got float64
	for time.Now().Before(deadline) {
		got = 0
		families, err := m.Registry.Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		for _, f := range families {
			if f.GetName() != name {
				continue
			}
			for _, metric := range f.GetMetric() {
				got += metric.GetCounter().GetValue()
			}
		}
		if got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s = %v, want %v", name, got, want)
}
