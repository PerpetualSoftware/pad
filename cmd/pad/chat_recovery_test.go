package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Summary failure does not gate the independent per-conversation floor.
func TestMonitorChatFailedResyncUnlistedConversationDefers(t *testing.T) {
	chatSessionEnv(t)
	f := &fakeChatSink{floor: 2, floorErr: errors.New("floor unavailable"), messages: []monitorChatMessage{chatMsg("a", 1), chatMsg("b", 2), chatMsg("c", 3), chatMsg("d", 4)}}
	c := newMonitorChat(f, newRecentSet(256, 10*time.Minute))
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	var out bytes.Buffer
	c.summary(&out)
	f.unreadErr = errors.New("summary unavailable")
	c.resync(&out)
	p := watchStreamPayload{Conversation: 2, MessageID: "d", MessageSeq: 4}
	c.handle(&out, p)
	if out.Len() != 0 || f.fetches != 0 || f.acks != 0 || len(c.last) != 0 || len(c.delivered) != 0 {
		t.Fatal("first contact did not defer")
	}
	f.floorErr = nil
	now = now.Add(monitorChatFloorBackoff)
	c.handle(&out, p)
	if f.floorReads.Load() != 2 || !slices.Equal(f.after, []int64{2}) || !slices.Equal(f.ackSeqs, []int64{4}) || c.delivered[2] != 4 || strings.Contains(out.String(), "fetched a") || strings.Contains(out.String(), "fetched b") || strings.Count(out.String(), "fetched c\n") != 1 || strings.Count(out.String(), "fetched d\n") != 1 {
		t.Fatalf("after=%v acks=%v output=%q", f.after, f.ackSeqs, out.String())
	}
}

func TestMonitorChatRetractionOutlivesSeenChat(t *testing.T) {
	for _, expiry := range []bool{false, true} {
		t.Run(fmt.Sprintf("expiry=%t", expiry), func(t *testing.T) {
			chatSessionEnv(t)
			seen := newRecentSet(256, 10*time.Minute)
			now := time.Unix(1000, 0)
			seen.now = func() time.Time { return now }
			f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("original", 1)}}
			c := newSeededMonitorChat(f, seen)
			var out bytes.Buffer
			c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "original", MessageSeq: 1})
			if out.Len() == 0 || !seen.contains("original") {
				t.Fatal("original was not printed")
			}
			if expiry {
				now = now.Add(11 * time.Minute)
			} else {
				for i := 0; i < 256; i++ {
					seen.seen(fmt.Sprintf("other-%d", i))
				}
			}
			if seen.contains("original") {
				t.Fatal("test must expire or evict original from seenChat")
			}
			// A cursor seed does not make another process's rows ours to retract.
			c.last[1] = 2
			c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "other-process", MessageSeq: 2, Deleted: true})
			if strings.Contains(out.String(), "retracted") {
				t.Fatal("retracted a row this process never printed")
			}
			p := watchStreamPayload{Conversation: 1, MessageID: "original", MessageSeq: 1, Deleted: true}
			acks := f.acks
			c.handle(failChatWriter{}, p)
			if f.acks != acks || c.last[1] != 2 {
				t.Fatal("failed retraction advanced or acknowledged delivery")
			}
			c.handle(&out, p)
			if strings.Count(out.String(), "message original retracted") != 1 {
				t.Fatalf("owed retraction lost: %q", out.String())
			}
			// The next contiguous tombstone can acknowledge only after the owed
			// retraction has been written. The earlier delivered floor stays at 1.
			c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "tombstone", MessageSeq: 3, Deleted: true})
			if c.delivered[1] != 3 || !slices.Equal(f.ackSeqs, []int64{1, 3}) {
				t.Fatalf("tombstone did not acknowledge: delivered=%v acks=%v", c.delivered, f.ackSeqs)
			}
			c.handle(&out, p)
			if strings.Count(out.String(), "message original retracted") != 1 {
				t.Fatal("duplicate retraction")
			}
		})
	}
}

func TestMonitorChatIdentityChangeResetsMemory(t *testing.T) {
	for _, change := range []string{"same", "base-url", "user", "profile-error", "empty-user", "profile-once"} {
		t.Run(change, func(t *testing.T) {
			chatSessionEnv(t)
			t.Setenv("PAD_URL", "")
			t.Setenv("PAD_MODE", "remote")
			t.Setenv("PAD_TOKEN", "")
			t.Chdir(t.TempDir())
			oldSeen := seenChat
			seenChat = newRecentSet(256, 10*time.Minute)
			t.Cleanup(func() { seenChat = oldSeen })
			// Assigned inside the bubble by runMonitorInBubble's setup.
			var ctx context.Context
			var cancel context.CancelFunc
			var mu sync.Mutex
			connections, profileReads := 0, 0
			// Closed and replaced on every acknowledgement, so the stream
			// handler waits for one without polling (exact virtual time).
			// Made in setup: a channel made outside the bubble never blocks
			// durably, and the bubble's clock would stop on it.
			var ackSignal chan struct{}
			// Virtual time of the first acknowledgement, from the monitor's start.
			var bubbleStart time.Time
			firstAck := time.Duration(-1)
			var afters, acks []int64
			var serverURL string
			writeURL := func(base string) error {
				return os.WriteFile(filepath.Join(".", ".pad.toml"), []byte(fmt.Sprintf("workspace = \"test\"\nurl = %q\n", base)), 0600)
			}
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				path := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/first"), "/second")
				switch path {
				case "/api/v1/auth/me":
					profileReads++
					if change == "profile-once" && profileReads == 1 {
						http.Error(w, "temporary failure", http.StatusServiceUnavailable)
						return
					}
					if change == "profile-error" || change == "empty-user" {
						time.AfterFunc(500*time.Millisecond, cancel)
						if change == "profile-error" {
							http.Error(w, "unavailable", http.StatusServiceUnavailable)
						} else {
							fmt.Fprint(w, `{"id":""}`)
						}
						return
					}
					id := "user1"
					if change == "user" && connections > 1 {
						id = "user2"
					}
					json.NewEncoder(w).Encode(map[string]string{"id": id})
				case "/api/v1/events/stream":
					connections++
					w.Header().Set("Content-Type", "text/event-stream")
					if change == "profile-error" || change == "empty-user" {
						fmt.Fprint(w, "event: notification\ndata: {\"kind\":\"push\",\"summary\":\"push-survives\"}\n\n")
					}
					event := chatSSE("next", 3, false)
					if connections == 1 {
						event = chatSSE("old", 2, false)
					}
					mu.Unlock()
					fmt.Fprint(w, event)
					w.(http.Flusher).Flush()
					if change == "profile-once" && connections == 1 {
						// The failed connect identity lookup now backs off globally. Deliver
						// a fresh event after expiry to exercise recovery on this connection.
						if sleepOrDone(ctx, monitorChatFloorBackoff+time.Second) {
							fmt.Fprint(w, event)
							w.(http.Flusher).Flush()
						}
					}
					// Hold the stream until this single event has been acknowledged.
					for {
						mu.Lock()
						handled, acked := len(acks) >= connections, ackSignal
						mu.Unlock()
						if handled {
							break
						}
						select {
						case <-ctx.Done():
						case <-acked:
							continue
						}
						break
					}
					mu.Lock()
				case "/api/v1/me/conversations":
					// TASK-4043: the background session-start ensure reuses this DM.
					if r.Method != http.MethodPost {
						t.Errorf("ensure DM method = %s, want POST", r.Method)
					}
					fmt.Fprint(w, `{"number":1}`)
				case "/api/v1/me/chat/unread":
					if connections == 1 || change == "profile-once" || change == "base-url" || change == "user" {
						fmt.Fprint(w, `{"total":2,"conversations":[{"number":1,"delivered_seq":0}]}`)
					} else {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
					}
				case "/api/v1/me/conversations/1":
					fmt.Fprint(w, `{"delivered_seq":0}`)
				case "/api/v1/me/conversations/1/messages":
					after, _ := strconv.ParseInt(r.URL.Query().Get("after_seq"), 10, 64)
					afters = append(afters, after)
					rows := []monitorChatMessage{chatMsg("shared", 1), chatMsg("old", 2)}
					if connections > 1 {
						rows[1].Deleted = true
						rows = append(rows, chatMsg("next", 3))
					}
					var messages []monitorChatMessage
					for _, row := range rows {
						if row.Seq > after {
							messages = append(messages, row)
						}
					}
					json.NewEncoder(w).Encode(map[string]any{"messages": messages})
				case "/api/v1/me/conversations/1/delivered":
					var body struct{ Seq int64 }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					acks = append(acks, body.Seq)
					if firstAck < 0 {
						firstAck = time.Since(bubbleStart)
					}
					close(ackSignal)
					ackSignal = make(chan struct{})
					if connections == 1 && change == "base-url" {
						if err := writeURL(serverURL + "/second"); err != nil {
							t.Error(err)
						}
					}
					fmt.Fprint(w, `{}`)
					if connections > 1 {
						cancel()
					}
				default:
					t.Error("unexpected request", r.URL)
					http.NotFound(w, r)
				}
			})
			out, elapsed, err := runMonitorInBubble(t, 25*time.Second, handler, func(bubbleCtx context.Context, bubbleCancel context.CancelFunc, url string) error {
				ctx, cancel, serverURL = bubbleCtx, bubbleCancel, url
				ackSignal = make(chan struct{})
				bubbleStart = time.Now()
				return writeURL(serverURL + "/first")
			})
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			defer mu.Unlock()
			// TASK-4067: exact virtual time. One reconnect backoff (padddBackoff(1),
			// 5s) between the two connections; profile-once also waits out the
			// identity backoff (monitorChatFloorBackoff, 10s) plus 1s before its
			// handler re-sends; the unresolved cases end at the handler's own
			// 500ms cancel, with no reconnect. Literals, not the constants, so a
			// change to either backoff turns this red rather than moving with it.
			wantElapsed := 5 * time.Second
			switch change {
			case "profile-error", "empty-user":
				wantElapsed = 500 * time.Millisecond
			case "profile-once":
				wantElapsed = 16 * time.Second
			}
			if elapsed != wantElapsed {
				t.Fatalf("identity %s: virtual elapsed %v, want %v", change, elapsed, wantElapsed)
			}
			// The first connection's event is acknowledged at once, except in
			// profile-once: its failed connect lookup backs identity off globally,
			// so only the handler's re-send at 11s (10s backoff + 1s) is handled.
			// The unresolved cases never acknowledge.
			wantFirstAck := time.Duration(0)
			switch change {
			case "profile-error", "empty-user":
				wantFirstAck = -1
			case "profile-once":
				wantFirstAck = 11 * time.Second
			}
			if firstAck != wantFirstAck {
				t.Fatalf("identity %s: first acknowledgement at virtual %v, want %v", change, firstAck, wantFirstAck)
			}
			if change == "profile-error" || change == "empty-user" {
				if connections != 1 || len(afters) != 0 || len(acks) != 0 || !strings.Contains(out, "push-survives") || strings.Contains(out, "fetched ") {
					t.Fatalf("unresolved identity affected push or handled chat: connections=%d after=%v acks=%v output=%q", connections, afters, acks, out)
				}
				return
			}
			wantAfters, wantShared := []int64{0, 0}, 2
			wantAcks := []int64{2, 3}
			if change == "same" {
				wantAfters, wantShared = []int64{0, 2}, 1
			}
			acks = slices.Compact(acks)
			if change == "profile-once" {
				wantAfters, wantShared = []int64{0, 2}, 1
			}
			if connections != 2 || !slices.Equal(afters, wantAfters) || !slices.Equal(acks, wantAcks) || strings.Count(out, "fetched shared\n") != wantShared || strings.Count(out, "fetched next\n") != 1 || strings.Contains(out, "retracted") {
				t.Fatalf("identity %s: connections=%d after=%v acks=%v output=%q", change, connections, afters, acks, out)
			}
		})
	}
}
