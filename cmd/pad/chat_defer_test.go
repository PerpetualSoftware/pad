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
	"testing/synctest"
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
			c.resolveIdentity = func(context.Context) (monitorChatIdentity, chatSink) {
				attempts++
				if !recovered {
					return monitorChatIdentity{}, nil
				}
				return monitorChatIdentity{"server", "user"}, f
			}
			var out bytes.Buffer
			c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "owed", MessageSeq: 3})
			c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "owed", MessageSeq: 3})
			wantAttempts := 1
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
	c.resolveIdentity = func(context.Context) (monitorChatIdentity, chatSink) {
		attempts++
		if attempts == 1 {
			return monitorChatIdentity{}, nil
		}
		return monitorChatIdentity{"server", "user"}, f
	}
	var out bytes.Buffer
	now := time.Now()
	c.now = func() time.Time { return now }
	if c.summary(&out) {
		t.Fatal("connect should fail identity lookup")
	}
	now = now.Add(monitorChatFloorBackoff)
	p := watchStreamPayload{Conversation: 1, MessageID: "owed4", MessageSeq: 4}
	c.handle(&out, p)
	// The successful first-contact step fills from the server floor immediately.
	if attempts != 2 || !slices.Equal(f.after, []int64{2}) || !slices.Equal(f.ackSeqs, []int64{4}) || c.delivered[1] != 4 || strings.Count(out.String(), "fetched ") != 2 || strings.Count(out.String(), "fetched owed3\n") != 1 || strings.Count(out.String(), "fetched owed4\n") != 1 {
		t.Fatalf("recovery must print exactly owed rows: output=%q after=%v acks=%v delivered=%v", out.String(), f.after, f.ackSeqs, c.delivered)
	}
}

// B1 global identity failures must not multiply the deadline by conversation.
func TestMonitorChatIdentityTimeoutGlobalBackoff(t *testing.T) {
	chatSessionEnv(t)
	synctest.Test(t, func(t *testing.T) {
		c := newMonitorChat(nil, newRecentSet(256, time.Minute))
		c.floorTimeout = 3 * time.Second
		now := time.Now()
		c.now = func() time.Time { return now }
		attempts := 0
		c.resolveIdentity = func(ctx context.Context) (monitorChatIdentity, chatSink) {
			attempts++
			<-ctx.Done()
			return monitorChatIdentity{}, nil
		}
		start := time.Now()
		for n := 1; n <= 5; n++ {
			c.handle(io.Discard, floorEvent(n, 1))
		}
		c.stream(io.Discard, fakeSSEResponse("event: sync_required\ndata: {}\n\n"), "")
		c.handle(io.Discard, floorEvent(1, 1))
		if attempts != 1 || time.Since(start) > c.floorTimeout+100*time.Millisecond || len(c.last) != 0 {
			t.Fatalf("global retry storm: attempts=%d elapsed=%v last=%v", attempts, time.Since(start), c.last)
		}
		now = now.Add(monitorChatFloorBackoff)
		c.handle(io.Discard, floorEvent(5, 1))
		if attempts != 2 {
			t.Fatalf("identity retry attempts=%d", attempts)
		}
	})
}

// Identity and GET must share exactly one deadline, including HTTP body reads.
func TestMonitorChatFirstContactSharesDeadline(t *testing.T) {
	chatSessionEnv(t)
	synctest.Test(t, func(t *testing.T) {
		var identityDeadline time.Time
		f := &boundedFloorSink{read: func(ctx context.Context, _ int) (int64, error) {
			deadline, ok := ctx.Deadline()
			if !ok || deadline != identityDeadline {
				t.Fatal("GET got a new deadline")
			}
			<-ctx.Done()
			return 0, ctx.Err()
		}}
		c := newMonitorChat(nil, newRecentSet(256, time.Minute))
		c.floorTimeout = 3 * time.Second
		c.resolveIdentity = func(ctx context.Context) (monitorChatIdentity, chatSink) {
			identityDeadline, _ = ctx.Deadline()
			time.Sleep(2 * time.Second) // Virtual time, no real sleep.
			return monitorChatIdentity{"server", "user"}, f
		}
		start := time.Now()
		c.handle(io.Discard, floorEvent(1, 1))
		if time.Since(start) != c.floorTimeout || len(c.last) != 0 || f.acks != 0 {
			t.Fatal("step exceeded shared bound or handled failed fetch")
		}
	})
}

func TestMonitorChatFirstContactHTTPDeadline(t *testing.T) {
	for _, hang := range []string{"identity", "floor", "floor-body"} {
		t.Run(hang, func(t *testing.T) {
			chatSessionEnv(t)
			requests := 0
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hang == "identity" || strings.Contains(r.URL.Path, "conversations") {
					if hang == "floor-body" {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"delivered_seq":`)
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
					return
				}
				fmt.Fprint(w, `{"id":"user"}`)
			}))
			defer h.Close()
			client := cli.NewClientFromURL(h.URL)
			c := newMonitorChat(nil, newRecentSet(256, time.Minute))
			c.floorTimeout = 20 * time.Millisecond
			c.resolveIdentity = func(ctx context.Context) (monitorChatIdentity, chatSink) {
				requests++
				user, err := client.GetCurrentUserContext(ctx)
				if err != nil {
					return monitorChatIdentity{}, nil
				}
				return monitorChatIdentity{h.URL, user.ID}, httpChatSink{client}
			}
			start := time.Now()
			c.handle(io.Discard, floorEvent(1, 1))
			if requests != 1 || time.Since(start) > time.Second || len(c.last) != 0 {
				t.Fatal("HTTP ignored deadline")
			}
		})
	}
}

func TestMonitorChatIdentityChangeFailedSummaryDiscardsOldBaseline(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{unreadErr: errors.New("summary unavailable"), floorErr: errors.New("floor unavailable")}
	c := newMonitorChat(f, newRecentSet(256, 10*time.Minute))
	c.setIdentity("server", "old-user")
	c.last[1], c.delivered[1], c.printedIDs["old"] = 5, 5, true
	c.resolveIdentity = func(context.Context) (monitorChatIdentity, chatSink) {
		return monitorChatIdentity{"server", "new-user"}, f
	}
	var out bytes.Buffer
	c.resync(&out)
	c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "new", MessageSeq: 6})
	if len(c.last) != 0 || len(c.delivered) != 0 || len(c.printedIDs) != 0 || out.Len() != 0 || f.fetches != 0 || f.acks != 0 {
		t.Fatalf("restored another identity's baseline: last=%v delivered=%v printed=%v output=%q fetches=%d acks=%v", c.last, c.delivered, c.printedIDs, out.String(), f.fetches, f.ackSeqs)
	}
}
