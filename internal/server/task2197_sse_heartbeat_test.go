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
