package server

import (
	"context"
	"io"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/homehint"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// homeHintStreamBuffer is the per-connection hint buffer for ?home=true
// watch streams. Separate from the watch bus subscriber channel so a slow
// consumer drops hints rather than competing with pushes, status changes or
// chat, and never opens a watch bus gap.
const homeHintStreamBuffer = 16

// homeHintHub fans out home_changed hints to this instance's live ?home=true
// watch streams without touching the watch bus replay ring.
type homeHintHub struct {
	origin string
	mu     sync.Mutex
	subs   map[chan watchevents.Notification]struct{}
}

// register subscribes a live ?home=true stream with a channel of the given
// capacity. The returned unregister removes it, is idempotent, and never
// closes the channel.
func (h *homeHintHub) register(capacity int) (<-chan watchevents.Notification, func()) {
	ch := make(chan watchevents.Notification, capacity)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = make(map[chan watchevents.Notification]struct{})
	}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
		})
	}
}

// deliver performs a non-blocking send of n to every registered subscriber.
// A full channel drops that hint for that subscriber and is counted in dropped.
func (h *homeHintHub) deliver(n watchevents.Notification) (dropped int) {
	h.mu.Lock()
	for ch := range h.subs {
		select {
		case ch <- n:
		default:
			dropped++
		}
	}
	h.mu.Unlock()
	if dropped > 0 {
		slog.Debug("home hints: subscriber channel full, hint dropped",
			"dropped", dropped, "workspace_id", n.WorkspaceID)
	}
	return dropped
}

func (h *homeHintHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// homeHints returns the server's home-hint fan-out hub, created on first use
// so every constructor path (tests included) gets one.
func (s *Server) homeHints() *homeHintHub {
	s.homeHintOnce.Do(func() {
		s.homeHint = &homeHintHub{
			origin: uuid.NewString(),
			subs:   make(map[chan watchevents.Notification]struct{}),
		}
	})
	return s.homeHint
}

// SetHomeHintTransport installs the cross-instance home-hint transport and
// subscribes this instance to it. Idempotent for the same transport;
// installing a different one stops the previous subscription and worker and
// closes the previous transport if it implements io.Closer; nil leaves hints
// local.
func (s *Server) SetHomeHintTransport(t homehint.Transport) {
	s.homeHintTransportMu.Lock()
	defer s.homeHintTransportMu.Unlock()
	if t == s.homeHintTransport {
		return
	}
	if s.homeHintStop != nil {
		s.homeHintStop()
		s.homeHintStop = nil
	}
	if s.homeHintWorker != nil {
		s.homeHintWorker.stop()
		s.homeHintWorker = nil
	}
	if c, ok := s.homeHintTransport.(io.Closer); ok {
		if err := c.Close(); err != nil {
			slog.Warn("home hints: closing the replaced transport failed", "error", err)
		}
	}
	s.homeHintTransport = t
	if t != nil {
		s.homeHintStop = t.Subscribe(s.applyHomeHint)
		s.homeHintWorker = startHomeHintPublisher(t)
	}
}

// homeHintPublisherQueue bounds the hints waiting for the remote publisher.
// Past it, a hint is dropped for other instances only (this instance already
// received it locally).
const homeHintPublisherQueue = 1024

// homeHintPublisher publishes home hints to the transport from one background
// goroutine so no request waits on Redis.
type homeHintPublisher struct {
	ch     chan homehint.Message
	done   chan struct{}
	cancel context.CancelFunc
}

func (p *homeHintPublisher) stop() {
	close(p.done)
	p.cancel()
}

func startHomeHintPublisher(t homehint.Transport) *homeHintPublisher {
	ctx, cancel := context.WithCancel(context.Background())
	p := &homeHintPublisher{
		ch:     make(chan homehint.Message, homeHintPublisherQueue),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	go func() {
		for {
			select {
			case <-p.done:
				return
			case m := <-p.ch:
				if err := t.Publish(ctx, m); err != nil {
					slog.Debug("home hint not sent to other instances",
						"workspace_id", m.WorkspaceID, "error", err)
				}
			}
		}
	}()
	return p
}

// applyHomeHint delivers a hint received from another instance to this
// instance's local subscribers, ignoring echoes of this instance's own
// publishes and messages without a workspace id.
func (s *Server) applyHomeHint(m homehint.Message) {
	hub := s.homeHints()
	if m.Origin == hub.origin || m.WorkspaceID == "" {
		return
	}
	hub.deliver(watchevents.Notification{
		Kind:         watchevents.KindHomeChanged,
		WorkspaceID:  m.WorkspaceID,
		CollectionID: m.CollectionID,
		ItemID:       m.ItemID,
		Timestamp:    m.Timestamp,
	})
}
