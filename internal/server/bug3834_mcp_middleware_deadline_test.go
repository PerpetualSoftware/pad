package server

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// BUG-3834: a response the /mcp middleware chain writes before the transport
// runs (an auth refusal, the rate limiter's 429) had no write deadline:
// mcpBoundWrites wraps only the transport, and boundResponseWrites exempted
// /mcp by path. A keep-alive client that pipelines requests and never reads
// filled both socket buffers with those refusals and left a handler blocked
// in net/http's flush, holding the connection open, for as long as TCP let
// it.
//
// Every pipelined request here is refused by middleware, and the recorder
// proves it: no response reached the transport, so a deadline that fired came
// from boundResponseWrites and not from the transport's own (BUG-3808).

// statusRecorder records each response's status and unwraps, so the
// middleware's http.ResponseController still reaches the connection.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(p)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func TestBUG3834_MiddlewareRefusalsToAStalledPipelinedClientAreBounded(t *testing.T) {
	legs := []struct {
		name string
		// prepare returns the Authorization header the pipelined requests
		// carry, and the statuses the middleware may answer them with.
		prepare func(t *testing.T, f *mcpStallFixture) (auth string, want map[int]bool)
	}{
		// No valid credential: the pre-auth refusal (401, then the per-address
		// floor's 429).
		{"unauthenticated", func(t *testing.T, f *mcpStallFixture) (string, map[int]bool) {
			return "Bearer pad_not-a-token", map[int]bool{http.StatusUnauthorized: true, http.StatusTooManyRequests: true}
		}},
		// A valid token whose per-token bucket is already empty: every
		// request is the limiter's 429. The bucket never refills, or a
		// request would reach the transport mid-run (the production 1/s did,
		// once in 64).
		{"rate_limited", func(t *testing.T, f *mcpStallFixture) (string, map[int]bool) {
			f.srv.rateLimiters.MCPPerToken = newIPRateLimiter(rateLimitConfig{Rate: 0, Burst: 1, Retention: time.Minute})
			for i := 0; ; i++ {
				if i == 10000 {
					t.Fatal("control: the per-token MCP limiter never answered 429")
				}
				rr := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
				r.Header.Set("Authorization", "Bearer "+f.token)
				r.RemoteAddr = "127.0.0.1:1234"
				f.srv.ServeHTTP(rr, r)
				if rr.Code == http.StatusTooManyRequests {
					break
				}
			}
			return "Bearer " + f.token, map[int]bool{http.StatusTooManyRequests: true}
		}},
	}
	for _, leg := range legs {
		t.Run(leg.name, func(t *testing.T) {
			f := newMCPStallFixture(t)
			f.srv.responseWriteTimeoutOverride = 200 * time.Millisecond
			auth, want := leg.prepare(t, f)

			var mu sync.Mutex
			codes := map[int]int{}
			closed := make(chan struct{})
			ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec := &statusRecorder{ResponseWriter: w}
				f.srv.ServeHTTP(rec, r)
				mu.Lock()
				codes[rec.code]++
				mu.Unlock()
			}))
			wf := &writeFailures{failed: make(chan struct{})}
			ts.Listener = &writeFailureListener{Listener: ts.Listener, wf: wf} // 4KB send buffer
			ts.Config.ConnState = func(c net.Conn, st http.ConnState) {
				if st == http.StateClosed {
					close(closed)
				}
			}
			ts.Start()
			t.Cleanup(ts.Close)

			u, err := url.Parse(ts.URL)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := net.Dial("tcp", u.Host)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			t.Cleanup(func() { conn.Close() })
			wf.watch(conn.LocalAddr().String())
			_ = conn.(*net.TCPConn).SetReadBuffer(4096)
			req := "DELETE /mcp HTTP/1.1\r\nHost: " + u.Host + "\r\nAuthorization: " + auth + "\r\n\r\n"
			const pipelined = 2000
			go func() { _, _ = conn.Write([]byte(strings.Repeat(req, pipelined))) }()

			select {
			case <-wf.failed:
			case <-time.After(stallFillCeiling):
				mu.Lock()
				defer mu.Unlock()
				t.Fatalf("no write to the stalled client failed within %v (statuses served: %v): the middleware's refusal is written unbounded",
					stallFillCeiling, codes)
			}
			if !errors.Is(wf.err, os.ErrDeadlineExceeded) {
				t.Fatalf("the first failed write returned %v, want a write deadline overrun", wf.err)
			}
			select {
			case <-closed:
			case <-time.After(time.Until(wf.at.Add(stallReleaseMargin))):
				t.Fatalf("the connection was still open %v after a write overran its deadline", stallReleaseMargin)
			}

			// Instrument: every response was a middleware refusal, so none
			// was written under the transport's own deadline; and the
			// requests did not run out, so the close is the deadline firing
			// on a blocked write.
			mu.Lock()
			defer mu.Unlock()
			total := 0
			for code, n := range codes {
				total += n
				if !want[code] {
					t.Fatalf("instrument: %d response(s) answered %d, not a middleware refusal (statuses: %v)", n, code, codes)
				}
			}
			if total >= pipelined {
				t.Fatalf("instrument: all %d requests were answered, so no response blocked", total)
			}
		})
	}
}
