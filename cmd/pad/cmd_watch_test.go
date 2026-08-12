package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeSSEResponse wraps a raw SSE body string in an *http.Response
// shaped enough for streamWatchEvents to read (it only touches .Body).
func fakeSSEResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestPadddBackoff_MonotonicUntilCap asserts the backoff grows with each
// attempt and is capped — the decision logic behind "padd unreachable →
// backoff retry, print nothing" (DOC-2479), unit-tested per the
// dispatcher's ask instead of literally sleeping.
func TestPadddBackoff_MonotonicUntilCap(t *testing.T) {
	prev := time.Duration(0)
	for attempt := 1; attempt <= 200; attempt++ {
		d := padddBackoff(attempt)
		if d < prev {
			t.Fatalf("attempt %d: backoff %v is less than previous %v — expected non-decreasing", attempt, d, prev)
		}
		if d > padddBackoffCap {
			t.Fatalf("attempt %d: backoff %v exceeds cap %v", attempt, d, padddBackoffCap)
		}
		prev = d
	}
	if got := padddBackoff(1000); got != padddBackoffCap {
		t.Fatalf("expected a large attempt count to saturate at the cap %v, got %v", padddBackoffCap, got)
	}
}

func TestPadddBackoff_NonPositiveAttemptTreatedAsFirst(t *testing.T) {
	if got, want := padddBackoff(0), padddBackoff(1); got != want {
		t.Fatalf("padddBackoff(0) = %v, want padddBackoff(1) = %v", got, want)
	}
	if got, want := padddBackoff(-5), padddBackoff(1); got != want {
		t.Fatalf("padddBackoff(-5) = %v, want padddBackoff(1) = %v", got, want)
	}
}

func TestNoPadTomlRetryInterval_IsAnHour(t *testing.T) {
	// Pinned as an explicit assertion (not just "compiles") so a future
	// edit to the constant fails a test, not just silently changes
	// DOC-2479's specified cadence.
	if noPadTomlRetryInterval != time.Hour {
		t.Fatalf("expected the no-.pad.toml retry interval to be exactly 1 hour (DOC-2479), got %v", noPadTomlRetryInterval)
	}
}

func TestFormatMonitorLine(t *testing.T) {
	cases := []struct {
		name string
		in   watchStreamPayload
		want string
	}{
		{
			name: "status change",
			in:   watchStreamPayload{ItemRef: "TASK-214", Kind: "status-change", Actor: "Dave", Summary: "open → done"},
			want: "PAD TASK-214 → status-change (Dave): open → done",
		},
		{
			name: "assignment",
			in:   watchStreamPayload{ItemRef: "BUG-5", Kind: "assignment", Actor: "Alice", Summary: "assigned to Alice"},
			want: "PAD BUG-5 → assignment (Alice): assigned to Alice",
		},
		{
			name: "comment",
			in:   watchStreamPayload{ItemRef: "TASK-1", Kind: "comment", Actor: "Bob", Summary: "fix verified"},
			want: "PAD TASK-1 → comment (Bob): fix verified",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatMonitorLine(c.in); got != c.want {
				t.Errorf("formatMonitorLine(%+v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSleepOrDone_ReturnsTrueWhenDurationElapses(t *testing.T) {
	ctx := context.Background()
	if !sleepOrDone(ctx, time.Millisecond) {
		t.Fatal("expected sleepOrDone to return true when the duration elapses uninterrupted")
	}
}

func TestSleepOrDone_ReturnsFalseWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepOrDone(ctx, time.Hour) {
		t.Fatal("expected sleepOrDone to return false immediately for an already-cancelled context")
	}
}

func TestStreamWatchEvents_ParsesNotificationsAndTracksLastEventID(t *testing.T) {
	body := "id: 1\nevent: connected\ndata: {\"user_id\":\"u1\"}\n\n" +
		"id: 2\nevent: notification\ndata: {\"item_ref\":\"TASK-1\",\"kind\":\"comment\",\"actor\":\"Dave\",\"summary\":\"looks good\"}\n\n" +
		"id: 3\nevent: notification\ndata: {\"item_ref\":\"TASK-2\",\"kind\":\"status-change\",\"actor\":\"Dave\",\"summary\":\"open → done\"}\n\n"

	resp := fakeSSEResponse(body)
	lastID := streamWatchEvents(resp, "")
	if lastID != "3" {
		t.Fatalf("expected lastEventID to track the final id, got %q", lastID)
	}
}
