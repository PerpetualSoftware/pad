package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// BUG-3476: the bundle import watches its upload. These drive importWatch
// against real HTTP servers with short timings.

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// lockedBuf is a bytes.Buffer safe to write from the watcher and read from
// the test.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type watchHarness struct {
	watch       *importWatch
	stderr      *lockedBuf
	statusCalls atomic.Int32
}

// newWatchHarness posts size bytes to srv. statuses are what import-status
// answers, in order; the last repeats.
func newWatchHarness(t *testing.T, srv *httptest.Server, size int64, statuses ...func() (*cli.ImportStatus, error)) *watchHarness {
	t.Helper()
	client := cli.NewClientFromURL(srv.URL)
	body := cli.NewCountingReader(io.LimitReader(zeroReader{}, size))
	h := &watchHarness{stderr: &lockedBuf{}}
	h.watch = &importWatch{
		total: size, body: body, stderr: h.stderr,
		tick:       10 * time.Millisecond,
		stallAfter: 300 * time.Millisecond,
		pollEvery:  20 * time.Millisecond,
		settleWait: 300 * time.Millisecond,
		upload: func(ctx context.Context) (http.Header, error) {
			var out map[string]any
			return client.PostStreamContext(ctx, "/workspaces/import", body, "application/gzip", &out)
		},
		status: func() (*cli.ImportStatus, error) {
			n := int(h.statusCalls.Add(1)) - 1
			if len(statuses) == 0 {
				t.Errorf("import-status asked, but this case expects it never is")
				return nil, cli.ErrImportStatusUnknown
			}
			if n >= len(statuses) {
				n = len(statuses) - 1
			}
			return statuses[n]()
		},
	}
	return h
}

func state(s string) func() (*cli.ImportStatus, error) {
	return func() (*cli.ImportStatus, error) {
		return &cli.ImportStatus{State: s, WorkspaceName: "Imported", WorkspaceSlug: "imported"}, nil
	}
}

func unknown() (*cli.ImportStatus, error) { return nil, cli.ErrImportStatusUnknown }

func readAllThen(after func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		after(w, r)
	}))
}

func TestBUG3476_SuccessReturnsTheAnswer(t *testing.T) {
	srv := readAllThen(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"slug":"imported"}`))
	})
	defer srv.Close()
	// A poll may land between the last byte and the answer; running keeps
	// the wait going, as it would against a real server.
	h := newWatchHarness(t, srv, 1<<20, state(cli.ImportStateRunning))
	_, resolved, err := h.watch.run()
	if err != nil || resolved != nil {
		t.Fatalf("run = (%v, %v), want success", resolved, err)
	}
}

func TestBUG3476_ServerErrorEnvelopeIsTheOutcome(t *testing.T) {
	srv := readAllThen(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"validation_error","message":"bad bundle"}}`))
	})
	defer srv.Close()
	h := newWatchHarness(t, srv, 1<<20, state(cli.ImportStateRunning))
	_, _, err := h.watch.run()
	if err == nil || !strings.Contains(err.Error(), "bad bundle") {
		t.Fatalf("err = %v, want the server's own answer", err)
	}
}

// The server stops reading: the transport stops taking bytes, the watcher
// abandons the upload and reports what the server did with the attempt.
func TestBUG3476_StalledUploadIsAbandonedAndResolved(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // never read the body
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	h := newWatchHarness(t, srv, 1<<30, state(cli.ImportStateRemoved))
	start := time.Now()
	_, _, err := h.watch.run()
	if err == nil || !strings.Contains(err.Error(), "the upload stalled") || !strings.Contains(err.Error(), "safe to re-run") {
		t.Fatalf("err = %v, want a stall resolved as removed", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("stall took %s to notice (stallAfter is 300ms)", took)
	}
	if !strings.Contains(h.stderr.String(), "Uploading:") {
		t.Errorf("no progress line while uploading: %q", h.stderr.String())
	}
}

// Every byte is out and the server finished, but its answer never arrives:
// two settled polls end the wait, and a completed import is reported as such.
func TestBUG3476_LostAnswerResolvesFromTheImportKey(t *testing.T) {
	release := make(chan struct{})
	srv := readAllThen(func(w http.ResponseWriter, r *http.Request) {
		select { // the answer is lost
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer srv.Close()
	defer close(release)

	h := newWatchHarness(t, srv, 1<<20, state(cli.ImportStateRunning), state(cli.ImportStateComplete))
	_, resolved, err := h.watch.run()
	if err != nil || resolved == nil || resolved.status.WorkspaceSlug != "imported" {
		t.Fatalf("run = (%+v, %v), want the import resolved complete", resolved, err)
	}
	if !strings.Contains(h.stderr.String(), "Upload complete; the server is importing.") {
		t.Errorf("no upload-complete line: %q", h.stderr.String())
	}
}

// The connection dies with no answer. A server that has no record of the key
// (an older server, or a restart) is an UNKNOWN outcome, never "nothing made".
func TestBUG3476_UnknownOutcomeSaysCheckBeforeRerunning(t *testing.T) {
	srv := readAllThen(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	})
	defer srv.Close()
	h := newWatchHarness(t, srv, 1<<20, unknown)
	_, _, err := h.watch.run()
	if err == nil || !strings.Contains(err.Error(), "outcome is unknown") || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want an unknown outcome with the duplicate warning", err)
	}
}

// A failure while the server is still finishing waits for it to settle.
func TestBUG3476_RunningAttemptIsWaitedOut(t *testing.T) {
	srv := readAllThen(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	})
	defer srv.Close()
	h := newWatchHarness(t, srv, 1<<20, state(cli.ImportStateRunning), state(cli.ImportStateRunning), state(cli.ImportStateNotCreated))
	_, _, err := h.watch.run()
	if err == nil || !strings.Contains(err.Error(), "before any workspace was created") {
		t.Fatalf("err = %v, want the settled not_created outcome", err)
	}
}

// The partial workspace a mid-stream data error left is named.
func TestBUG3476_KeptPartialWorkspaceIsNamed(t *testing.T) {
	srv := readAllThen(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	})
	defer srv.Close()
	h := newWatchHarness(t, srv, 1<<20, state(cli.ImportStateKept))
	_, _, err := h.watch.run()
	if err == nil || !strings.Contains(err.Error(), `partial workspace "Imported" (slug: imported) was kept`) {
		t.Fatalf("err = %v, want the kept workspace named", err)
	}
}
