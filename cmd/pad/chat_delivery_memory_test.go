package main

import (
	"bytes"
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
	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestMonitorChatSyncSummaryFailureRetainsBaselines(t *testing.T) {
	chatSessionEnv(t)
	oldSeen := seenChat
	seenChat = newRecentSet(2, 10*time.Minute)
	t.Cleanup(func() { seenChat = oldSeen })
	now := time.Unix(1000, 0)
	seenChat.now = func() time.Time { return now }
	f := &fakeChatSink{
		messages:    []monitorChatMessage{chatMsg("a", 1), chatMsg("b", 2), chatMsg("c", 3), chatMsg("d", 4)},
		summaryRows: []models.ConversationUnread{{Number: 1, DeliveredSeq: 2}},
	}
	f.beforeUnread = func() {
		if f.totals == 2 {
			now = now.Add(11 * time.Minute)
			if seenChat.contains("c") {
				t.Fatal("test must expire printed ids before recovery")
			}
			f.unreadErr = errors.New("summary unavailable")
		}
	}
	var out bytes.Buffer
	stream := chatSSE("c", 3, false) + "event: sync_required\ndata: {}\n\n" + chatSSE("d", 4, false)
	streamWatchEventsTo(&out, fakeSSEResponse(stream), "", f)
	if !slices.Equal(f.after, []int64{2, 3}) || !slices.Equal(f.ackSeqs, []int64{3, 4}) || strings.Count(out.String(), "fetched ") != 2 || strings.Count(out.String(), "fetched c\n") != 1 || strings.Count(out.String(), "fetched d\n") != 1 {
		t.Fatalf("lost baselines or reprinted history: after=%v acks=%v output=%q", f.after, f.ackSeqs, out.String())
	}
}

func TestMonitorChatDeliveryMemoryOutlivesSeenChat(t *testing.T) {
	chatSessionEnv(t)
	oldSeen := seenChat
	seenChat = newRecentSet(256, 10*time.Minute)
	t.Cleanup(func() { seenChat = oldSeen })
	now := time.Unix(1000, 0)
	seenChat.now = func() time.Time { return now }
	f := &fakeChatSink{summaryRows: []models.ConversationUnread{{Number: 1, DeliveredSeq: 0}}}
	var stream strings.Builder
	for seq := int64(1); seq <= 301; seq++ {
		id := fmt.Sprintf("m%d", seq)
		f.messages = append(f.messages, chatMsg(id, seq))
		if seq <= 300 {
			stream.WriteString(chatSSE(id, seq, false))
		}
	}
	// Every print's acknowledgement fails, leaving the server marker at zero.
	f.before = func() { f.failAck = true }
	f.beforeUnread = func() {
		if f.totals == 2 {
			if seenChat.contains("m1") || !seenChat.contains("m300") || f.deliveredSeq != 0 {
				t.Fatal("test must evict the oldest of 300 printed ids with all acks failed")
			}
			now = now.Add(11 * time.Minute)
		}
	}
	stream.WriteString("event: sync_required\ndata: {}\n\n")
	stream.WriteString(chatSSE("m1", 1, false))
	stream.WriteString(chatSSE("m301", 301, false))
	var out bytes.Buffer
	streamWatchEventsTo(&out, fakeSSEResponse(stream.String()), "", f)
	if strings.Count(out.String(), "fetched ") != 301 || strings.Count(out.String(), "fetched m1\n") != 1 || strings.Count(out.String(), "fetched m301\n") != 1 || f.after[len(f.after)-1] != 300 || f.seq != 301 {
		t.Fatalf("delivery memory lost: after=%v acks=%v printed=%d", f.after, f.ackSeqs, strings.Count(out.String(), "fetched "))
	}
}

func TestMonitorChatDedupeAckUsesDeliveredNotSeed(t *testing.T) {
	for _, name := range []string{"seq-minus-one-seed", "server-marker-ahead"} {
		t.Run(name, func(t *testing.T) {
			chatSessionEnv(t)
			printed := newRecentSet(2, time.Minute)
			now := time.Unix(1000, 0)
			printed.now = func() time.Time { return now }
			printed.seen("expired")
			now = now.Add(2 * time.Minute)
			if printed.contains("expired") {
				t.Fatal("test must exercise expiry")
			}
			// A printed id retained from an earlier connection is not a local
			// delivery high-water, and cannot make a seed acknowledgeable.
			printed.seen("old")
			f := &fakeChatSink{}
			c := newMonitorChat(f, printed)
			var out bytes.Buffer
			var wantAcks []int64
			if name == "seq-minus-one-seed" {
				c.summary(&out)
				f.fail = true
				c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "eight", MessageSeq: 8})
			} else {
				f.messages = []monitorChatMessage{chatMsg("one", 1)}
				c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "one", MessageSeq: 1})
				delete(c.last, 1) // a successful reconnect summary re-seeds the cursor
				f.summaryRows = []models.ConversationUnread{{Number: 1, DeliveredSeq: 7}}
				c.summary(&out)
				wantAcks = []int64{1, 1}
			}
			if c.last[1] != 7 {
				t.Fatalf("test needs an unhandled seed at 7, got %d", c.last[1])
			}
			c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "old", MessageSeq: 2})
			if !slices.Equal(f.ackSeqs, wantAcks) {
				t.Fatalf("ack crossed local delivery: acks=%v want=%v cursor=%d", f.ackSeqs, wantAcks, c.last[1])
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
			printed.seen("deleted") // the original is still owed a retraction
			f := &fakeChatSink{messages: []monitorChatMessage{{ID: "deleted", Seq: 1, Deleted: true}, chatMsg("next", 2)}}
			c := newMonitorChat(f, printed)
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
		} else {
			io.WriteString(w, `{"total":1,"conversations":[{"number":12,"delivered_seq":7}]}`)
		}
	}))
	defer h.Close()
	sink := httpChatSink{client: cli.NewClientFromURLWithAgentName(h.URL, "monitor")}
	for _, after := range []int64{0, 7} {
		messages, err := sink.messagesAfter(12, after, 20)
		if err != nil || len(messages) != 1 || messages[0].Seq != 8 {
			t.Fatalf("messages=%v err=%v", messages, err)
		}
	}
	summary, err := sink.unread("seat +&/name")
	if err != nil || summary.Total != 1 || len(summary.Conversations) != 1 || summary.Conversations[0].DeliveredSeq != 7 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	// Literal old raw-GET paths pin the adapter through the TASK-4042 client,
	// including after_seq=0 (a nil cursor would change the server's default).
	want := []string{
		"GET /api/v1/me/conversations/12/messages?after_seq=0&limit=20",
		"GET /api/v1/me/conversations/12/messages?after_seq=7&limit=20",
		"GET /api/v1/me/chat/unread?agent=seat+%2B%26%2Fname",
	}
	if !slices.Equal(requests, want) {
		t.Fatalf("adapter requests=%q want=%q", requests, want)
	}
}
