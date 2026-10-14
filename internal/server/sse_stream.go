package server

import (
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// sseWriteTimeout bounds one frame on a long-lived event stream: a single
// Write or Flush that has not completed in this long ends the stream
// (BUG-3637). The server sets no WriteTimeout, because a stream lives for
// hours, so without this a client that stops reading leaves its handler
// blocked in a write once the socket buffers fill. That handler is the one
// goroutine that would notice cancellation, revalidate access and release the
// admission slot, so all three stop with it.
//
// Generous on purpose: a keepalive is a few bytes and a replay frame a few
// kilobytes, so a reader that is merely slow drains one in far less.
const sseWriteTimeout = 30 * time.Second

// sseStream is the ResponseWriter an event-stream handler writes through once
// it has decided to stream. Every Write and Flush arms a fresh write deadline
// first, and the first failure is sticky: later writes return it at once, and
// dead is closed so the handler's select loop can leave without waiting for
// its next write to fail.
type sseStream struct {
	http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration

	mu   sync.Mutex
	err  error
	dead chan struct{}

	// noDeadline is set once SetWriteDeadline has reported it cannot work on
	// this writer, so the warning is logged once per stream, not per frame.
	noDeadline bool
}

// sseFrameWriteTimeout is sseWriteTimeout, or a test's override.
func (s *Server) sseFrameWriteTimeout() time.Duration {
	if s.sseWriteTimeoutOverride > 0 {
		return s.sseWriteTimeoutOverride
	}
	return sseWriteTimeout
}

func newSSEStream(w http.ResponseWriter, timeout time.Duration) *sseStream {
	return &sseStream{
		ResponseWriter: w,
		rc:             http.NewResponseController(w),
		timeout:        timeout,
		dead:           make(chan struct{}),
	}
}

// Unwrap lets http.ResponseController and middleware see the writer beneath.
func (s *sseStream) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Dead is closed on the stream's first write or flush failure.
func (s *sseStream) Dead() <-chan struct{} { return s.dead }

// Err is the stream's first write or flush failure, or nil.
func (s *sseStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *sseStream) Write(p []byte) (int, error) {
	if err := s.Err(); err != nil {
		return 0, err
	}
	s.arm()
	n, err := s.ResponseWriter.Write(p)
	if err != nil {
		s.fail(err)
	}
	return n, err
}

// Flush satisfies http.Flusher, which has no error return; a failure is
// recorded and reported through Err, Dead and the next Write.
func (s *sseStream) Flush() {
	if s.Err() != nil {
		return
	}
	s.arm()
	if err := s.rc.Flush(); err != nil {
		s.fail(err)
	}
}

func (s *sseStream) arm() {
	if s.noDeadline {
		return
	}
	if err := s.rc.SetWriteDeadline(time.Now().Add(s.timeout)); err != nil {
		s.noDeadline = true
		if errors.Is(err, http.ErrNotSupported) {
			slog.Warn("SSE: write deadline not supported by this ResponseWriter; a stalled reader can pin the stream")
		} else {
			slog.Warn("SSE: set write deadline failed", "error", err)
		}
	}
}

func (s *sseStream) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
		close(s.dead)
	}
}
