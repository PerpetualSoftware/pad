package server

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/accesskick"
)

// TASK-3365 (Dave's ruling on TASK-3345 #3): revoking access must reach live
// connections NOW, not on their next 60s revalidation tick. The mechanism is
// a KICK, not new authorization: every long-lived connection (collab
// WebSocket, /events SSE, /events/stream) already has a revalidation routine
// that re-checks the credential and the access it holds, and a kick simply
// runs that routine immediately. The ticker stays as the backstop for a kick
// the bus dropped.
//
// A kick is addressed to a USER (their access or credential changed) or to a
// WORKSPACE (it was deleted, restored or purged). Across instances it travels
// on its own transport (internal/accesskick: Redis pub/sub on cloud), NOT on
// the client watch bus, whose sequence ids, replay buffer and per-client
// queues kicks must not consume (codex r1). With no transport (one instance)
// a kick is applied locally.
//
// The residual is accepted (lead ruling): frames already in flight between
// the revoking commit and the kick landing are processed under the old
// access, a window of bus latency plus one store check instead of up to 60s.

// accessKicker indexes this instance's live connections by user and by
// workspace. Each registration holds a buffered(1) channel, so any number of
// kicks before the connection gets to its select coalesce into one re-check,
// and a kick is a non-blocking send: the goroutine that delivers it never
// waits on, or runs, any connection's store checks.
type accessKicker struct {
	mu     sync.Mutex
	byUser map[string]map[*kickReg]struct{}
	byWS   map[string]map[*kickReg]struct{}
}

type kickReg struct {
	ch          chan struct{}
	userID      string
	workspaceID string
}

func newAccessKicker() *accessKicker {
	return &accessKicker{
		byUser: map[string]map[*kickReg]struct{}{},
		byWS:   map[string]map[*kickReg]struct{}{},
	}
}

// register adds a live connection under its user and workspace (either may
// be empty: a legacy workspace-token stream has no user, and the watch
// stream spans every workspace). It returns the connection's kick channel
// and the function that removes it.
func (k *accessKicker) register(userID, workspaceID string) (<-chan struct{}, func()) {
	reg := &kickReg{ch: make(chan struct{}, 1), userID: userID, workspaceID: workspaceID}
	k.mu.Lock()
	if userID != "" {
		if k.byUser[userID] == nil {
			k.byUser[userID] = map[*kickReg]struct{}{}
		}
		k.byUser[userID][reg] = struct{}{}
	}
	if workspaceID != "" {
		if k.byWS[workspaceID] == nil {
			k.byWS[workspaceID] = map[*kickReg]struct{}{}
		}
		k.byWS[workspaceID][reg] = struct{}{}
	}
	k.mu.Unlock()
	var once sync.Once
	return reg.ch, func() {
		once.Do(func() {
			k.mu.Lock()
			defer k.mu.Unlock()
			if set := k.byUser[userID]; set != nil {
				delete(set, reg)
				if len(set) == 0 {
					delete(k.byUser, userID)
				}
			}
			if set := k.byWS[workspaceID]; set != nil {
				delete(set, reg)
				if len(set) == 0 {
					delete(k.byWS, workspaceID)
				}
			}
		})
	}
}

// kickUser and kickWorkspace wake every matching connection and report how
// many they reached. Non-blocking: a connection with a kick already pending
// needs no second one.
func (k *accessKicker) kickUser(userID string) int {
	if userID == "" {
		return 0
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return kickAll(k.byUser[userID])
}

func (k *accessKicker) kickWorkspace(workspaceID string) int {
	if workspaceID == "" {
		return 0
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return kickAll(k.byWS[workspaceID])
}

func kickAll(set map[*kickReg]struct{}) int {
	for reg := range set {
		select {
		case reg.ch <- struct{}{}:
		default:
		}
	}
	return len(set)
}

// workspaceWildcard is the workspace key of a connection that spans every
// workspace (the watch stream): every workspace kick reaches it too.
const workspaceWildcard = "*"

// accessKicks returns the server's kicker, created on first use so every
// constructor path (tests included) gets one.
func (s *Server) accessKicks() *accessKicker {
	s.accessKickOnce.Do(func() { s.accessKick = newAccessKicker() })
	return s.accessKick
}

// applyAccessKick kicks this instance's connections a message addresses.
func (s *Server) applyAccessKick(m accesskick.Message) {
	k := s.accessKicks()
	k.kickUser(m.UserID)
	if m.WorkspaceID != "" {
		k.kickWorkspace(m.WorkspaceID)
		k.kickWorkspace(workspaceWildcard)
	}
}

// SetAccessKickTransport installs the cross-instance kick transport and
// subscribes this instance to it. Idempotent for the same transport;
// installing a different one stops the previous subscription, and nil
// stops it and leaves kicks local.
func (s *Server) SetAccessKickTransport(t accesskick.Transport) {
	s.accessKickMu.Lock()
	defer s.accessKickMu.Unlock()
	if t == s.accessKickTransport {
		return
	}
	if s.accessKickStop != nil {
		s.accessKickStop()
		s.accessKickStop = nil
	}
	if s.accessKickWorker != nil {
		close(s.accessKickWorker.done)
		s.accessKickWorker = nil
	}
	s.accessKickTransport = t
	if t != nil {
		s.accessKickStop = t.Subscribe(s.applyAccessKick)
		s.accessKickWorker = startKickPublisher(t)
	}
}

// kickPublisherQueue bounds the kicks waiting for the remote publisher. Past
// it, a kick is dropped for OTHER instances only (this one was kicked
// directly) and their revalidation tick covers it.
const kickPublisherQueue = 1024

// kickPublisher publishes kicks to the transport from ONE background
// goroutine, so no request waits on Redis (codex r2): a stalled Redis costs
// the queue, never the handler that just committed an access change.
type kickPublisher struct {
	ch   chan accesskick.Message
	done chan struct{}
}

func startKickPublisher(t accesskick.Transport) *kickPublisher {
	p := &kickPublisher{ch: make(chan accesskick.Message, kickPublisherQueue), done: make(chan struct{})}
	go func() {
		for {
			select {
			case <-p.done:
				return
			case m := <-p.ch:
				if err := t.Publish(context.Background(), m); err != nil {
					slog.Warn("access kicks: publish failed; other instances fall back to the revalidation tick",
						"user_id", m.UserID, "workspace_id", m.WorkspaceID, "error", err)
				}
			}
		}
	}()
	return p
}

// invalidateUserAccess tells every instance that userID's access or
// credential changed, so their live connections re-check now. Best-effort:
// a failed publish still kicks this instance, and leaves the others to their
// revalidation tick.
func (s *Server) invalidateUserAccess(userID string) {
	s.publishAccessKick(accesskick.Message{UserID: userID})
}

// invalidateWorkspaceAccess is invalidateUserAccess for every connection on
// a workspace.
func (s *Server) invalidateWorkspaceAccess(workspaceID string) {
	s.publishAccessKick(accesskick.Message{WorkspaceID: workspaceID})
}

func (s *Server) publishAccessKick(m accesskick.Message) {
	if m.UserID == "" && m.WorkspaceID == "" {
		return
	}
	// THIS instance's connections are kicked now, directly. The transport
	// also delivers the kick back here; a second kick coalesces.
	s.applyAccessKick(m)
	s.accessKickMu.Lock()
	w := s.accessKickWorker
	s.accessKickMu.Unlock()
	if w == nil {
		return // one instance
	}
	select {
	case w.ch <- m:
	case <-w.done:
	default:
		slog.Warn("access kicks: publisher queue full; other instances fall back to the revalidation tick",
			"user_id", m.UserID, "workspace_id", m.WorkspaceID)
	}
}

// revalFirstDelay is a connection's first revalidation delay: jittered
// across one interval so a reconnect herd does not re-check in lockstep. A
// variable so tests can make the first tick deterministic.
var revalFirstDelay = func(interval time.Duration) time.Duration {
	if interval <= 0 {
		return interval
	}
	return time.Duration(rand.Int63n(int64(interval)))
}
