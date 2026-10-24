package main

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestMonitorChatRetractionWhileIdentityUnresolved(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{}
	c := newMonitorChat(nil, newRecentSet(256, time.Minute))
	resolutions := 0
	c.resolveIdentity = func(context.Context) (monitorChatIdentity, chatSink) {
		resolutions++
		return monitorChatIdentity{"server", "user"}, f
	}
	// A failed identity lookup left no sink or floor, but an original printed
	// by this process still owns a retraction during the global retry backoff.
	now := time.Now()
	c.now = func() time.Time { return now }
	c.identityRetryAfter = now.Add(monitorChatFloorBackoff)
	c.printedIDs["m1"] = true
	p := watchStreamPayload{Conversation: 1, MessageID: "m1", MessageSeq: 1, Deleted: true}
	var out bytes.Buffer
	for range 2 {
		c.handle(&out, p)
		if got, want := out.String(), "Pad chat message m1 retracted (pad chat show 1).\n"; got != want {
			t.Errorf("owed retraction must print exactly once: got %q, want %q", got, want)
		}
	}
	if resolutions != 0 {
		t.Errorf("retraction resolved identity %d times", resolutions)
	}
	if f.floorReads.Load() != 0 || f.fetches != 0 || f.acks != 0 || f.totals != 0 {
		t.Errorf("retraction made network calls: floors=%d fetches=%d acks=%d unread=%d", f.floorReads.Load(), f.fetches, f.acks, f.totals)
	}
	if c.sink != nil || c.resolveIdentity == nil || len(c.last) != 0 || len(c.delivered) != 0 {
		t.Fatal("retraction changed unresolved identity, floor or delivery state")
	}
}
