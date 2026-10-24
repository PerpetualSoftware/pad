package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

func TestMonitorChatTombstoneEventFillsAfterSync(t *testing.T) {
	for _, printed := range []bool{false, true} {
		t.Run(fmt.Sprintf("printed=%t", printed), func(t *testing.T) {
			chatSessionEnv(t)
			f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("m1", 1), chatMsg("m2", 2), {ID: "m3", Seq: 3, Deleted: true, Body: "TOMBSTONED BODY MUST NEVER PRINT"}, chatMsg("m4", 4)}}
			c := newMonitorChat(f, newRecentSet(256, time.Minute))
			c.last[1] = 99 // sync_required must discard this floor.
			c.printedIDs["m3"] = printed
			var out bytes.Buffer
			c.stream(&out, fakeSSEResponse("event: sync_required\ndata: {}\n\n"), "")
			if len(c.last) != 0 {
				t.Fatal("sync retained floors")
			}
			c.handle(&out, watchStreamPayload{Conversation: 1, MessageID: "m3", MessageSeq: 3, Deleted: true})
			wantRetractions := 0
			if printed {
				wantRetractions = 1
			}
			if f.floorReads.Load() != 1 || !slices.Equal(f.after, []int64{0}) || !slices.Equal(f.limits, []int{20}) || !slices.Equal(f.ackSeqs, []int64{3}) || c.delivered[1] != 3 || c.last[1] != 3 || strings.Count(out.String(), "fetched m1\n") != 1 || strings.Count(out.String(), "fetched m2\n") != 1 || strings.Count(out.String(), "retracted") != wantRetractions || strings.Contains(out.String(), "TOMBSTONED BODY") || strings.Contains(out.String(), "fetched m4") {
				t.Fatalf("tombstone fill: reads=%d after=%v limits=%v acks=%v last=%v delivered=%v output=%q", f.floorReads.Load(), f.after, f.limits, f.ackSeqs, c.last, c.delivered, out.String())
			}
		})
	}
}

func TestMonitorChatSummaryUnreadFailureBackoff(t *testing.T) {
	for _, hang := range []bool{false, true} {
		t.Run(fmt.Sprintf("hang=%t", hang), func(t *testing.T) {
			chatSessionEnv(t)
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				failing := true
				f := &boundedStreamSink{readUnread: func(ctx context.Context) (monitorChatUnread, error) {
					calls++
					if !failing {
						return monitorChatUnread{Total: 2}, nil
					}
					if hang {
						<-ctx.Done()
					}
					return monitorChatUnread{}, errors.New("unread unavailable")
				}}
				c := newMonitorChat(nil, newRecentSet(256, time.Minute))
				c.floorTimeout = 50 * time.Millisecond
				c.resolveIdentity = func(context.Context) (monitorChatIdentity, chatSink) {
					return monitorChatIdentity{"server", "user"}, f
				}
				var out bytes.Buffer
				start := time.Now()
				c.stream(&out, fakeSSEResponse(strings.Repeat("event: sync_required\ndata: {}\n\n", 5)), "")
				wantElapsed := time.Duration(0)
				if hang {
					wantElapsed = c.floorTimeout
				}
				if calls != 1 || time.Since(start) != wantElapsed || strings.Contains(out.String(), "Pad chat:") || c.resolveIdentity != nil {
					t.Fatalf("unread burst: calls=%d elapsed=%v output=%q", calls, time.Since(start), out.String())
				}
				// Every resync still clears a floor installed during summary backoff.
				for i := 0; i < 5; i++ {
					c.last[1] = 42
					c.resync(&out)
					if len(c.last) != 0 || calls != 1 {
						t.Fatalf("resync: floors=%v calls=%d", c.last, calls)
					}
				}
				// Summary backoff must not prevent first-contact message delivery.
				f.messages = []monitorChatMessage{chatMsg("m1", 1)}
				c.handle(&out, floorEvent(1, 1))
				if c.delivered[1] != 1 || f.deliveredSeq != 1 {
					t.Fatal("summary failure blocked delivery")
				}
				failing = false
				time.Sleep(monitorChatFloorBackoff - time.Nanosecond)
				c.resync(&out)
				if calls != 1 {
					t.Fatal("summary retried before expiry")
				}
				time.Sleep(time.Nanosecond)
				c.resync(&out)
				if calls != 2 || strings.Count(out.String(), "Pad chat: 2 unread") != 1 {
					t.Fatalf("summary did not recover: calls=%d output=%q", calls, out.String())
				}
				c.setIdentity("other-server", "other-user")
				c.summary(&out)
				if calls != 3 {
					t.Fatal("identity change retained summary backoff")
				}
			})
		})
	}
}

func TestSessionDirectionClearFailureStillDisarms(t *testing.T) {
	for _, verb := range []string{"disarm", "reset"} {
		t.Run(verb, func(t *testing.T) {
			chatSessionEnv(t)
			path, err := cli.WriteChatDirection()
			if err != nil {
				t.Fatal(err)
			}
			oldClear := clearSessionChatDirection
			t.Cleanup(func() { clearSessionChatDirection = oldClear })
			calls := 0
			clearSessionChatDirection = func() error {
				calls++
				return fmt.Errorf("could not remove chat direction file %s: %w", path, os.ErrPermission)
			}
			cmd := sessionDisarmCmd()
			if verb == "reset" {
				cmd = sessionResetCmd()
			}
			var errout bytes.Buffer
			cmd.SetErr(&errout)
			err = cmd.Execute() // main exits 1 on any Execute error.
			state := "disarmed"
			if verb == "reset" {
				state = "reset"
			}
			message := fmt.Sprintf("session %s; could not remove chat direction file %s: %v", state, path, os.ErrPermission)
			if err == nil || err.Error() != message || !strings.Contains(errout.String(), message) || !errors.Is(err, os.ErrPermission) || calls != 1 {
				t.Errorf("clear failure must return non-zero and name file: err=%v stderr=%q calls=%d", err, errout.String(), calls)
			}
			want := cli.LocalArmOff
			if verb == "reset" {
				want = cli.LocalArmAbsent
			}
			if cli.SessionArmState() != want || cli.ChatDirectionOn() {
				t.Fatal("clear failure prevented disarm/reset")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("failing remover must leave direction file", err)
			}
		})
	}
}
