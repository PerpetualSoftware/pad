package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

func waitMonitorEnsureDM(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ensure DM worker did not finish")
	}
}

// TASK-4043: even reconnects and subsequent successful resolves share one worker.
// Kills: E-a (call on every resolve).
func TestMonitorEnsureDM_ExistingDMCallsOnce(t *testing.T) {
	var stderr bytes.Buffer
	d := newMonitorEnsureDM(&stderr)
	calls, sleeps := 0, 0
	d.sleep = func(context.Context, time.Duration) bool { sleeps++; return true }
	ensure := func(_ context.Context, agent string) error {
		calls++
		if agent != "presence-agent" {
			t.Errorf("agent = %q", agent)
		}
		return nil
	}
	stdout := captureStdout(t, func() {
		if done := d.start(context.Background(), "", ensure); done != nil {
			t.Fatal("empty agent started a worker")
		}
		waitMonitorEnsureDM(t, d.start(context.Background(), "presence-agent", ensure))
		// The same lifetime guard is called after a reconnect's identity lookup.
		waitMonitorEnsureDM(t, d.start(context.Background(), "presence-agent", ensure))
	})
	if calls != 1 || sleeps != 0 || stderr.Len() != 0 || stdout != "" {
		t.Fatalf("calls=%d sleeps=%d stderr=%q stdout=%q", calls, sleeps, stderr.String(), stdout)
	}
}

// TASK-4043: exercise the real client error, including errors.As and Retry-After.
// Kills: E-b (no 429 retry); E-c (fixed delay); E-h (lose client Retry-After).
func TestMonitorEnsureDM_RetriesAfter429(t *testing.T) {
	chatSessionEnv(t)
	var stderr bytes.Buffer
	d := newMonitorEnsureDM(&stderr)
	var waits []time.Duration
	d.sleep = func(_ context.Context, delay time.Duration) bool { waits = append(waits, delay); return true }
	var calls atomic.Int32
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/me/conversations" || r.Header.Get("X-Pad-Agent") != "presence-agent" {
			t.Errorf("request = %s %s, agent=%q", r.Method, r.URL, r.Header.Get("X-Pad-Agent"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"agent_key":"presence-agent"}` {
			t.Errorf("body = %s", body)
		}
		if call == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":"rate_limited","message":"slow down"}}`)
			return
		}
		fmt.Fprint(w, `{"number":1}`)
	}))
	defer h.Close()
	client := cli.NewClientFromURLWithAgentName(h.URL, "presence-agent")
	stdout := captureStdout(t, func() {
		waitMonitorEnsureDM(t, d.start(context.Background(), "presence-agent", func(ctx context.Context, agent string) error {
			_, err := client.EnsureChatDMContext(ctx, agent)
			if err != nil {
				var apiErr *cli.APIError
				if !errors.As(err, &apiErr) || apiErr.Status != 429 || apiErr.RetryAfterSeconds != 7 {
					t.Errorf("client error lost status/Retry-After: %+v", err)
				}
			}
			if err != nil {
				return fmt.Errorf("ensure: %w", err)
			}
			return nil
		}))
	})
	if calls.Load() != 2 || !slices.Equal(waits, []time.Duration{7 * time.Second}) || stderr.Len() != 0 || stdout != "" {
		t.Fatalf("calls=%d waits=%v stderr=%q stdout=%q", calls.Load(), waits, stderr.String(), stdout)
	}
}

// TASK-4043 r1 H1 and r2 J1: a 429 with no usable Retry-After, or one too
// large for a Duration, waits the 120s default; the largest value that fits is
// honoured as sent. The absent row goes through the real client, so the
// header-to-zero mapping and the worker's default are tested together.
// Kills: H1-m (no default for 0); J1-m (no overflow bound, 9223372037 wraps).
func TestMonitorEnsureDM_429WithoutRetryAfterWaitsDefault(t *testing.T) {
	maxFits := monitorEnsureDMMaxRetryAfterSeconds
	for _, tc := range []struct {
		name    string
		viaHTTP bool  // a real 429 with no Retry-After header, through the client
		secs    int64 // otherwise: the injected APIError's RetryAfterSeconds
		want    time.Duration
	}{
		{name: "zero", secs: 0, want: 120 * time.Second},
		{name: "negative", secs: -5, want: 120 * time.Second},
		{name: "absent", viaHTTP: true, want: 120 * time.Second},
		{name: "wraps", secs: 9223372037, want: 120 * time.Second},
		{name: "largest that fits", secs: maxFits, want: time.Duration(maxFits) * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// RetryAfterSeconds is an int: on a 32-bit GOARCH these rows cannot
			// be represented, so the overflow they test cannot arise there.
			if tc.secs > math.MaxInt {
				t.Skip("value exceeds int on this GOARCH")
			}
			t.Setenv("HOME", t.TempDir())
			var stderr bytes.Buffer
			d := newMonitorEnsureDM(&stderr)
			var waits []time.Duration
			d.sleep = func(_ context.Context, delay time.Duration) bool { waits = append(waits, delay); return true }
			var calls atomic.Int32
			ensure := func(context.Context, string) error {
				if calls.Add(1) == 1 {
					return &cli.APIError{Status: 429, RetryAfterSeconds: int(tc.secs), Message: "slow down"}
				}
				return nil
			}
			if tc.viaHTTP {
				h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if calls.Add(1) == 1 {
						w.WriteHeader(http.StatusTooManyRequests) // no Retry-After header at all
						fmt.Fprint(w, `{"error":{"code":"rate_limited","message":"slow down"}}`)
						return
					}
					fmt.Fprint(w, `{"number":1}`)
				}))
				defer h.Close()
				client := cli.NewClientFromURLWithAgentName(h.URL, "presence-agent")
				ensure = func(ctx context.Context, agent string) error {
					_, err := client.EnsureChatDMContext(ctx, agent)
					return err
				}
			}
			stdout := captureStdout(t, func() {
				waitMonitorEnsureDM(t, d.start(context.Background(), "presence-agent", ensure))
			})
			if calls.Load() != 2 || !slices.Equal(waits, []time.Duration{tc.want}) || stderr.Len() != 0 || stdout != "" {
				t.Fatalf("calls=%d waits=%v want [%v] stderr=%q stdout=%q", calls.Load(), waits, tc.want, stderr.String(), stdout)
			}
		})
	}
}

// TASK-4043: exhausting the worker never returns an error to the stream path.
// Kills: E-d (cap of four); E-f (failure written to stdout).
func TestMonitorEnsureDM_StopsAtCap(t *testing.T) {
	var stderr bytes.Buffer
	d := newMonitorEnsureDM(&stderr)
	calls, sleeps := 0, 0
	d.sleep = func(context.Context, time.Duration) bool { sleeps++; return true }
	stdout := captureStdout(t, func() {
		waitMonitorEnsureDM(t, d.start(context.Background(), "rook", func(context.Context, string) error {
			calls++
			return &cli.APIError{Status: 429, RetryAfterSeconds: 120, Message: "slow\ndown"}
		}))
	})
	// A stream can still run after the background worker gives up.
	chatSessionEnv(t)
	var stream bytes.Buffer
	newMonitorChat(&fakeChatSink{messages: []monitorChatMessage{chatMsg("after-cap", 1)}}, newRecentSet(256, time.Minute)).stream(&stream, fakeSSEResponse(chatSSE("after-cap", 1, false)), "")
	if calls != 3 || sleeps != 2 || strings.Count(stderr.String(), "\n") != 1 || stdout != "" || !strings.Contains(stream.String(), "fetched after-cap") {
		t.Fatalf("calls=%d sleeps=%d stderr=%q stdout=%q stream=%q", calls, sleeps, stderr.String(), stdout, stream.String())
	}
}

// Kills: E-e (retry any error).
func TestMonitorEnsureDM_Non429DoesNotRetry(t *testing.T) {
	for _, err := range []error{errors.New("network failure"), &cli.APIError{Status: 403, Message: "forbidden"}} {
		t.Run(err.Error(), func(t *testing.T) {
			var stderr bytes.Buffer
			d := newMonitorEnsureDM(&stderr)
			calls, sleeps := 0, 0
			d.sleep = func(context.Context, time.Duration) bool { sleeps++; return true }
			stdout := captureStdout(t, func() {
				waitMonitorEnsureDM(t, d.start(context.Background(), "rook", func(context.Context, string) error { calls++; return err }))
			})
			if calls != 1 || sleeps != 0 || strings.Count(stderr.String(), "\n") != 1 || stdout != "" {
				t.Fatalf("calls=%d sleeps=%d stderr=%q stdout=%q", calls, sleeps, stderr.String(), stdout)
			}
		})
	}
}

// TASK-4043: a stalled HTTP call cannot hold up stream delivery, and shutdown
// cancels both an HTTP attempt and its Retry-After wait.
// Kills: E-g (synchronous ensure); E-i (client drops context); E-j (uncancellable wait).
func TestMonitorEnsureDM_BackgroundAndRootCancellation(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		t.Run(fmt.Sprintf("waiting=%t", waiting), func(t *testing.T) {
			chatSessionEnv(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stderr bytes.Buffer
			d := newMonitorEnsureDM(&stderr)
			entered := make(chan struct{})
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if waiting {
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(429)
					fmt.Fprint(w, `{"error":{"code":"rate_limited","message":"slow down"}}`)
					return
				}
				close(entered)
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
				}
			}))
			defer h.Close()
			client := cli.NewClientFromURLWithAgentName(h.URL, "rook")
			if waiting {
				sleep := d.sleep
				d.sleep = func(ctx context.Context, delay time.Duration) bool {
					close(entered)
					return sleep(ctx, delay)
				}
			}
			stdout := captureStdout(t, func() {
				started := make(chan (<-chan struct{}), 1)
				go func() {
					started <- d.start(ctx, "rook", func(ctx context.Context, agent string) error {
						_, err := client.EnsureChatDMContext(ctx, agent)
						return err
					})
				}()
				var done <-chan struct{}
				select {
				case done = <-started:
				case <-time.After(time.Second):
					cancel()
					t.Fatal("start blocked delivery")
				}
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("worker did not enter request/wait")
				}
				var stream bytes.Buffer
				f := &fakeChatSink{messages: []monitorChatMessage{chatMsg("live", 1)}}
				newMonitorChat(f, newRecentSet(256, time.Minute)).stream(&stream, fakeSSEResponse(chatSSE("live", 1, false)), "")
				if !strings.Contains(stream.String(), "fetched live") {
					t.Fatalf("delivery stalled: %q", stream.String())
				}
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("root cancellation did not stop worker")
				}
			})
			if stdout != "" || stderr.Len() != 0 {
				t.Fatalf("stdout=%q stderr=%q", stdout, stderr.String())
			}
		})
	}
}

// TASK-4043: pin the lifetime, successful-resolution gate, agent source, and
// root-context handoff in the production loop.
// Kills: E-w (remove ensure wiring).
func TestMonitorEnsureDM_WiredAfterIdentity(t *testing.T) {
	data, err := os.ReadFile("cmd_watch.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	start := strings.Index(src, "func runWatchMonitor(")
	src = src[start:]
	init := strings.Index(src, "ensureDM := newMonitorEnsureDM(os.Stderr)")
	loop := strings.Index(src, "\n\tfor {")
	resolve := strings.Index(src, "chat.resolveIdentity = func(")
	guard := strings.Index(src[resolve:], `if identityErr != nil || user.ID == ""`) + resolve
	call := strings.Index(src, `ensureDM.start(sigCtx, req.Header.Get("X-Pad-Agent"),`)
	adopt := strings.Index(src, "return monitorChatIdentity{baseURL, user.ID}")
	if init < 0 || init > loop || guard < resolve || call < guard || call > adopt || !strings.Contains(src[call:adopt], "client.EnsureChatDMContext(ctx, agent)") {
		t.Fatal("DM worker must be once per process, after identity success, using presence agent and root context")
	}
}
