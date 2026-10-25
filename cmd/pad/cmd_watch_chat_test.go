package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

type fakeChatSink struct {
	floor                 int64
	floorErr              error
	floorReads            atomic.Int32
	messages              []monitorChatMessage
	fetches, acks, totals int
	seq                   int64
	after                 []int64
	limits                []int
	fail                  bool
	total                 int
	agent                 string
	before                func()
	beforeUnread          func()
	failAck               bool
	deliveredSeq          int64
	ackSeqs               []int64
	summaryRows           []models.ConversationUnread
	unreadErr             error
}

func (f *fakeChatSink) conversationFloor(context.Context, int) (int64, error) {
	f.floorReads.Add(1)
	return f.floor, f.floorErr
}

func (f *fakeChatSink) messagesAfter(_ context.Context, n int, after int64, limit int) ([]monitorChatMessage, error) {
	f.fetches++
	f.after = append(f.after, after)
	f.limits = append(f.limits, limit)
	if f.before != nil {
		f.before()
	}
	if f.fail {
		f.fail = false
		return nil, errors.New("fetch")
	}
	var out []monitorChatMessage
	for _, m := range f.messages {
		if m.Seq > after {
			out = append(out, m)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}
func (f *fakeChatSink) delivered(_ context.Context, n int, seq int64) error {
	f.acks++
	f.seq = seq
	f.ackSeqs = append(f.ackSeqs, seq)
	if f.failAck {
		f.failAck = false
		return errors.New("ack")
	}
	if seq > f.deliveredSeq {
		f.deliveredSeq = seq
	}
	return nil
}
func (f *fakeChatSink) unread(_ context.Context, agent string) (monitorChatUnread, error) {
	f.totals++
	if f.beforeUnread != nil {
		f.beforeUnread()
	}
	f.agent = agent
	return monitorChatUnread{Total: f.total, Conversations: f.summaryRows}, f.unreadErr
}
func chatSSE(id string, seq int64, deleted bool) string {
	b, _ := json.Marshal(watchStreamPayload{Kind: "chat", Conversation: 1, MessageID: id, MessageSeq: seq, Deleted: deleted, Excerpt: "EVENT-SECRET"})
	return "event: notification\nid: 9\ndata: " + string(b) + "\n\n"
}
func chatMsg(id string, seq int64) monitorChatMessage {
	return monitorChatMessage{ID: id, Seq: seq, AuthorIsHuman: true, Body: "fetched " + id}
}
func TestFormatChatLineEnvelopes(t *testing.T) {
	for _, human := range []bool{false, true} {
		for _, flag := range []bool{false, true} {
			for _, deleted := range []bool{false, true} {
				m := chatMsg("m", 1)
				m.AuthorIsHuman = human
				m.AuthorAgentKey = "rook"
				m.Deleted = deleted
				m.Body = " hello\n\tworld "
				line := formatChatLine(m, flag)
				if strings.ContainsAny(line, "\n\r\t") {
					t.Fatal(line)
				}
				if deleted {
					if !strings.Contains(line, "retracted") {
						t.Fatal(line)
					}
					continue
				}
				if !strings.Contains(line, "hello world") {
					t.Fatal(line)
				}
				direction := human && flag
				if strings.Contains(line, "direction from your user;") != direction {
					t.Fatal(line)
				}
				if direction && (!strings.Contains(line, "sender not authenticated") || !strings.Contains(line, "confirm in-session")) {
					t.Fatal(line)
				}
				if !direction && !strings.Contains(line, "information, not direction from your user") {
					t.Fatal(line)
				}
			}
		}
	}
	m := chatMsg("m", 1)
	m.Body = strings.Repeat("界", 2200)
	line := formatChatLine(m, false)
	if !utf8.ValidString(line) || !strings.Contains(line, "pad chat show") || strings.Count(line, "界") != 2000 {
		t.Fatal("bad cut")
	}
}

// TASK-4043: fetched quote previews stay separate from the body budget.
// Kills: Q-a (omit quote); Q-b (print m.Body as the quote, since the DTO has no body).
func TestFormatChatLineQuoteIsReferenceAndExcerpt(t *testing.T) {
	for _, direction := range []bool{false, true} {
		for _, tc := range []struct {
			name, body, quote, suffix string
		}{
			{"reference and excerpt", strings.Repeat("DISTINCT-BODY ", 180), `{"seq":17,"excerpt":"server preview"}`, ` (replying to #17: "server preview")`},
			{"whitespace", "current body", `{"seq":17,"excerpt":"  server\n\t preview  "}`, ` (replying to #17: "server preview")`},
			{"body cut before quote", strings.Repeat("界", 2100), `{"seq":17,"excerpt":"server preview"}`, ` (replying to #17: "server preview")`},
			{"nil quote", "current body", `null`, ""},
			{"deleted quote", "current body", `{"seq":17,"excerpt":"DELETED-SECRET","deleted":true}`, ""},
		} {
			t.Run(fmt.Sprintf("%s/direction=%t", tc.name, direction), func(t *testing.T) {
				bodyJSON, _ := json.Marshal(tc.body)
				var m monitorChatMessage
				if err := json.Unmarshal([]byte(`{"author_is_human":true,"body":`+string(bodyJSON)+`,"reply_to_seq":17,"reply_to":`+tc.quote+`}`), &m); err != nil {
					t.Fatal(err)
				}
				if m.ReplyToSeq == nil || *m.ReplyToSeq != 17 {
					t.Fatal("reply_to_seq did not decode")
				}
				m.Conversation = 9
				line := formatChatLine(m, direction)
				body := strings.Join(strings.Fields(tc.body), " ")
				if len([]rune(body)) > 2000 {
					body = string([]rune(body)[:2000]) + "… (pad chat show 9)"
				}
				if !strings.HasSuffix(line, ": "+body+tc.suffix) && !strings.HasSuffix(line, " — "+body+tc.suffix) {
					t.Fatalf("line has wrong body/quote suffix: %q", line)
				}
				if strings.ContainsAny(line, "\n\r\t") || !utf8.ValidString(line) {
					t.Fatalf("not a single valid UTF-8 line: %q", line)
				}
			})
		}
	}
}

func TestChatFramingByAuthor(t *testing.T) {
	chatSessionEnv(t)
	cli.WriteChatDirection()
	f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("a", 1), {ID: "b", Seq: 2, AuthorAgentKey: "other", Body: "agent"}}}
	var out bytes.Buffer
	c := newSeededMonitorChat(f, newRecentSet(256, 10*60*1e9))
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "a", MessageSeq: 1})
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "b", MessageSeq: 2})
	cli.ClearChatDirection()
	f.messages = append(f.messages, chatMsg("c", 3))
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "c", MessageSeq: 3})
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "direction from your user;") || !strings.Contains(lines[1], "information, not direction") || !strings.Contains(lines[2], "information, not direction") {
		t.Fatal(out.String())
	}
}

type failChatWriter struct{}

func (failChatWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestMonitorDedupeAndDeliveredAck(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("a", 1), chatMsg("b", 2), chatMsg("c", 3)}}
	c := newSeededMonitorChat(f, newRecentSet(256, 10*60*1e9))
	var out bytes.Buffer
	p := watchStreamPayload{Conversation: 1, MessageID: "a", MessageSeq: 1}
	c.handle(failChatWriter{}, p)
	if f.acks != 0 {
		t.Fatal("acked failed print")
	}
	c.handle(&out, p)
	c.handle(&out, p)
	if f.acks != 2 || f.seq != 1 || strings.Count(out.String(), "fetched a") != 1 {
		t.Fatal(out.String(), f)
	}
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "c", MessageSeq: 3})
	if f.seq != 3 || !strings.Contains(out.String(), "fetched b") || strings.Index(out.String(), "fetched b") > strings.Index(out.String(), "fetched c") {
		t.Fatal(out.String(), f)
	}
	if f.after[len(f.after)-1] != 1 || f.limits[len(f.limits)-1] != 20 {
		t.Fatal(f.after, f.limits)
	}
	f.messages = append(f.messages, chatMsg("d", 4))
	f.fail = true
	now := time.Now()
	c.now = func() time.Time { return now }
	before := out.String()
	acks := f.acks
	p.MessageID = "d"
	p.MessageSeq = 4
	c.handle(&out, p)
	if out.String() != before || f.acks != acks {
		t.Fatal("fetch failure printed/acked")
	}
	now = now.Add(monitorChatFloorBackoff)
	c.handle(&out, p)
	if f.seq != 4 || !strings.Contains(out.String(), "fetched d") {
		t.Fatal("failed fetch deduped")
	}
}
func TestMonitorChatUsesOwnCredentialNotEventBody(t *testing.T) {
	chatSessionEnv(t)
	var gotAuth, gotAgent string
	acks := 0
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAgent = r.Header.Get("X-Pad-Agent")
		if r.URL.Path == "/api/v1/me/chat/unread" {
			fmt.Fprint(w, `{"total":0,"conversations":[]}`)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/me/conversations/1/delivered" {
			var body struct {
				Seq int64 `json:"seq"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Seq != 7 {
				t.Errorf("bad delivered acknowledgement: %+v, %v", body, err)
			}
			acks++
			fmt.Fprint(w, `{"delivered_seq":7}`)
			return
		}
		if r.URL.Path == "/api/v1/me/conversations/1" {
			fmt.Fprint(w, `{"delivered_seq":6}`)
			return
		}
		if r.URL.Query().Get("after_seq") != "6" || r.URL.Query().Get("limit") != "1" {
			t.Error(r.URL)
		}
		fmt.Fprint(w, `{"messages":[{"id":"m","seq":7,"author_is_human":true,"body":"OWN-CREDENTIAL-BODY","reply_to_seq":3,"reply_to":{"seq":3,"author_agent_key":"rook","author_is_human":false,"excerpt":"fetched quote","deleted":false}}]}`)
	}))
	defer h.Close()
	client := cli.NewClientFromURLWithAgentName(h.URL, "rook")
	client.SetAuthToken("monitor-token")
	sink := httpChatSink{client: client}
	var out bytes.Buffer
	c := newMonitorChat(sink, newRecentSet(256, 10*60*1e9))
	c.summary(&out)
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "m", MessageSeq: 7, Excerpt: "EVENT-SECRET"})
	if gotAuth != "Bearer monitor-token" || gotAgent != "rook" || acks != 1 || !strings.Contains(out.String(), "OWN-CREDENTIAL-BODY") || !strings.Contains(out.String(), ` (replying to #3: "fetched quote")`) || strings.Contains(out.String(), "EVENT-SECRET") {
		t.Fatal(gotAuth, gotAgent, out.String())
	}
}
func TestReconnectPrintsUnreadSummary(t *testing.T) {
	chatSessionEnv(t)
	t.Setenv("PAD_AGENT", "unread-test-agent")
	f := &fakeChatSink{total: 3}
	var out bytes.Buffer
	streamWatchEventsTo(&out, fakeSSEResponse("event: sync_required\ndata: {}\n\n"), "", f)
	if strings.Count(out.String(), "unread") != 2 || f.acks != 0 || f.totals != 2 || f.agent != cli.ResolveAgentName() {
		t.Fatal(out.String(), f)
	}
	out.Reset()
	f.total = 0
	streamWatchEventsTo(&out, fakeSSEResponse(""), "", f)
	if out.Len() != 0 {
		t.Fatal(out.String())
	}
	cli.WriteDisarmState()
	f.total = 3
	streamWatchEventsTo(&out, fakeSSEResponse(""), "", f)
	if out.Len() != 0 {
		t.Fatal(out.String())
	}
}
func TestMonitorChatRespectsConsentRecheck(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("a", 1)}}
	cli.WriteDisarmState()
	var out bytes.Buffer
	streamWatchEventsTo(&out, fakeSSEResponse(chatSSE("a", 1, false)), "", f)
	if f.fetches != 0 || f.acks != 0 || out.Len() != 0 {
		t.Fatal(out.String(), f)
	}
	cli.WriteArmState()
	f.before = func() { cli.WriteDisarmState() }
	c := newSeededMonitorChat(f, newRecentSet(256, 10*60*1e9))
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "a", MessageSeq: 1})
	if out.Len() != 0 || f.acks != 0 {
		t.Fatal("consent changed during fetch", out.String())
	}
}
func TestMonitorDeleteRetraction(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{}
	c := newSeededMonitorChat(f, newRecentSet(256, 10*60*1e9))
	var out bytes.Buffer
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "never", MessageSeq: 2, Deleted: true})
	if out.Len() != 0 || f.fetches != 1 || f.acks != 0 {
		t.Fatal("unseen tombstone")
	}
	f.messages = []monitorChatMessage{chatMsg("a", 1)}
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "a", MessageSeq: 1})
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "a", MessageSeq: 1, Deleted: true})
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "a", MessageSeq: 1, Deleted: true})
	if strings.Count(out.String(), "retracted") != 1 || f.fetches != 2 || f.acks != 1 {
		t.Fatal(out.String(), f)
	}
}

func TestMonitorChatFinalTombstoneAdvancesContiguously(t *testing.T) {
	for _, printed := range []bool{false, true} {
		t.Run(fmt.Sprintf("original-printed=%t", printed), func(t *testing.T) {
			chatSessionEnv(t)
			f := &fakeChatSink{}
			c := newSeededMonitorChat(f, newRecentSet(256, 10*60*1e9))
			c.last[1] = 1
			if printed {
				c.printedIDs["b"] = true
			}
			var out bytes.Buffer
			p := watchStreamPayload{Conversation: 1, MessageID: "b", MessageSeq: 2, Deleted: true}
			c.handle(&out, p)
			c.handle(&out, p)
			wantRetractions := 0
			if printed {
				wantRetractions = 1
			}
			if c.last[1] != 2 || f.deliveredSeq != 2 || !slices.Equal(f.ackSeqs, []int64{2}) || f.fetches != 0 || strings.Count(out.String(), "retracted") != wantRetractions {
				t.Fatalf("cursor=%d acks=%v delivered=%d fetches=%d output=%q", c.last[1], f.ackSeqs, f.deliveredSeq, f.fetches, out.String())
			}
			// An out-of-order tombstone attempts a fill, but cannot cross missing rows.
			c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "d", MessageSeq: 4, Deleted: true})
			if c.last[1] != 2 || !slices.Equal(f.ackSeqs, []int64{2}) || f.fetches != 1 {
				t.Fatalf("crossed gap: cursor=%d acks=%v fetches=%d", c.last[1], f.ackSeqs, f.fetches)
			}
			f.messages = []monitorChatMessage{chatMsg("c", 3), {ID: "d", Seq: 4, Deleted: true}, chatMsg("e", 5)}
			c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "e", MessageSeq: 5})
			if c.last[1] != 5 || !slices.Equal(f.ackSeqs, []int64{2, 5}) || !slices.Equal(f.after, []int64{2, 2}) || !strings.Contains(out.String(), "fetched c\n") || !strings.Contains(out.String(), "fetched e\n") {
				t.Fatalf("gap fill: cursor=%d acks=%v after=%v output=%q", c.last[1], f.ackSeqs, f.after, out.String())
			}
		})
	}
}

func TestMonitorChatDedupeRetriesFailedAck(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("a", 1), chatMsg("b", 2)}, failAck: true}
	c := newSeededMonitorChat(f, newRecentSet(256, 10*60*1e9))
	var out bytes.Buffer
	p := watchStreamPayload{Conversation: 1, MessageID: "b", MessageSeq: 2}
	c.handle(&out, p)
	if c.last[1] != 2 || f.deliveredSeq != 0 || !slices.Equal(f.ackSeqs, []int64{2}) {
		t.Fatalf("failed ack: cursor=%d delivered=%d acks=%v", c.last[1], f.deliveredSeq, f.ackSeqs)
	}
	now := time.Now()
	c.now = func() time.Time { return now }
	now = now.Add(monitorChatFloorBackoff)
	// An older duplicate after backoff retries the contiguous cursor, not the event seq.
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "a", MessageSeq: 1})
	if c.last[1] != 2 || f.deliveredSeq != 2 || !slices.Equal(f.ackSeqs, []int64{2, 2}) || f.fetches != 1 || strings.Count(out.String(), "fetched ") != 2 {
		t.Fatalf("dedupe retry: cursor=%d delivered=%d acks=%v fetches=%d output=%q", c.last[1], f.deliveredSeq, f.ackSeqs, f.fetches, out.String())
	}
	// Printed ids can survive a reconnect without a local baseline.
	c = newMonitorChat(f, c.printed)
	c.handle(&out, p)
	if !slices.Equal(f.ackSeqs, []int64{2, 2}) || f.fetches != 1 {
		t.Fatalf("acked without a positive baseline: acks=%v fetches=%d", f.ackSeqs, f.fetches)
	}
}

func TestMonitorChatSyncRequiredReseedsBaselines(t *testing.T) {
	for _, name := range []string{"failed-ack", "message-fetch-failure", "summary-unavailable"} {
		t.Run(name, func(t *testing.T) {
			chatSessionEnv(t)
			f := &fakeChatSink{floor: 2, messages: []monitorChatMessage{chatMsg("a", 1), chatMsg("b", 2), chatMsg("c", 3), chatMsg("d", 4)}}
			c := newSeededMonitorChat(f, newRecentSet(256, 10*60*1e9))
			var out bytes.Buffer
			if name == "failed-ack" {
				f.failAck = true
			} else {
				f.fail = true
			}
			c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "c", MessageSeq: 3})
			if name == "summary-unavailable" {
				f.unreadErr = errors.New("summary")
			}
			f.floor = 0 // a lower server snapshot cannot replace handled local rows
			c.stream(&out, fakeSSEResponse("event: sync_required\ndata: {}\n\n"), "")
			if len(c.last) != 0 {
				t.Fatal("sync retained old floor")
			}
			c.now = func() time.Time { return time.Now().Add(monitorChatFloorBackoff) }
			p := watchStreamPayload{Conversation: 1, MessageID: "d", MessageSeq: 4}
			c.handle(&out, p)
			after := int64(0)
			want := []int64{4}
			ids := []string{"a", "b", "c", "d"}
			if name == "failed-ack" {
				after = 3
				want = []int64{3, 4}
				ids = []string{"c", "d"}
			}
			if f.floorReads.Load() != 2 || !slices.Equal(f.after, []int64{2, after}) || !slices.Equal(f.ackSeqs, want) {
				t.Fatalf("reads=%d after=%v acks=%v", f.floorReads.Load(), f.after, f.ackSeqs)
			}
			previous := -1
			for _, id := range ids {
				line := "fetched " + id + "\n"
				at := strings.Index(out.String(), line)
				if strings.Count(out.String(), line) != 1 || at <= previous {
					t.Fatal(out.String())
				}
				previous = at
			}
		})
	}
}

func TestMonitorChatGapPagesAndOwnMessages(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{}
	for seq := int64(1); seq <= 43; seq++ {
		m := chatMsg(fmt.Sprint(seq), seq)
		m.Mine = seq == 2
		f.messages = append(f.messages, m)
	}
	c := newSeededMonitorChat(f, newRecentSet(256, 10*60*1e9))
	var out bytes.Buffer
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "1", MessageSeq: 1})
	for i, want := range []int64{21, 41, 43} {
		previous := c.last[1]
		c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "43", MessageSeq: 43})
		if f.fetches != i+2 || f.acks != i+2 || f.seq != want || c.last[1] != want || f.after[i+1] != previous || f.limits[i+1] != 20 {
			t.Fatalf("page %d: cursor=%d sink=%+v output=%q", i, c.last[1], f, out.String())
		}
	}
	if strings.Count(out.String(), "fetched ") != 42 || strings.Contains(out.String(), "fetched 2\n") || strings.Count(out.String(), "more in C1") != 2 {
		t.Fatal(out.String())
	}
	previous := -1
	for seq := int64(1); seq <= 43; seq++ {
		if seq == 2 {
			continue
		}
		at := strings.Index(out.String(), fmt.Sprintf("fetched %d\n", seq))
		if at <= previous {
			t.Fatalf("missing or unordered human seq %d: %s", seq, out.String())
		}
		previous = at
	}
}

func TestMonitorChatTombstoneInGap(t *testing.T) {
	for _, printed := range []bool{false, true} {
		t.Run(fmt.Sprintf("original-printed=%t", printed), func(t *testing.T) {
			chatSessionEnv(t)
			f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("a", 1), {ID: "b", Seq: 2, Deleted: true}, chatMsg("c", 3)}}
			c := newSeededMonitorChat(f, newRecentSet(256, 10*60*1e9))
			if printed {
				c.printedIDs["b"] = true
			}
			var out bytes.Buffer
			c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "a", MessageSeq: 1})
			c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "c", MessageSeq: 3})
			if f.fetches != 2 || f.acks != 2 || f.seq != 3 || c.last[1] != 3 || strings.Count(out.String(), "fetched ") != 2 || !strings.Contains(out.String(), "fetched c\n") {
				t.Fatalf("cursor=%d sink=%+v output=%q", c.last[1], f, out.String())
			}
			wantRetractions := 0
			if printed {
				wantRetractions = 1
			}
			if strings.Count(out.String(), "message b retracted") != wantRetractions {
				t.Fatal(out.String())
			}
		})
	}
}

func TestMonitorChatOwnMessageAtCappedFillEnd(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{}
	for seq := int64(1); seq <= 20; seq++ {
		m := chatMsg(fmt.Sprint(seq), seq)
		m.Mine = seq == 20
		f.messages = append(f.messages, m)
	}
	c := newSeededMonitorChat(f, newRecentSet(256, 10*60*1e9))
	var out bytes.Buffer
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "20", MessageSeq: 20})
	if f.fetches != 1 || f.limits[0] != 20 || f.acks != 1 || f.seq != 20 || c.last[1] != 20 || strings.Count(out.String(), "fetched ") != 19 || strings.Contains(out.String(), "fetched 20\n") || strings.Contains(out.String(), "more in C1") {
		t.Fatalf("cursor=%d sink=%+v output=%q", c.last[1], f, out.String())
	}
}

func TestMonitorChatOwnMessageAtTruncatedPageBoundary(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{}
	for seq := int64(1); seq <= 25; seq++ {
		m := chatMsg(fmt.Sprint(seq), seq)
		m.Mine = seq == 20 || seq == 23
		f.messages = append(f.messages, m)
	}
	c := newSeededMonitorChat(f, newRecentSet(256, 10*60*1e9))
	var out bytes.Buffer
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "25", MessageSeq: 25})
	if f.fetches != 1 || f.after[0] != 0 || f.limits[0] != 20 || f.acks != 1 || f.seq != 20 || c.last[1] != 20 || strings.Count(out.String(), "fetched ") != 19 || strings.Contains(out.String(), "fetched 20\n") {
		t.Fatalf("cursor=%d sink=%+v output=%q", c.last[1], f, out.String())
	}
	// The span beyond the fetched page may include own messages (seq 23).
	if !strings.HasSuffix(out.String(), "up to 5 more in C1: pad chat show C1 --since 20\n") || strings.Count(out.String(), "more in C1") != 1 {
		t.Fatal(out.String())
	}
}

// TASK-4041, DOC-4036: missing sequences still stop the fill and ack.
func TestMonitorChatAckStopsAtUnprintedMessage(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		chatSessionEnv(t)
		f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("a", 1), chatMsg("c", 3)}}
		c := newSeededMonitorChat(f, newRecentSet(256, 10*60*1e9))
		var out bytes.Buffer
		c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "a", MessageSeq: 1})
		c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "c", MessageSeq: 3})
		if f.seq != 1 || f.acks != 1 || strings.Contains(out.String(), "fetched c") {
			t.Fatalf("crossed missing sequence: output=%q sink=%+v", out.String(), f)
		}
	})
}

func TestMonitorChatSummaryRespectsConsentRecheck(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{total: 3, beforeUnread: func() { cli.WriteDisarmState() }}
	var out bytes.Buffer
	newMonitorChat(f, newRecentSet(256, 10*60*1e9)).summary(&out)
	if f.totals != 1 || out.Len() != 0 || f.acks != 0 {
		t.Fatalf("consent changed during unread fetch: output=%q sink=%+v", out.String(), f)
	}
}

func TestMonitorChatConnectBaselineAndContiguousAck(t *testing.T) {
	for _, tc := range []struct {
		name, summary     string
		floor             int64
		event, after, ack int64
		printed, limit    int
		more              string
	}{
		{"25-message-gap", `{"total":25,"conversations":[{"number":1,"delivered_seq":0,"unread":25}]}`, 0, 25, 0, 20, 20, 20, "up to 5 more in C1: pad chat show C1 --since 20"},
		{"fresh-unread-history", `{"total":1,"conversations":[{"number":1,"delivered_seq":5,"read_seq":7,"unread":1}]}`, 5, 8, 5, 8, 3, 20, ""},
		{"arrived-after-summary", `{"total":1,"conversations":[{"number":1,"delivered_seq":5,"last_seq":6,"unread":1}]}`, 5, 7, 5, 7, 2, 20, ""},
		{"missing-summary-still-fetches-floor", `{"total":0,"conversations":[]}`, 2, 8, 2, 8, 6, 20, ""},
		{"other-conversation-unread", `{"total":1,"conversations":[{"number":2,"delivered_seq":4,"unread":1}]}`, 2, 8, 2, 8, 6, 20, ""},
		{"summary-error-does-not-gate-floor", `error`, 2, 8, 2, 8, 6, 20, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chatSessionEnv(t)
			oldSeen := seenChat
			seenChat = newRecentSet(256, 10*60*1e9)
			t.Cleanup(func() { seenChat = oldSeen })
			var acks []int64
			fetches := 0
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/me/chat/unread":
					if r.URL.Query().Get("agent") != cli.ResolveAgentName() {
						t.Error("unread agent filter missing", r.URL)
					}
					if tc.summary == "error" {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					fmt.Fprint(w, tc.summary)
				case "/api/v1/me/conversations/1":
					fmt.Fprintf(w, `{"delivered_seq":%d}`, tc.floor)
				case "/api/v1/me/conversations/1/messages":
					fetches++
					if r.URL.Query().Get("after_seq") != fmt.Sprint(tc.after) || r.URL.Query().Get("limit") != fmt.Sprint(tc.limit) {
						t.Errorf("fetch %s, want after_seq=%d limit=%d", r.URL, tc.after, tc.limit)
					}
					var messages []monitorChatMessage
					for seq := tc.after + 1; seq <= tc.event && len(messages) < tc.limit; seq++ {
						messages = append(messages, chatMsg(fmt.Sprintf("m%02d", seq), seq))
					}
					json.NewEncoder(w).Encode(map[string]any{"messages": messages})
				case "/api/v1/me/conversations/1/delivered":
					var body struct {
						Seq int64 `json:"seq"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					acks = append(acks, body.Seq)
					fmt.Fprint(w, `{}`)
				default:
					t.Error("unexpected request", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer h.Close()
			var out bytes.Buffer
			c := newMonitorChat(httpChatSink{cli.NewClientFromURLWithAgentName(h.URL, cli.ResolveAgentName())}, seenChat)
			c.stream(&out, fakeSSEResponse(chatSSE(fmt.Sprintf("m%02d", tc.event), tc.event, false)), "")
			if fetches != 1 || len(acks) != 1 || acks[0] != tc.ack || strings.Count(out.String(), "fetched m") != tc.printed {
				t.Fatalf("fetches=%d acks=%v output=%s", fetches, acks, out.String())
			}
			previous := -1
			for seq := tc.after + 1; seq <= tc.ack; seq++ {
				at := strings.Index(out.String(), fmt.Sprintf("fetched m%02d", seq))
				if at <= previous {
					t.Fatalf("acked past unprinted or unordered seq %d: %s", seq, out.String())
				}
				previous = at
			}
			if tc.more != "" && !strings.Contains(out.String(), tc.more+"\n") {
				t.Fatalf("missing continuation %q: %s", tc.more, out.String())
			}
		})
	}
}

func newSeededMonitorChat(f *fakeChatSink, printed *recentSet) *monitorChat {
	c := newMonitorChat(f, printed)
	if !c.attemptFloor(1) {
		panic("test floor unavailable")
	}
	return c
}
