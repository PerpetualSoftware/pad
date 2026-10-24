package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

func TestMonitorChatSummaryIdentityTimeoutBackoff(t *testing.T) {
	for _, phase := range []string{"connect", "sync_required"} {
		t.Run(phase, func(t *testing.T) {
			chatSessionEnv(t)
			synctest.Test(t, func(t *testing.T) {
				f := &fakeChatSink{}
				c := newMonitorChat(f, newRecentSet(256, time.Minute))
				c.floorTimeout = 50 * time.Millisecond
				calls := 0
				resolve := func(ctx context.Context) (monitorChatIdentity, chatSink) {
					calls++
					select {
					case <-ctx.Done():
					case <-time.After(10 * c.floorTimeout):
					}
					return monitorChatIdentity{}, nil
				}
				response := fakeSSEResponse(strings.Repeat("event: sync_required\ndata: {}\n\n", 5))
				if phase == "connect" {
					c.resolveIdentity = resolve
				} else {
					response.Body = io.NopCloser(&chatFloorStreamReader{Reader: response.Body, before: func() { c.resolveIdentity = resolve }})
				}
				var out bytes.Buffer
				start := time.Now()
				c.stream(&out, response, "")
				if elapsed := time.Since(start); elapsed != c.floorTimeout || calls != 1 || strings.Contains(out.String(), "Pad chat:") || len(c.last) != 0 || f.totals != map[string]int{"connect": 0, "sync_required": 1}[phase] {
					t.Fatalf("summary must bound and globally back off: elapsed=%v calls=%d unread=%d output=%q floors=%v", elapsed, calls, f.totals, out.String(), c.last)
				}
				if c.identityRetryAfter != time.Now().Add(monitorChatFloorBackoff) {
					t.Fatal("identity backoff must start at completion")
				}
				time.Sleep(monitorChatFloorBackoff)
				c.summary(&out)
				if calls != 2 {
					t.Fatalf("identity not retried at expiry: %d", calls)
				}
			})
		})
	}
}

type boundedStreamSink struct {
	fakeChatSink
	readUnread   func(context.Context) (monitorChatUnread, error)
	readMessages func(context.Context, int, int64, int) ([]monitorChatMessage, error)
	ack          func(context.Context, int, int64) error
}

func (s *boundedStreamSink) unread(ctx context.Context, _ string) (monitorChatUnread, error) {
	return s.readUnread(ctx)
}
func (s *boundedStreamSink) messagesAfter(ctx context.Context, n int, after int64, limit int) ([]monitorChatMessage, error) {
	if s.readMessages != nil {
		return s.readMessages(ctx, n, after, limit)
	}
	return s.fakeChatSink.messagesAfter(ctx, n, after, limit)
}
func (s *boundedStreamSink) delivered(ctx context.Context, n int, seq int64) error {
	if s.ack != nil {
		return s.ack(ctx, n, seq)
	}
	return s.fakeChatSink.delivered(ctx, n, seq)
}

func TestMonitorChatSummarySharedDeadline(t *testing.T) {
	chatSessionEnv(t)
	synctest.Test(t, func(t *testing.T) {
		var identityCtx context.Context
		f := &boundedStreamSink{readUnread: func(ctx context.Context) (monitorChatUnread, error) {
			if ctx != identityCtx {
				t.Fatal("identity and unread must use one context")
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			return monitorChatUnread{Total: 1}, ctx.Err()
		}}
		c := newMonitorChat(nil, newRecentSet(256, time.Minute))
		c.floorTimeout = 50 * time.Millisecond
		c.resolveIdentity = func(ctx context.Context) (monitorChatIdentity, chatSink) {
			identityCtx = ctx
			time.Sleep(c.floorTimeout / 2)
			return monitorChatIdentity{"server", "user"}, f
		}
		start := time.Now()
		var out bytes.Buffer
		c.stream(&out, fakeSSEResponse(""), "")
		if time.Since(start) != c.floorTimeout || out.Len() != 0 || len(c.last) != 0 || c.resolveIdentity != nil || !c.identityRetryAfter.IsZero() {
			t.Fatalf("failed unread must be bounded, silent and install no floor: elapsed=%v output=%q last=%v", time.Since(start), out.String(), c.last)
		}
	})
}

func TestMonitorChatFillAndAckTimeoutBackoff(t *testing.T) {
	for _, operation := range []string{"fill", "ack"} {
		t.Run(operation, func(t *testing.T) {
			chatSessionEnv(t)
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				hanging := true
				wait := func(ctx context.Context) error {
					calls++
					if !hanging {
						return nil
					}
					select {
					case <-ctx.Done():
					case <-time.After(time.Second):
					}
					return context.DeadlineExceeded
				}
				f := &boundedStreamSink{fakeChatSink: fakeChatSink{messages: []monitorChatMessage{chatMsg("m1", 1), chatMsg("m2", 2)}}}
				if operation == "fill" {
					f.readMessages = func(ctx context.Context, n int, after int64, limit int) ([]monitorChatMessage, error) {
						if err := wait(ctx); err != nil {
							return nil, err
						}
						return f.fakeChatSink.messagesAfter(ctx, n, after, limit)
					}
				} else {
					f.ack = func(ctx context.Context, n int, seq int64) error {
						if err := wait(ctx); err != nil {
							return err
						}
						return f.fakeChatSink.delivered(ctx, n, seq)
					}
				}
				c := newMonitorChat(f, newRecentSet(256, time.Minute))
				c.floorTimeout = 50 * time.Millisecond
				c.last[1] = 0 // Backoff must cover conversations that already have a floor.
				var out bytes.Buffer
				start := time.Now()
				c.handle(&out, floorEvent(1, 1))
				if time.Since(start) != c.floorTimeout || calls != 1 || c.retryAfter[1] != time.Now().Add(monitorChatFloorBackoff) {
					t.Fatalf("unbounded or missing backoff: elapsed=%v calls=%d retry=%v", time.Since(start), calls, c.retryAfter)
				}
				if operation == "fill" && (out.Len() != 0 || f.acks != 0 || c.delivered[1] != 0 || c.last[1] != 0) {
					t.Fatal("failed fill printed, acknowledged or advanced")
				}
				if operation == "ack" && (c.delivered[1] != 1 || strings.Count(out.String(), "fetched m1\n") != 1) {
					t.Fatal("ack failure lost local delivery")
				}
				for i := 0; i < 5; i++ {
					c.handle(&out, floorEvent(1, 1))
					c.handle(&out, floorEvent(1, 2))
				}
				if calls != 1 || f.fetches != map[string]int{"fill": 0, "ack": 1}[operation] {
					t.Fatal("network call during backoff")
				}
				hanging = false
				time.Sleep(monitorChatFloorBackoff - time.Nanosecond)
				c.handle(&out, floorEvent(1, 1))
				if calls != 1 {
					t.Fatal("retried before expiry")
				}
				time.Sleep(time.Nanosecond)
				c.handle(&out, floorEvent(1, 1))
				if calls != 2 || c.delivered[1] != 1 || f.deliveredSeq != 1 || strings.Count(out.String(), "fetched m1\n") != 1 {
					t.Fatalf("retry must fill from floor or reack without reprint: calls=%d output=%q delivered=%v", calls, out.String(), c.delivered)
				}
			})
		})
	}
}

// Exercise the adapters too: a fake sink alone cannot prove HTTP propagation.
func TestMonitorChatStreamHTTPDeadlines(t *testing.T) {
	for _, operation := range []string{"unread", "unread-body", "fill", "fill-body", "ack", "ack-error-body"} {
		t.Run(operation, func(t *testing.T) {
			chatSessionEnv(t)
			stop := make(chan struct{})
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if operation == "ack" || operation == "ack-error-body" {
					if strings.HasSuffix(r.URL.Path, "/messages") {
						fmt.Fprint(w, `{"messages":[{"id":"m1","seq":1,"body":"hello"}]}`)
						return
					}
					io.Copy(io.Discard, r.Body)
				}
				if strings.HasSuffix(operation, "body") {
					if operation == "ack-error-body" {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
					fmt.Fprint(w, `{"`)
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-stop:
				}
			}))
			done := make(chan struct{})
			t.Cleanup(func() {
				close(stop)
				h.Close()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("HTTP operation did not finish after releasing the handler")
				}
			})
			c := newMonitorChat(httpChatSink{cli.NewClientFromURL(h.URL)}, newRecentSet(256, time.Minute))
			c.floorTimeout = 20 * time.Millisecond
			c.last[1] = 0
			var out bytes.Buffer
			go func() {
				defer close(done)
				if strings.HasPrefix(operation, "unread") {
					c.summary(&out)
				} else {
					c.handle(&out, floorEvent(1, 1))
				}
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("HTTP ignored the stream deadline")
			}
			if !strings.HasPrefix(operation, "ack") && out.Len() != 0 {
				t.Fatal("failed read printed output")
			}
			if !strings.HasPrefix(operation, "unread") && !c.now().Before(c.retryAfter[1]) {
				t.Fatal("HTTP failure did not back off")
			}
		})
	}
}
