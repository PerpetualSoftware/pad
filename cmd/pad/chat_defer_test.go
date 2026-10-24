package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestMonitorChatFailedIdentityDefersDeliveredHistory(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("old1", 1), chatMsg("old2", 2), chatMsg("owed", 3)}, unreadErr: fmt.Errorf("summary unavailable")}
	c := newMonitorChat(nil, newRecentSet(256, 10*time.Minute))
	// Connect failed to identify the user. The event can recover identity,
	// but must still obtain the server's delivered_seq before fetching rows.
	c.retryIdentity = func() { c.setIdentity("server", "user"); c.sink = f; c.retryIdentity = nil }
	var out bytes.Buffer
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "owed", MessageSeq: 3})
	if out.Len() != 0 || f.fetches != 0 || f.acks != 0 || len(c.last) != 0 || len(c.delivered) != 0 {
		t.Fatalf("unresolved summary replayed history: output=%q after=%v acks=%v last=%v delivered=%v", out.String(), f.after, f.ackSeqs, c.last, c.delivered)
	}
}

func TestMonitorChatIdentityRecoveryPrintsOnlyOwedMessages(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("old1", 1), chatMsg("old2", 2), chatMsg("owed3", 3), chatMsg("owed4", 4)}, summaryRows: []models.ConversationUnread{{Number: 1, DeliveredSeq: 2}}, total: 2}
	c := newMonitorChat(nil, newRecentSet(256, 10*time.Minute))
	c.retryIdentity = func() { c.setIdentity("server", "user"); c.sink = f; c.retryIdentity = nil }
	var out bytes.Buffer
	p := watchStreamPayload{Conversation: 1, MessageID: "owed4", MessageSeq: 4}
	c.handle(&out, p)
	// The next event after a successful summary must start at the server floor.
	c.summary(&out)
	c.handle(&out, p)
	if !slices.Equal(f.after, []int64{2}) || !slices.Equal(f.ackSeqs, []int64{4}) || c.delivered[1] != 4 || strings.Count(out.String(), "fetched ") != 2 || strings.Count(out.String(), "fetched owed3\n") != 1 || strings.Count(out.String(), "fetched owed4\n") != 1 || strings.Count(out.String(), "2 unread messages") != 1 {
		t.Fatalf("recovery must print exactly owed rows: output=%q after=%v acks=%v delivered=%v", out.String(), f.after, f.ackSeqs, c.delivered)
	}
}

func TestMonitorChatHandleDoesNotBlockOnIdentity(t *testing.T) {
	chatSessionEnv(t)
	entered, release := make(chan struct{}), make(chan struct{})
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer h.Close()
	defer close(release)
	client := cli.NewClientFromURL(h.URL)
	c := newMonitorChat(nil, newRecentSet(256, 10*time.Minute))
	c.retryIdentity = func() { _, _ = client.GetCurrentUser() }
	done := make(chan struct{})
	go func() {
		c.handle(&bytes.Buffer{}, watchStreamPayload{Conversation: 1, MessageID: "m", MessageSeq: 2})
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("identity was not attempted")
	}
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("handle blocked on hanging GetCurrentUser")
	}
}
