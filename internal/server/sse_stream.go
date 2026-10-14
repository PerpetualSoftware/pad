package server

import (
	"errors"
	"fmt"
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
// kilobytes, so a reader that is merely slow drains one in far less. Pinned
// at no less than twice the keepalive cadence on both streams (the test
// TestSSE_BUG3637_WriteTimeoutOutlastsTheKeepaliveCadence), so a reader that
// can keep up with keepalives is never cut off by this bound.
const sseWriteTimeout = 2 * sseKeepaliveInterval

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
}

// sseFrameWriteTimeout is sseWriteTimeout, or a test's override.
func (s *Server) sseFrameWriteTimeout() time.Duration {
	if s.sseWriteTimeoutOverride > 0 {
		return s.sseWriteTimeoutOverride
	}
	return sseWriteTimeout
}

// errSSENoWriteDeadline is openSSEStream's refusal: the writer cannot take a
// write deadline, so nothing could bound a frame.
var errSSENoWriteDeadline = errors.New("event stream: the response writer does not support write deadlines")

// openSSEStream wraps w, or refuses when w cannot take a write deadline. It
// FAILS CLOSED (codex r1 High): streaming without the deadline is the pinned
// handler this type exists to prevent, so a writer that cannot be bounded is
// not streamed to at all. The probe clears any deadline, which is the state
// net/http leaves a connection in when it has no WriteTimeout. net/http's own
// HTTP/1 and HTTP/2 writers support it; a wrapper without Unwrap does not.
func openSSEStream(w http.ResponseWriter, timeout time.Duration) (*sseStream, error) {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("%w: %w", errSSENoWriteDeadline, err)
	}
	return &sseStream{
		ResponseWriter: w,
		rc:             rc,
		timeout:        timeout,
		dead:           make(chan struct{}),
	}, nil
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
	if err := s.arm(); err != nil {
		return 0, err
	}
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
	if s.arm() != nil {
		return
	}
	if err := s.rc.Flush(); err != nil {
		s.fail(err)
	}
}

// arm sets a fresh deadline for the write about to happen. A failure here
// fails the stream: the write would otherwise be unbounded.
func (s *sseStream) arm() error {
	if err := s.rc.SetWriteDeadline(time.Now().Add(s.timeout)); err != nil {
		s.fail(err)
		return err
	}
	return nil
}

func (s *sseStream) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
		close(s.dead)
	}
}
