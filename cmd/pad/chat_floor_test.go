package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

type boundedFloorSink struct {
	fakeChatSink
	read func(context.Context, int) (int64, error)
}

func (f *boundedFloorSink) conversationFloor(ctx context.Context, n int) (int64, error) {
	return f.read(ctx, n)
}

func floorEvent(n int, seq int64) watchStreamPayload {
	return watchStreamPayload{Conversation: n, MessageID: fmt.Sprintf("m%d", seq), MessageSeq: seq}
}

// T1: B's first contact is an out-of-order tombstone. A listed summary
// marker must not authorize skipping rows 1-2: the fetched floor starts the fill.
func TestMonitorChatServerFloorHunter(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale-snapshot=%t", stale), func(t *testing.T) {
			chatSessionEnv(t)
			f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("m1", 1), chatMsg("m2", 2)}}
			if stale {
				f.summaryRows = []models.ConversationUnread{{Number: 2, DeliveredSeq: 2, Unread: 1}}
				f.total = 1
			}
			c := newMonitorChat(f, newRecentSet(256, 10*time.Minute))
			var out bytes.Buffer
			c.summary(&out)
			out.Reset() // Inspect the event fill separately from the unread line.
			p := floorEvent(2, 3)
			p.Deleted = true
			c.handle(&out, p)
			floor, applied := c.last[2]
			if !applied || floor != 2 || c.delivered[2] != 2 || !slices.Equal(f.ackSeqs, []int64{2}) || f.fetches != 1 || !slices.Equal(f.after, []int64{0}) || strings.Count(out.String(), "fetched ") != 2 {
				t.Fatalf("T1 must fetch floor 0 and handle rows 1-2 before acknowledging: last=%v delivered=%v acks=%v output=%q", c.last, c.delivered, f.ackSeqs, out.String())
			}
		})
	}
}

// T1 with local delivery: the first-contact floor is max(server floor, local
// delivered) (cmd_watch_chat.go attemptFloor). Each row pins one side: below
// the floor, local must not reopen rows the server says are delivered; above
// it, the server floor must not re-acknowledge what this process already did
// (TASK-4067). The event is the same out-of-order tombstone, seq 3.
func TestMonitorChatServerFloorHunterLocalDelivered(t *testing.T) {
	for _, row := range []struct {
		name         string
		local        int64
		wantAcks     []int64
		wantFloor    int64
		wantDelivery int64
	}{
		// Floor 2: seq 3 is the next contiguous tombstone, consumed directly.
		{name: "local-below-floor", local: 1, wantAcks: []int64{3}, wantFloor: 3, wantDelivery: 3},
		// Floor 3: seq 3 is already delivered, so nothing is acknowledged.
		{name: "local-above-floor", local: 3, wantAcks: nil, wantFloor: 3, wantDelivery: 3},
	} {
		t.Run(row.name, func(t *testing.T) {
			chatSessionEnv(t)
			f := &fakeChatSink{floor: 2, messages: []monitorChatMessage{chatMsg("m1", 1), chatMsg("m2", 2)}}
			c := newMonitorChat(f, newRecentSet(256, 10*time.Minute))
			c.delivered[2] = row.local
			var out bytes.Buffer
			p := floorEvent(2, 3)
			p.Deleted = true
			c.handle(&out, p)
			if f.floorReads.Load() != 1 || c.last[2] != row.wantFloor || c.delivered[2] != row.wantDelivery || !slices.Equal(f.ackSeqs, row.wantAcks) || f.fetches != 0 || out.Len() != 0 {
				t.Fatalf("floor reads=%d last=%v delivered=%v acks=%v fetches=%d after=%v output=%q", f.floorReads.Load(), c.last, c.delivered, f.ackSeqs, f.fetches, f.after, out.String())
			}
		})
	}
}

// B2: a lone DM must be delivered during this call, with no second event.
func TestMonitorChatFirstContactDeliversSameEvent(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("m1", 1)}}
	c := newMonitorChat(f, newRecentSet(256, 10*time.Minute))
	var out bytes.Buffer
	c.handle(&out, floorEvent(1, 1))
	if f.floorReads.Load() != 1 || !slices.Equal(f.after, []int64{0}) || !slices.Equal(f.ackSeqs, []int64{1}) || strings.Count(out.String(), "fetched m1\n") != 1 || c.delivered[1] != 1 {
		t.Fatalf("first call must deliver: reads=%d after=%v acks=%v output=%q", f.floorReads.Load(), f.after, f.ackSeqs, out.String())
	}
}

// B1: context deadlines advance virtual time; no real three-second sleeps.
func TestMonitorChatFloorTimeoutBackoff(t *testing.T) {
	chatSessionEnv(t)
	synctest.Test(t, func(t *testing.T) {
		reads := map[int]int{}
		f := &boundedFloorSink{read: func(ctx context.Context, n int) (int64, error) {
			reads[n]++
			<-ctx.Done()
			return 0, ctx.Err()
		}}
		c := newMonitorChat(f, newRecentSet(256, 10*time.Minute))
		c.floorTimeout = 3 * time.Second
		now := time.Now()
		c.now = func() time.Time { return now }
		var out bytes.Buffer
		start := time.Now()
		for i := 0; i < 5; i++ {
			c.handle(&out, floorEvent(1, 1))
		}
		if elapsed := time.Since(start); reads[1] != 1 || elapsed < c.floorTimeout || elapsed > c.floorTimeout+100*time.Millisecond {
			t.Fatalf("five events: reads=%v elapsed=%v", reads, elapsed)
		}
		for i := 0; i < 5; i++ {
			c.handle(&out, floorEvent(2, 1))
		}
		if reads[2] != 1 || out.Len() != 0 || f.acks != 0 || f.fetches != 0 || len(c.last) != 0 {
			t.Fatalf("independent bounded attempt: reads=%v last=%v acks=%v output=%q", reads, c.last, f.ackSeqs, out.String())
		}
	})
}

func TestMonitorChatFailedFloorIsolatedAndRetries(t *testing.T) {
	chatSessionEnv(t)
	reads := map[int]int{}
	fail := true
	f := &boundedFloorSink{read: func(_ context.Context, n int) (int64, error) {
		reads[n]++
		if n == 1 && fail {
			return 0, errors.New("floor unavailable")
		}
		return 0, nil
	}}
	c := newMonitorChat(f, newRecentSet(256, 10*time.Minute))
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	// Even positive local delivery may not be acknowledged after a failed GET.
	c.delivered[1] = 1
	var out bytes.Buffer
	for i := 0; i < 5; i++ {
		c.handle(&out, floorEvent(1, 2))
	}
	// Both sync_required and a new connection must preserve the deadline.
	c.stream(io.Discard, fakeSSEResponse("event: sync_required\ndata: {}\n\n"), "")
	c.handle(&out, floorEvent(1, 2))
	if reads[1] != 1 || out.Len() != 0 || f.acks != 0 || f.fetches != 0 || len(c.last) != 0 {
		t.Fatalf("failure/backoff handled rows: reads=%v last=%v acks=%v output=%q", reads, c.last, f.ackSeqs, out.String())
	}
	// A different conversation gets its own attempt after A's bounded failure.
	f.messages = []monitorChatMessage{chatMsg("c1", 1)}
	p := floorEvent(3, 1)
	p.MessageID = "c1"
	c.handle(&out, p)
	if c.delivered[3] != 1 || f.acks != 1 || reads[3] != 1 {
		t.Fatal("another conversation blocked")
	}
	fail = false
	f.messages = []monitorChatMessage{chatMsg("m2", 2)}
	out.Reset()
	now = now.Add(monitorChatFloorBackoff - time.Nanosecond)
	c.handle(&out, floorEvent(1, 2))
	if reads[1] != 1 {
		t.Fatal("retried before deadline")
	}
	now = now.Add(time.Nanosecond)
	c.handle(&out, floorEvent(1, 2))
	if reads[1] != 2 || c.delivered[1] != 2 || !slices.Equal(f.ackSeqs, []int64{1, 2}) || strings.Count(out.String(), "fetched m2\n") != 1 {
		t.Fatalf("one retry at deadline: reads=%v last=%v acks=%v output=%q", reads, c.last, f.ackSeqs, out.String())
	}
	c.handle(&out, floorEvent(1, 2))
	if reads[1] != 2 {
		t.Fatal("successful floor refetched")
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
	if f.floorReads.Load() != 2 || c.delivered[1] != 8 {
		t.Fatal("next event did not refetch and deliver")
	}
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
			if !c.attemptFloor(1) {
				t.Fatal("floor unavailable")
			}
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
	if !slices.Equal(f.after, []int64{2}) || !slices.Equal(f.ackSeqs, []int64{6}) || strings.Count(out.String(), "fetched m") != 4 {
		t.Fatalf("stale floor skipped rows: after=%v acks=%v output=%q", f.after, f.ackSeqs, out.String())
	}
}

// T5 replaces obsolete identity/generation result injection. All calls run on
// the stream goroutine; adoption must precede selecting a sink or floor.
func TestMonitorChatSynchronousIdentityChange(t *testing.T) {
	for _, adopt := range []bool{false, true} {
		t.Run(fmt.Sprintf("adopt-during-step=%t", adopt), func(t *testing.T) {
			chatSessionEnv(t)
			old := &fakeChatSink{floor: 9}
			fresh := &fakeChatSink{messages: []monitorChatMessage{chatMsg("m1", 1)}}
			c := newMonitorChat(old, newRecentSet(256, 10*time.Minute))
			c.setIdentity("server", "old")
			c.last[1], c.delivered[2], c.printedIDs["old"] = 9, 9, true
			c.printed.seen("m1")
			c.retryAfter[3] = c.now().Add(time.Hour)
			c.identityRetryAfter = c.now().Add(-time.Second)
			if adopt {
				c.resolveIdentity = func(context.Context) (monitorChatIdentity, chatSink) {
					return monitorChatIdentity{"server", "new"}, fresh
				}
			} else {
				c.identityRetryAfter = c.now().Add(time.Hour)
				c.setIdentity("server", "new")
				c.sink = fresh
			}
			var out bytes.Buffer
			c.handle(&out, floorEvent(2, 1))
			if c.identity.userID != "new" || c.sink != fresh || c.resolveIdentity != nil || len(c.retryAfter) != 0 || !c.identityRetryAfter.IsZero() || len(c.last) != 1 || c.last[2] != 1 || c.delivered[2] != 1 || c.printedIDs["old"] || old.floorReads.Load() != 0 || old.acks != 0 || !slices.Equal(fresh.after, []int64{0}) || !slices.Equal(fresh.ackSeqs, []int64{1}) || strings.Count(out.String(), "fetched m1\n") != 1 {
				t.Fatalf("identity leaked old state: identity=%v last=%v delivered=%v retries=%v global=%v oldReads=%d after=%v acks=%v output=%q", c.identity, c.last, c.delivered, c.retryAfter, c.identityRetryAfter, old.floorReads.Load(), fresh.after, fresh.ackSeqs, out.String())
			}
		})
	}
}
