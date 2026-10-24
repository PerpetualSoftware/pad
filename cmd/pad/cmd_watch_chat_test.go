package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/PerpetualSoftware/pad/internal/cli"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

type fakeChatSink struct {
	messages              []monitorChatMessage
	fetches, acks, totals int
	seq                   int64
	after                 []int64
	limits                []int
	fail                  bool
	total                 int
	agent                 string
	before                func()
}

func (f *fakeChatSink) messagesAfter(n int, after int64, limit int) ([]monitorChatMessage, error) {
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
func (f *fakeChatSink) delivered(n int, seq int64) error { f.acks++; f.seq = seq; return nil }
func (f *fakeChatSink) unread(agent string) (int, error) {
	f.totals++
	f.agent = agent
	return f.total, nil
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
func TestChatFramingByAuthor(t *testing.T) {
	chatSessionEnv(t)
	cli.WriteChatDirection()
	f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("a", 1), {ID: "b", Seq: 2, AuthorAgentKey: "other", Body: "agent"}}}
	var out bytes.Buffer
	c := newMonitorChat(f, newRecentSet(256, 10*60*1e9))
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
	c := newMonitorChat(f, newRecentSet(256, 10*60*1e9))
	var out bytes.Buffer
	p := watchStreamPayload{Conversation: 1, MessageID: "a", MessageSeq: 1}
	c.handle(failChatWriter{}, p)
	if f.acks != 0 {
		t.Fatal("acked failed print")
	}
	c.handle(&out, p)
	c.handle(&out, p)
	if f.acks != 1 || f.seq != 1 || strings.Count(out.String(), "fetched a") != 1 {
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
	before := out.String()
	acks := f.acks
	p.MessageID = "d"
	p.MessageSeq = 4
	c.handle(&out, p)
	if out.String() != before || f.acks != acks {
		t.Fatal("fetch failure printed/acked")
	}
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
		if r.URL.Query().Get("after_seq") != "6" || r.URL.Query().Get("limit") != "1" {
			t.Error(r.URL)
		}
		fmt.Fprint(w, `{"messages":[{"id":"m","seq":7,"author_is_human":true,"body":"OWN-CREDENTIAL-BODY"}]}`)
	}))
	defer h.Close()
	client := cli.NewClientFromURLWithAgentName(h.URL, "rook")
	client.SetAuthToken("monitor-token")
	sink := httpChatSink{client: client}
	var out bytes.Buffer
	c := newMonitorChat(sink, newRecentSet(256, 10*60*1e9))
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "m", MessageSeq: 7, Excerpt: "EVENT-SECRET"})
	if gotAuth != "Bearer monitor-token" || gotAgent != "rook" || acks != 1 || !strings.Contains(out.String(), "OWN-CREDENTIAL-BODY") || strings.Contains(out.String(), "EVENT-SECRET") {
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
	c := newMonitorChat(f, newRecentSet(256, 10*60*1e9))
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "a", MessageSeq: 1})
	if out.Len() != 0 || f.acks != 0 {
		t.Fatal("consent changed during fetch", out.String())
	}
}
func TestMonitorDeleteRetraction(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("a", 1)}}
	c := newMonitorChat(f, newRecentSet(256, 10*60*1e9))
	var out bytes.Buffer
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "never", MessageSeq: 2, Deleted: true})
	if out.Len() != 0 || f.fetches != 0 || f.acks != 0 {
		t.Fatal("unseen tombstone")
	}
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "a", MessageSeq: 1})
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "a", MessageSeq: 1, Deleted: true})
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "a", MessageSeq: 1, Deleted: true})
	if strings.Count(out.String(), "retracted") != 1 || f.fetches != 1 || f.acks != 1 {
		t.Fatal(out.String(), f)
	}
}

func TestMonitorChatGapPagesAndOwnMessages(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{}
	for i := int64(1); i <= 43; i++ {
		m := chatMsg(fmt.Sprint(i), i)
		m.Mine = i == 2
		f.messages = append(f.messages, m)
	}
	c := newMonitorChat(f, newRecentSet(256, 10*60*1e9))
	var out bytes.Buffer
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "1", MessageSeq: 1})
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "43", MessageSeq: 43})
	if f.fetches != 4 || f.acks != 2 || f.seq != 43 || len(strings.Split(strings.TrimSpace(out.String()), "\n")) != 42 || strings.Contains(out.String(), "fetched 2\n") {
		t.Fatal(out.String(), f)
	}
}
