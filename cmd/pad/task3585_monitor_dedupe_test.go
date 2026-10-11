package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TASK-3585: the monitor prints a push once, however many times it is
// published under the same push_id (a re-publish after a claim takeover).
func TestTASK3585_MonitorDropsARepublishedPush(t *testing.T) {
	prev := seenPushes
	seenPushes = newRecentSet(256, 10*time.Minute)
	t.Cleanup(func() { seenPushes = prev })
	armSessionForTest(t)

	ev := func(id, pushID string) string {
		return "id: " + id + "\nevent: notification\ndata: {\"id\":" + id +
			",\"workspace\":\"w\",\"item_ref\":\"TASK-1\",\"kind\":\"push\",\"summary\":\"triage this\",\"push_id\":\"" + pushID + "\"}\n\n"
	}
	var buf bytes.Buffer
	streamWatchEventsTo(&buf, fakeSSEResponse(ev("1", "push_a")+ev("2", "push_a")+ev("3", "push_b")), "")
	if n := strings.Count(buf.String(), "triage this"); n != 2 {
		t.Fatalf("want push_a once and push_b once (2 lines), got %d:\n%s", n, buf.String())
	}
	// The set outlives one connection: a reconnect replaying push_a is dropped too.
	buf.Reset()
	streamWatchEventsTo(&buf, fakeSSEResponse(ev("4", "push_a")), "3")
	if buf.Len() != 0 {
		t.Fatalf("a re-publish on a new connection was printed: %q", buf.String())
	}
}

func TestTASK3585_RecentSetBounds(t *testing.T) {
	now := time.Unix(0, 0)
	s := newRecentSet(2, time.Minute)
	s.now = func() time.Time { return now }
	if s.seen("a") || s.seen("b") || !s.seen("a") {
		t.Fatal("a and b are new; a again is a repeat")
	}
	s.seen("c") // over max: evicts the oldest (a)
	if s.seen("a") {
		t.Fatal("a should have been evicted by count")
	}
	now = now.Add(2 * time.Minute)
	if s.seen("c") {
		t.Fatal("c should have expired")
	}
}
