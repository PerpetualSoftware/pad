package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// Codex r1 asked whether a stream busy with other events still gets
// heartbeats. The keepalive ticker is never reset by an event write, so a
// heartbeat arrives on schedule however much else the stream carries; this
// keeps it that way.
func TestSSE_TASK2197_HeartbeatsArriveOnABusyStream(t *testing.T) {
	srv := testServerWithEvents(t)
	srv.sseKeepaliveOverride = 200 * time.Millisecond
	ts := httptest.NewServer(srv)
	defer ts.Close()
	slug := createTestWorkspace(t, ts.URL, "Busy")

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				resp := apiRequest(t, ts.URL, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]any{"title": "busy", "fields": "{}"})
				resp.Body.Close()
			}
		}
	}()
	// Read the stream: item events must flow, and a heartbeat must still come.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/v1/events?workspace="+slug+"&heartbeat=1", nil)
	resp, err := isolatedTestClient().Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	items := 0
	for sc.Scan() {
		switch sc.Text() {
		case "event: item_created":
			items++
		case "event: heartbeat":
			if items < 3 {
				t.Fatalf("heartbeat after only %d item events; the stream was not busy, so this proves nothing", items)
			}
			return
		}
	}
	t.Fatalf("no heartbeat on a busy stream (%d item events seen): %v", items, sc.Err())
}
