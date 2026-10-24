package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

type floorReply struct {
	value int64
	err   error
}
type controlledFloorSink struct {
	fakeChatSink
	requests map[int]chan chan floorReply
	reads    [4]atomic.Int32
}

func newControlledFloorSink() *controlledFloorSink {
	f := &controlledFloorSink{requests: map[int]chan chan floorReply{}}
	for _, n := range []int{1, 2, 3} {
		f.requests[n] = make(chan chan floorReply, 4)
	}
	return f
}
func (f *controlledFloorSink) conversationFloor(n int) (int64, error) {
	f.reads[n].Add(1)
	reply := make(chan floorReply, 1)
	f.requests[n] <- reply
	r := <-reply
	return r.value, r.err
}
func floorRequest(t *testing.T, f *controlledFloorSink, n int) chan floorReply {
	t.Helper()
	select {
	case reply := <-f.requests[n]:
		return reply
	case <-time.After(time.Second):
		t.Fatal("no conversation floor fetch", n)
		return nil
	}
}
func floorEvent(n int, seq int64) watchStreamPayload {
	return watchStreamPayload{Conversation: n, MessageID: fmt.Sprintf("m%d", seq), MessageSeq: seq}
}

// T1: A's outstanding read must not authorize B from a stale unread snapshot.
// The final tombstone takes the direct path, so rows 1-2 remain undelivered.
func TestMonitorChatServerFloorHunter(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale-snapshot=%t", stale), func(t *testing.T) {
			chatSessionEnv(t)
			f := newControlledFloorSink()
			c := newMonitorChat(f, newRecentSet(256, 10*time.Minute))
			var out bytes.Buffer
			c.summary(&out) // B absent from the successful (possibly stale) summary.
			c.handle(&out, floorEvent(1, 1))
			a := floorRequest(t, f, 1)
			defer func() { a <- floorReply{} }()
			var b chan floorReply
			if stale {
				c.handle(&out, floorEvent(2, 1))
				b = floorRequest(t, f, 2)
			}
			// B's rows commit after the snapshot in the stale variant.
			f.messages = []monitorChatMessage{chatMsg("m1", 1), chatMsg("m2", 2)}
			c.handle(&out, floorEvent(2, 1))
			if !stale {
				b = floorRequest(t, f, 2)
			}
			c.handle(&out, floorEvent(2, 2))
			if out.Len() != 0 || f.acks != 0 || len(c.last) != 0 {
				t.Fatalf("first contact handled rows: last=%v acks=%v output=%q", c.last, f.ackSeqs, out.String())
			}
			b <- floorReply{value: 0}
			awaitChatFloor(t, c, 2)
			p := floorEvent(2, 3)
			p.Deleted = true
			c.handle(&out, p)
			floor, applied := c.last[2]
			if !applied || floor != 0 || c.delivered[2] != 0 || f.acks != 0 || f.fetches != 0 || out.Len() != 0 {
				t.Fatalf("T1 requires nothing acked above unprinted rows 1-2: last=%v delivered=%v acks=%v output=%q", c.last, c.delivered, f.ackSeqs, out.String())
			}
		})
	}
}

func TestMonitorChatFailedFloorIsolatedAndRetries(t *testing.T) {
	chatSessionEnv(t)
	f := newControlledFloorSink()
	f.messages = []monitorChatMessage{chatMsg("m1", 1)}
	c := newMonitorChat(f, newRecentSet(256, 10*time.Minute))
	var out bytes.Buffer
	c.summary(&out)
	c.handle(&out, floorEvent(1, 1))
	a := floorRequest(t, f, 1)
	c.handle(&out, floorEvent(1, 1))
	if len(f.requests[1]) != 0 {
		t.Fatal("duplicate in-flight fetch")
	}
	// C can complete while A remains blocked. Message ids are globally unique.
	cEvent := floorEvent(3, 1)
	cEvent.MessageID = "c1"
	f.messages = []monitorChatMessage{chatMsg("c1", 1)}
	c.handle(&out, cEvent)
	floorRequest(t, f, 3) <- floorReply{}
	awaitChatFloor(t, c, 3)
	c.handle(&out, cEvent)
	if c.delivered[3] != 1 || f.acks != 1 {
		t.Fatal("another conversation blocked")
	}
	f.messages = []monitorChatMessage{chatMsg("m1", 1)}
	out.Reset()
	acks := f.acks
	a <- floorReply{err: errors.New("floor unavailable")}
	awaitChatFloor(t, c, 1)
	c.handle(&out, floorEvent(1, 1))
	if out.Len() != 0 || f.acks != acks {
		t.Fatal("failed fetch printed or acknowledged")
	}
	if _, ok := c.last[1]; ok {
		t.Fatal("failed fetch stored floor")
	}
	retry := floorRequest(t, f, 1)
	retry <- floorReply{}
	awaitChatFloor(t, c, 1)
	c.handle(&out, floorEvent(1, 1))
	if f.reads[1].Load() != 2 || f.reads[3].Load() != 1 {
		t.Fatalf("fetch counts: A=%d C=%d", f.reads[1].Load(), f.reads[3].Load())
	}
	if c.delivered[1] != 1 || f.acks != acks+1 || strings.Count(out.String(), "fetched m1") != 1 || len(f.requests[1]) != 0 {
		t.Fatal("retry did not handle row exactly once")
	}
}

type chatFloorStreamReader struct {
	io.Reader
	before func()
}

func (r *chatFloorStreamReader) Read(p []byte) (int, error) {
	if r.before != nil {
		before := r.before
		r.before = nil
		before()
	}
	return r.Reader.Read(p)
}

func TestMonitorChatSyncRequiredRefetchesFloor(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{floor: 2, messages: []monitorChatMessage{chatMsg("m3", 3), chatMsg("m8", 8)}}
	c := newMonitorChat(f, newRecentSet(256, 10*time.Minute))
	var out bytes.Buffer
	response := fakeSSEResponse("")
	response.Body = io.NopCloser(&chatFloorStreamReader{Reader: strings.NewReader("event: sync_required\ndata: {}\n\n"), before: func() {
		// This runs on the stream goroutine, after connect resync, before sync_required.
		c.handle(&out, floorEvent(1, 3))
		awaitChatFloor(t, c, 1)
		c.handle(&out, floorEvent(1, 3))
		if c.delivered[1] != 3 {
			t.Fatal("setup did not deliver")
		}
		f.floor = 7
	}})
	c.stream(&out, response, "")
	if len(c.last) != 0 || c.delivered[1] != 3 || !c.printedIDs["m3"] {
		t.Fatal("sync kept floor or lost delivery memory")
	}
	c.handle(&out, floorEvent(1, 8))
	awaitChatFloor(t, c, 1)
	if f.floorReads.Load() != 2 || c.delivered[1] != 3 {
		t.Fatal("next event did not refetch or floor advanced delivery")
	}
	c.handle(&out, floorEvent(1, 8))
	if !slices.Equal(f.after, []int64{2, 7}) || !slices.Equal(f.ackSeqs, []int64{3, 8}) {
		t.Fatalf("after=%v acks=%v", f.after, f.ackSeqs)
	}
}

func TestMonitorChatFloorMaxLocalDelivery(t *testing.T) {
	for _, floor := range []int64{0, 7} {
		t.Run(fmt.Sprint(floor), func(t *testing.T) {
			chatSessionEnv(t)
			f := &fakeChatSink{floor: floor, messages: []monitorChatMessage{chatMsg("m6", 6), chatMsg("m8", 8)}}
			c := newMonitorChat(f, newRecentSet(256, 10*time.Minute))
			c.delivered[1] = 5
			c.resync(io.Discard)
			seq := max(floor, 5) + 1
			c.handle(io.Discard, floorEvent(1, seq))
			awaitChatFloor(t, c, 1)
			if c.delivered[1] != 5 || f.acks != 0 {
				t.Fatal("floor advanced local delivery")
			}
			c.handle(io.Discard, floorEvent(1, seq))
			if !slices.Equal(f.after, []int64{max(floor, 5)}) || c.delivered[1] != seq {
				t.Fatalf("wrong floor: after=%v delivered=%v", f.after, c.delivered)
			}
		})
	}
}

func TestMonitorChatFreshProcessRedeliversAboveStaleFloor(t *testing.T) {
	chatSessionEnv(t)
	// Another process has already acked 5, but this GET snapshot predates it.
	f := &fakeChatSink{floor: 2, deliveredSeq: 5, summaryRows: []models.ConversationUnread{{Number: 1, DeliveredSeq: 5}}, total: 1}
	for seq := int64(1); seq <= 6; seq++ {
		f.messages = append(f.messages, chatMsg(fmt.Sprintf("m%d", seq), seq))
	}
	c := newMonitorChat(f, newRecentSet(256, 10*time.Minute))
	var out bytes.Buffer
	c.summary(&out)
	c.handle(&out, floorEvent(1, 6))
	awaitChatFloor(t, c, 1)
	c.handle(&out, floorEvent(1, 6))
	if !slices.Equal(f.after, []int64{2}) || !slices.Equal(f.ackSeqs, []int64{6}) || strings.Count(out.String(), "fetched m") != 4 {
		t.Fatalf("stale floor skipped rows: after=%v acks=%v output=%q", f.after, f.ackSeqs, out.String())
	}
}

// Wait without consuming the result; only the next event may apply it.
func awaitChatFloor(t *testing.T, c *monitorChat, n int) {
	t.Helper()
	pending := c.pendingFloor[n]
	if pending == nil {
		t.Fatal("no pending floor", n)
	}
	select {
	case result := <-pending:
		pending <- result
	case <-time.After(time.Second):
		t.Fatal("floor read did not finish", n)
	}
}

func TestMonitorChatStaleFloorResultDiscarded(t *testing.T) {
	for _, change := range []string{"generation", "identity"} {
		t.Run(change, func(t *testing.T) {
			chatSessionEnv(t)
			f := newControlledFloorSink()
			c := newMonitorChat(f, newRecentSet(256, 10*time.Minute))
			c.setIdentity("server", "old")
			var out bytes.Buffer
			c.handle(&out, floorEvent(1, 8))
			oldReply := floorRequest(t, f, 1)
			oldPending := c.pendingFloor[1]
			if change == "generation" {
				c.resync(&out)
			} else {
				c.setIdentity("server", "new")
			}
			if len(c.pendingFloor) != 0 || len(c.last) != 0 {
				t.Fatal("change retained pending read or floor")
			}
			c.handle(&out, floorEvent(1, 8))
			newReply := floorRequest(t, f, 1)
			oldReply <- floorReply{value: 7}
			var stale floorResult
			select {
			case stale = <-oldPending:
			case <-time.After(time.Second):
				t.Fatal("old read did not complete")
			}
			// Exercise the tag check as well as the map reset: an obsolete completion
			// must be rejected even if it is presented to the current polling path.
			c.pendingFloor[1] <- stale
			c.handle(&out, floorEvent(1, 8))
			retry := floorRequest(t, f, 1)
			newReply <- floorReply{}
			retry <- floorReply{}
			awaitChatFloor(t, c, 1)
			if len(c.last) != 0 || f.acks != 0 || f.fetches != 0 || out.Len() != 0 {
				t.Fatalf("stale result applied: last=%v acks=%v output=%q", c.last, f.ackSeqs, out.String())
			}
		})
	}
}
