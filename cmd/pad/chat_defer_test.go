package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

func TestMonitorChatFailedIdentityDefersDeliveredHistory(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		t.Run(fmt.Sprint(recovered), func(t *testing.T) {
			chatSessionEnv(t)
			f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("old1", 1), chatMsg("old2", 2), chatMsg("owed", 3)}, floorErr: errors.New("floor unavailable")}
			c := newMonitorChat(nil, newRecentSet(256, 10*time.Minute))
			// Connect failed to identify the user. Even if this event recovers
			// identity, it still needs the server floor before fetching rows.
			attempts := 0
			c.resolveIdentity = func() (monitorChatIdentity, chatSink) {
				attempts++
				if !recovered {
					return monitorChatIdentity{}, nil
				}
				return monitorChatIdentity{"server", "user"}, f
			}
			var out bytes.Buffer
			c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "owed", MessageSeq: 3})
			awaitChatSummary(t, c)
			c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "owed", MessageSeq: 3})
			awaitChatFloor(t, c, 1)
			wantAttempts := 2
			if recovered {
				wantAttempts = 1
			}
			if attempts != wantAttempts || out.Len() != 0 || f.fetches != 0 || f.acks != 0 || len(c.last) != 0 || len(c.delivered) != 0 {
				t.Fatalf("unresolved floor replayed history: attempts=%d output=%q after=%v acks=%v last=%v delivered=%v", attempts, out.String(), f.after, f.ackSeqs, c.last, c.delivered)
			}
		})
	}
}

func TestMonitorChatIdentityRecoveryPrintsOnlyOwedMessages(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("old1", 1), chatMsg("old2", 2), chatMsg("owed3", 3), chatMsg("owed4", 4)}, floor: 2, total: 2}
	c := newMonitorChat(nil, newRecentSet(256, 10*time.Minute))
	attempts := 0
	c.resolveIdentity = func() (monitorChatIdentity, chatSink) {
		attempts++
		if attempts == 1 {
			return monitorChatIdentity{}, nil
		}
		return monitorChatIdentity{"server", "user"}, f
	}
	var out bytes.Buffer
	if c.summary(&out) {
		t.Fatal("connect should fail identity lookup")
	}
	p := watchStreamPayload{Conversation: 1, MessageID: "owed4", MessageSeq: 4}
	c.handle(&out, p)
	// The next event after a successful GET must start at the server floor.
	awaitChatSummary(t, c)
	c.handle(&out, p)
	if attempts != 2 || !slices.Equal(f.after, []int64{2}) || !slices.Equal(f.ackSeqs, []int64{4}) || c.delivered[1] != 4 || strings.Count(out.String(), "fetched ") != 2 || strings.Count(out.String(), "fetched owed3\n") != 1 || strings.Count(out.String(), "fetched owed4\n") != 1 {
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
	t.Cleanup(h.Close)

	client := cli.NewClientFromURL(h.URL)
	c := newMonitorChat(nil, newRecentSet(256, 10*time.Minute))
	c.resolveIdentity = func() (monitorChatIdentity, chatSink) {
		_, _ = client.GetCurrentUser()
		return monitorChatIdentity{}, nil
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(release); <-done; awaitChatSummary(t, c) })
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

// Wait for the single asynchronous read, leaving its result for the next event.
// No monitor state or writer is touched by the reader goroutine.
func awaitChatSummary(t *testing.T, c *monitorChat) { t.Helper(); awaitChatFloor(t, c, 1) }

func TestMonitorChatIdentityChangeFailedSummaryDiscardsOldBaseline(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{unreadErr: errors.New("summary unavailable"), floorErr: errors.New("floor unavailable")}
	c := newMonitorChat(f, newRecentSet(256, 10*time.Minute))
	c.setIdentity("server", "old-user")
	c.last[1], c.delivered[1], c.printedIDs["old"] = 5, 5, true
	c.resolveIdentity = func() (monitorChatIdentity, chatSink) { return monitorChatIdentity{"server", "new-user"}, f }
	var out bytes.Buffer
	c.resync(&out)
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "new", MessageSeq: 6})
	awaitChatSummary(t, c)
	if len(c.last) != 0 || len(c.delivered) != 0 || len(c.printedIDs) != 0 || out.Len() != 0 || f.fetches != 0 || f.acks != 0 {
		t.Fatalf("restored another identity's baseline: last=%v delivered=%v printed=%v output=%q fetches=%d acks=%v", c.last, c.delivered, c.printedIDs, out.String(), f.fetches, f.ackSeqs)
	}
}
