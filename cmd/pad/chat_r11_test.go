package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMonitorChatRetractionBeforeBackoff(t *testing.T) {
	for _, resetFloor := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset-floor=%t", resetFloor), func(t *testing.T) {
			chatSessionEnv(t)
			f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("m1", 1)}, failAck: true}
			c := newMonitorChat(f, newRecentSet(256, time.Minute))
			now := time.Now()
			c.now = func() time.Time { return now }
			var out bytes.Buffer
			c.handle(&out, floorEvent(1, 1))
			if !strings.Contains(out.String(), "fetched m1\n") || !c.printedIDs["m1"] || !now.Before(c.retryAfter[1]) {
				t.Fatal("fixture must print m1 and back off after a failed ack")
			}
			if resetFloor {
				c.resetFloors()
			}
			out.Reset()
			// An unowned delete cannot print or make a network call behind backoff.
			c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "never-printed", MessageSeq: 2, Deleted: true})
			if out.Len() != 0 {
				t.Fatalf("unowned delete printed %q", out.String())
			}
			p := watchStreamPayload{Conversation: 1, MessageID: "m1", MessageSeq: 1, Deleted: true}
			c.handle(&out, p)
			if strings.Count(out.String(), "retracted") != 1 || c.printedIDs["m1"] || !c.printed.contains("retracted:m1") {
				t.Fatalf("owed retraction lost behind backoff: output=%q owed=%v", out.String(), c.printedIDs["m1"])
			}
			c.handle(&out, p)
			if strings.Count(out.String(), "retracted") != 1 || f.floorReads.Load() != 1 || f.fetches != 1 || f.acks != 1 || c.delivered[1] != 1 || (resetFloor && len(c.last) != 0) || (!resetFloor && c.last[1] != 1) {
				t.Fatalf("duplicate or network/advancement behind backoff: output=%q floors=%v delivered=%v reads=%d fetches=%d acks=%d", out.String(), c.last, c.delivered, f.floorReads.Load(), f.fetches, f.acks)
			}
		})
	}
}

func TestMonitorChatEarlyRetractionWriteFailure(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{}
	c := newMonitorChat(f, newRecentSet(256, time.Minute))
	c.printedIDs["m1"] = true
	p := watchStreamPayload{Conversation: 1, MessageID: "m1", MessageSeq: 1, Deleted: true}
	c.handle(failChatWriter{}, p)
	if !c.printedIDs["m1"] || c.printed.contains("retracted:m1") || len(c.delivered) != 0 || f.floorReads.Load() != 0 || f.fetches != 0 || f.acks != 0 {
		t.Fatalf("failed retraction advanced or contacted server: owed=%v delivered=%v reads=%d fetches=%d acks=%d", c.printedIDs["m1"], c.delivered, f.floorReads.Load(), f.fetches, f.acks)
	}
	if _, ok := c.last[1]; ok {
		t.Fatal("failed retraction fetched a floor")
	}
}
