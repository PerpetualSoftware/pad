package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

func TestMonitorChatSyncSummaryFailureRefetchesFloor(t *testing.T) {
	chatSessionEnv(t)
	seen := newRecentSet(2, 10*time.Minute)
	now := time.Unix(1000, 0)
	seen.now = func() time.Time { return now }
	f := &fakeChatSink{floor: 2, messages: []monitorChatMessage{chatMsg("a", 1), chatMsg("b", 2), chatMsg("c", 3), chatMsg("d", 4)}}
	c := newSeededMonitorChat(f, seen)
	var out bytes.Buffer
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "c", MessageSeq: 3})
	now = now.Add(11 * time.Minute)
	if seen.contains("c") {
		t.Fatal("test needs expired id")
	}
	f.unreadErr = errors.New("summary unavailable")
	f.floor = 0
	c.resync(&out)
	if len(c.last) != 0 {
		t.Fatal("restored pre-sync floor")
	}
	p := watchStreamPayload{Conversation: 1, MessageID: "d", MessageSeq: 4}
	c.handle(&out, p)
	if f.floorReads.Load() != 2 || !slices.Equal(f.after, []int64{2, 3}) || !slices.Equal(f.ackSeqs, []int64{3, 4}) || strings.Count(out.String(), "fetched ") != 2 {
		t.Fatalf("after=%v acks=%v output=%q", f.after, f.ackSeqs, out.String())
	}
}

func TestMonitorChatDeliveryMemoryOutlivesSeenChat(t *testing.T) {
	chatSessionEnv(t)
	seen := newRecentSet(256, 10*time.Minute)
	now := time.Unix(1000, 0)
	seen.now = func() time.Time { return now }
	f := &fakeChatSink{}
	for seq := int64(1); seq <= 302; seq++ {
		f.messages = append(f.messages, chatMsg(fmt.Sprintf("m%d", seq), seq))
	}
	f.before = func() { f.failAck = true }
	c := newSeededMonitorChat(f, seen)
	// Keep the cache clock fixed during the 300-row count-eviction check.
	retryNow := now
	c.now = func() time.Time { return retryNow }
	var out bytes.Buffer
	for seq := int64(1); seq <= 300; seq++ {
		c.handle(&out, floorEvent(1, seq))
		retryNow = retryNow.Add(monitorChatFloorBackoff)
	}
	if seen.contains("m1") || !seen.contains("m300") || f.deliveredSeq != 0 {
		t.Fatal("test needs eviction and failed acks")
	}
	now = now.Add(11 * time.Minute)
	for seq := int64(301); seq <= 302; seq++ {
		c.stream(&out, fakeSSEResponse("event: sync_required\ndata: {}\n\n"), "")
		c.handle(&out, floorEvent(1, 1))
		c.handle(&out, floorEvent(1, seq))
		if strings.Count(out.String(), "fetched ") != int(seq) || strings.Count(out.String(), "fetched m1\n") != 1 || f.after[len(f.after)-1] != seq-1 || c.delivered[1] != seq {
			t.Fatalf("after=%v acks=%v printed=%d", f.after, f.ackSeqs, strings.Count(out.String(), "fetched "))
		}
		now = now.Add(11 * time.Minute)
		retryNow = retryNow.Add(monitorChatFloorBackoff)
	}
}

func TestMonitorChatDedupeAckUsesDeliveredNotSeed(t *testing.T) {
	for _, handled := range []bool{false, true} {
		t.Run(fmt.Sprint(handled), func(t *testing.T) {
			chatSessionEnv(t)
			seen := newRecentSet(2, time.Minute)
			now := time.Unix(1000, 0)
			seen.now = func() time.Time { return now }
			seen.seen("expired")
			now = now.Add(2 * time.Minute)
			if seen.contains("expired") {
				t.Fatal("test needs expiry")
			}
			seen.seen("old")
			f := &fakeChatSink{floor: 7}
			c := newMonitorChat(f, seen)
			var out bytes.Buffer
			want := []int64(nil)
			if handled {
				c.delivered[1] = 1
				want = []int64{1}
			}
			p := watchStreamPayload{Conversation: 1, MessageID: "old", MessageSeq: 2}
			c.handle(&out, p)
			if c.last[1] != 7 || !slices.Equal(f.ackSeqs, want) || f.fetches != 0 || out.Len() != 0 {
				t.Fatalf("ack used floor: last=%v acks=%v want=%v", c.last, f.ackSeqs, want)
			}
		})
	}
}

func TestMonitorChatRetractionWriteFailureStopsAck(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(fmt.Sprintf("direct=%t", direct), func(t *testing.T) {
			chatSessionEnv(t)
			printed := newRecentSet(2, time.Minute)
			now := time.Unix(1000, 0)
			printed.now = func() time.Time { return now }
			printed.seen("expired")
			now = now.Add(2 * time.Minute)
			if printed.contains("expired") {
				t.Fatal("test must exercise expiry")
			}
			// The original is still owed a retraction.
			f := &fakeChatSink{messages: []monitorChatMessage{{ID: "deleted", Seq: 1, Deleted: true}, chatMsg("next", 2)}}
			c := newSeededMonitorChat(f, printed)
			c.printedIDs["deleted"] = true
			p := watchStreamPayload{Conversation: 1, MessageID: "next", MessageSeq: 2}
			if direct {
				p = watchStreamPayload{Conversation: 1, MessageID: "deleted", MessageSeq: 1, Deleted: true}
			}
			c.handle(failChatWriter{}, p)
			if c.last[1] != 0 || len(f.ackSeqs) != 0 || printed.contains("retracted:deleted") {
				t.Fatalf("failed retraction advanced: cursor=%d acks=%v", c.last[1], f.ackSeqs)
			}
			var out bytes.Buffer
			c.handle(&out, p)
			if c.last[1] != p.MessageSeq || !slices.Equal(f.ackSeqs, []int64{p.MessageSeq}) || strings.Count(out.String(), "message deleted retracted") != 1 {
				t.Fatalf("retraction retry failed: cursor=%d acks=%v output=%q", c.last[1], f.ackSeqs, out.String())
			}
		})
	}
}

func TestMonitorChatAdapterQueryShape(t *testing.T) {
	var requests []string
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		if strings.HasSuffix(r.URL.Path, "/messages") {
			io.WriteString(w, `{"messages":[{"id":"m","seq":8}]}`)
		} else if strings.HasSuffix(r.URL.Path, "/12") {
			io.WriteString(w, `{"delivered_seq":7}`)
		} else {
			io.WriteString(w, `{"total":1,"conversations":[{"number":12,"delivered_seq":7}]}`)
		}
	}))
	defer h.Close()
	sink := httpChatSink{client: cli.NewClientFromURLWithAgentName(h.URL, "monitor")}
	for _, query := range []struct {
		after int64
		limit int
	}{{0, 1}, {7, 20}} {
		messages, err := sink.messagesAfter(context.Background(), 12, query.after, query.limit)
		if err != nil || len(messages) != 1 || messages[0].Seq != 8 {
			t.Fatalf("messages=%v err=%v", messages, err)
		}
	}
	floor, err := sink.conversationFloor(context.Background(), 12)
	if err != nil || floor != 7 {
		t.Fatalf("floor=%d err=%v", floor, err)
	}
	summary, err := sink.unread(context.Background(), "seat +&/name")
	if err != nil || summary.Total != 1 || len(summary.Conversations) != 1 || summary.Conversations[0].DeliveredSeq != 7 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	// Literal old raw-GET paths pin the adapter through the TASK-4042 client,
	// including after_seq=0 (a nil cursor would change the server's default).
	want := []string{
		"GET /api/v1/me/conversations/12/messages?after_seq=0&limit=1",
		"GET /api/v1/me/conversations/12/messages?after_seq=7&limit=20",
		"GET /api/v1/me/conversations/12",
		"GET /api/v1/me/chat/unread?agent=seat+%2B%26%2Fname",
	}
	if !slices.Equal(requests, want) {
		t.Fatalf("adapter requests=%q want=%q", requests, want)
	}
}
