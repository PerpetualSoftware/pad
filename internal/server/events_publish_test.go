package server

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
)

type capturedLog struct {
	mu    sync.Mutex
	lines []map[string]any
}

func (c *capturedLog) log(msg string, args ...any) {
	line := map[string]any{"msg": msg}
	for i := 0; i+1 < len(args); i += 2 {
		line[fmt.Sprint(args[i])] = args[i+1]
	}
	c.mu.Lock()
	c.lines = append(c.lines, line)
	c.mu.Unlock()
}

func (c *capturedLog) snapshot() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.lines...)
}

// BUG-2732, the lead's addition: the failure log is rate-bounded, because
// shutdown and a Redis outage both fail every publish at once, and the bound
// is tested. A burst of publishFailureLogBurst lines, then one per
// publishFailureLogEvery, each reporting what it suppressed.
func TestPublishFailureLogIsRateBounded(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var logs capturedLog
	l := &publishFailureLog{now: func() time.Time { return now }, log: logs.log}
	e := events.Event{Type: events.ItemUpdated, WorkspaceID: "ws-1"}

	const storm = 50
	for range storm {
		l.record(events.ErrBusClosed, e)
	}
	got := logs.snapshot()
	if len(got) != publishFailureLogBurst {
		t.Fatalf("a storm of %d at one instant logged %d lines, want the burst of %d", storm, len(got), publishFailureLogBurst)
	}
	for i, line := range got {
		if s := line["suppressed_since_last"]; s != int64(0) {
			t.Fatalf("line %d inside the burst reports suppressed=%v, want 0", i, s)
		}
	}

	// Less than one interval later: still suppressed.
	now = now.Add(publishFailureLogEvery / 2)
	l.record(events.ErrBusClosed, e)
	if n := len(logs.snapshot()); n != publishFailureLogBurst {
		t.Fatalf("half an interval later logged again (%d lines)", n)
	}

	// One full interval after the burst: exactly one line, carrying the count
	// of everything it stood in for.
	now = now.Add(publishFailureLogEvery / 2)
	l.record(events.ErrBusClosed, e)
	got = logs.snapshot()
	if len(got) != publishFailureLogBurst+1 {
		t.Fatalf("after one interval: %d lines, want %d", len(got), publishFailureLogBurst+1)
	}
	wantSuppressed := int64(storm - publishFailureLogBurst + 1)
	if s := got[len(got)-1]["suppressed_since_last"]; s != wantSuppressed {
		t.Fatalf("suppressed_since_last = %v, want %d", s, wantSuppressed)
	}

	// The count is reset once reported: a later line with nothing suppressed
	// in between reports zero, not the running total.
	now = now.Add(publishFailureLogEvery)
	l.record(events.ErrBusClosed, e)
	got = logs.snapshot()
	if s := got[len(got)-1]["suppressed_since_last"]; s != int64(0) {
		t.Fatalf("a line after a quiet interval reports suppressed=%v, want 0", s)
	}
}

// The line names outcome, event type and workspace, and never the payload.
func TestPublishFailureLogCarriesNoPayload(t *testing.T) {
	var logs capturedLog
	l := &publishFailureLog{log: logs.log}
	l.record(fmt.Errorf("events: redis publish: %w", fmt.Errorf("connection refused")), events.Event{
		Type:        events.ItemCreated,
		WorkspaceID: "ws-1",
		ItemID:      "item-secret-id",
		Title:       "Secret title",
		Actor:       "user",
		ActorName:   "Secret Person",
	})
	got := logs.snapshot()
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1", len(got))
	}
	line := got[0]
	want := map[string]any{"outcome": "unconfirmed", "event_type": events.ItemCreated, "workspace": "ws-1"}
	for k, v := range want {
		if line[k] != v {
			t.Fatalf("%s = %v, want %v", k, line[k], v)
		}
	}
	rendered := fmt.Sprint(line)
	for _, secret := range []string{"Secret title", "Secret Person", "item-secret-id"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("the log line carries payload %q: %s", secret, rendered)
		}
	}
}

// CONVE-19: wiring, not just the helper. A real handler whose publish fails
// still answers success, and the failure reaches the helper's log.
func TestItemCreateSucceedsAndLogsWhenThePublishFails(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	bus := events.New()
	srv.SetEventBus(bus)
	var logs capturedLog
	srv.publishFailures.log = logs.log
	slug := createWSWithCollections(t, srv)

	// Positive control: a live bus logs nothing.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title": "before close",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create on a live bus: %d %s", rr.Code, rr.Body.String())
	}
	if n := len(logs.snapshot()); n != 0 {
		t.Fatalf("a live bus logged %d publish failures", n)
	}

	bus.Close()
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title": "after close",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("a failed publish must not fail the committed write: %d %s", rr.Code, rr.Body.String())
	}
	got := logs.snapshot()
	if len(got) == 0 {
		t.Fatal("the handler's publish failure never reached publishActivityEvent's log")
	}
	if got[0]["outcome"] != "closed" || got[0]["event_type"] != events.ItemCreated {
		t.Fatalf("unexpected line: %v", got[0])
	}
}
