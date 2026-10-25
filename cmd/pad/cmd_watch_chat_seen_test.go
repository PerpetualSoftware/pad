package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// TASK-4071: every chat line the monitor prints is teed, byte for byte, to
// the session's chat-seen log, and no other monitor line is.
func TestMonitorTeesEveryChatSiteAndNothingElse(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{total: 2}
	for seq := int64(1); seq <= 43; seq++ {
		f.messages = append(f.messages, chatMsg(fmt.Sprint(seq), seq))
	}
	var warn bytes.Buffer
	c := newSeededMonitorChat(f, newRecentSet(256, 10*60*1e9))
	c.seen = cli.OpenChatSeen(cli.SessionOwner{PID: 4071, PIDSource: "PAD_SESSION_PID"}, nil, &warn)

	item := watchStreamPayload{Kind: "item.updated", Workspace: "ws", ItemRef: "TASK-1", Actor: "a", Summary: "ITEM-LINE"}
	push := watchStreamPayload{Kind: pushKind, Workspace: "ws", ItemRef: "TASK-2", Actor: "a", Summary: "PUSH-LINE", PushID: "p-4071"}
	sse := func(p watchStreamPayload) string {
		b, _ := json.Marshal(p)
		return "event: notification\ndata: " + string(b) + "\n\n"
	}
	body := sse(item) + sse(push) + chatSSE("43", 43, false) + "event: sync_required\ndata: {}\n\n"

	var out bytes.Buffer
	// stream: unread summary (site 4) at connect and again on resync, the
	// first page of messages (site 2) and its overflow pointer (site 3).
	c.stream(&out, fakeSSEResponse(body), "")
	// retraction (site 1).
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "1", MessageSeq: 1, Deleted: true})

	stdout := out.String()
	for _, want := range []string{"unread messages", "fetched 1\n", "more in C1", "retracted", "ITEM-LINE", "PUSH-LINE", resyncMonitorLine} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("setup: stdout lacks %q:\n%s", want, stdout)
		}
	}
	var chatOnly strings.Builder
	for _, line := range strings.SplitAfter(stdout, "\n") {
		if line == "" || strings.Contains(line, "ITEM-LINE") || strings.Contains(line, "PUSH-LINE") || line == resyncMonitorLine+"\n" {
			continue
		}
		chatOnly.WriteString(line)
	}
	got, err := os.ReadFile(c.seen.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != chatOnly.String() {
		t.Fatalf("chat-seen log is not stdout's chat lines.\nlog:\n%s\nwant:\n%s", got, chatOnly.String())
	}
	if warn.Len() != 0 {
		t.Fatal(warn.String())
	}
}

// A line the agent did not get is not shown to the human either.
func TestMonitorTeesOnlyAfterStdoutWrite(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("a", 1)}}
	var warn bytes.Buffer
	c := newSeededMonitorChat(f, newRecentSet(256, 10*60*1e9))
	c.seen = cli.OpenChatSeen(cli.SessionOwner{PID: 4072, PIDSource: "PAD_SESSION_PID"}, nil, &warn)
	c.handle(failChatWriter{}, watchStreamPayload{Conversation: 1, MessageID: "a", MessageSeq: 1})
	if _, err := os.Stat(c.seen.Path()); !os.IsNotExist(err) {
		t.Fatalf("a failed stdout write must not reach the log; stat err = %v", err)
	}
}

// A broken log never costs the agent its line.
func TestMonitorStdoutSurvivesBrokenChatSeen(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("a", 1), chatMsg("b", 2)}}
	path, _ := cli.ChatSeenPath(4073)
	if err := os.MkdirAll(path, 0700); err != nil { // a directory where the log goes
		t.Fatal(err)
	}
	var warn bytes.Buffer
	c := newSeededMonitorChat(f, newRecentSet(256, 10*60*1e9))
	c.seen = cli.OpenChatSeen(cli.SessionOwner{PID: 4073, PIDSource: "PAD_SESSION_PID"}, nil, &warn)
	var out bytes.Buffer
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "b", MessageSeq: 2})
	if strings.Count(out.String(), "fetched ") != 2 || strings.Count(warn.String(), "\n") != 1 {
		t.Fatalf("stdout %q warn %q", out.String(), warn.String())
	}
}
