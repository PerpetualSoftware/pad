package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
)

// TASK-2197: a client that passes heartbeat=1 gets each keepalive as a named
// `heartbeat` event, which EventSource surfaces, so it can tell a quiet stream
// from a dead one. Without the parameter the keepalive stays an SSE comment,
// byte for byte, because `pad project watch` reads the same stream.

// firstKeepalive reads the raw stream until a keepalive frame and returns its
// first line.
func firstKeepalive(t *testing.T, url string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := isolatedTestClient().Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == ": keepalive" || line == "event: heartbeat" {
			if line == "event: heartbeat" && sc.Scan() && sc.Text() != "data: {}" {
				t.Fatalf("heartbeat data line = %q, want %q", sc.Text(), "data: {}")
			}
			return line
		}
	}
	t.Fatalf("no keepalive frame before the stream ended: %v", sc.Err())
	return ""
}

func TestSSE_TASK2197_HeartbeatIsOptIn(t *testing.T) {
	srv := testServerWithEvents(t)
	srv.sseKeepaliveOverride = 50 * time.Millisecond
	ts := httptest.NewServer(srv)
	defer ts.Close()
	slug := createTestWorkspace(t, ts.URL, "Heartbeat")
	base := ts.URL + "/api/v1/events?workspace=" + slug

	if got := firstKeepalive(t, base); got != ": keepalive" {
		t.Errorf("without heartbeat=1 the keepalive is %q, want the comment unchanged", got)
	}
	if got := firstKeepalive(t, base+"&heartbeat=1"); got != "event: heartbeat" {
		t.Errorf("with heartbeat=1 the keepalive is %q, want a named heartbeat event", got)
	}
	if got := firstKeepalive(t, base+"&heartbeat=yes"); !strings.HasPrefix(got, ": ") {
		t.Errorf("only heartbeat=1 opts in; heartbeat=yes gave %q", got)
	}
}

// A heartbeat must arrive while events are flowing faster than the keepalive
// interval: a ticker that every event write RESET would fire only after a full
// interval of silence, so on a busy stream it would never fire at all.
//
// Deterministic (BUG-3503). This used to drive load through the HTTP API every
// 20ms and fail if a heartbeat came after fewer than 3 item events, which a
// loaded -race runner tripped (item creation was slower than the interval).
// Events are now published straight to the bus at a fixed cadence, cheap
// enough to keep up under -race, and the claim is judged per heartbeat: one
// QUALIFIES when at least minItems events came before it and the last one came
// less than half an interval earlier. A heartbeat that does not qualify (the
// runner stalled the publisher) is not evidence either way and is skipped; the
// test fails only if no heartbeat qualifies within the deadline. A
// reset-on-write ticker can never produce a qualifying heartbeat.
func TestSSE_TASK2197_HeartbeatsArriveOnABusyStream(t *testing.T) {
	const (
		interval = 200 * time.Millisecond
		minItems = 5
	)
	srv := testServerWithEvents(t)
	srv.sseKeepaliveOverride = interval
	ts := httptest.NewServer(srv)
	defer ts.Close()
	slug := createTestWorkspace(t, ts.URL, "Busy")
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("workspace: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/v1/events?workspace="+slug+"&heartbeat=1", nil)
	resp, err := isolatedTestClient().Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tick := time.NewTicker(interval / 10)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				srv.publishActivityEvent(events.Event{Type: "item_updated", WorkspaceID: ws.ID, ItemID: "busy-item", Title: "busy"})
			}
		}
	}()

	sc := bufio.NewScanner(resp.Body)
	items, skipped := 0, 0
	var lastItem time.Time
	for sc.Scan() {
		switch sc.Text() {
		case "event: item_updated":
			items++
			lastItem = time.Now()
		case "event: heartbeat":
			if items >= minItems && time.Since(lastItem) < interval/2 {
				return // a heartbeat in the middle of a busy stream
			}
			skipped++
		}
	}
	t.Fatalf("no heartbeat arrived while events were flowing (%d item events, %d heartbeats did not qualify): %v", items, skipped, sc.Err())
}
