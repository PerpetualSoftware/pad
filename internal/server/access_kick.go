package server

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/watchevents"
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
// WORKSPACE (it was deleted, restored or purged). It travels on the watch bus
// as watchevents.KindAccessInvalidated, which is cross-instance on cloud, and
// every instance runs one subscriber (runAccessKickSubscriber) that kicks the
// connections it holds. The kind is internal: watchStreamDelivers drops it,
// so it never reaches a client.
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

// accessKicks returns the server's kicker, created on first use so every
// constructor path (tests included) gets one.
func (s *Server) accessKicks() *accessKicker {
	s.accessKickOnce.Do(func() { s.accessKick = newAccessKicker() })
	return s.accessKick
}

// applyAccessKick kicks the local connections a notification addresses. It
// reacts to the internal KindAccessInvalidated and to the existing
// KindWorkspaceAccessChanged, which every door that changes whether a user
// reaches a workspace already publishes, so those doors need no second call.
func (s *Server) applyAccessKick(n watchevents.Notification) {
	switch n.Kind {
	case watchevents.KindAccessInvalidated, watchevents.KindWorkspaceAccessChanged:
	default:
		return
	}
	k := s.accessKicks()
	k.kickUser(n.TargetUserID)
	if n.Kind == watchevents.KindAccessInvalidated ||
		n.AccessChange == watchevents.AccessDeleted ||
		n.AccessChange == watchevents.AccessRestored ||
		n.AccessChange == watchevents.AccessPurged {
		// Workspace lifecycle reaches every connection on it, including
		// legacy workspace-token streams that have no user to address.
		k.kickWorkspace(n.WorkspaceID)
	}
}

// runAccessKickSubscriber holds this instance's one subscription to the watch
// bus for kicks. It resubscribes after a failure (a Redis outage refuses the
// subscription) with capped backoff, and exits when the bus is closed.
func (s *Server) runAccessKickSubscriber(bus watchevents.Bus) {
	backoff := time.Second
	for {
		ch, _, err := bus.Subscribe()
		if errors.Is(err, watchevents.ErrBusClosed) {
			return
		}
		if err != nil {
			slog.Warn("access kicks: watch bus subscription refused, retrying", "error", err, "in", backoff)
			time.Sleep(backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for n := range ch {
			s.applyAccessKick(n)
		}
		// The channel closed: the bus closed, or dropped this subscriber.
		// Loop to resubscribe; a closed bus refuses and ends the loop.
	}
}

// invalidateUserAccess tells every instance that userID's access or
// credential changed, so their live connections re-check now. Best-effort:
// a failed publish leaves the 60s tick as the backstop, and is logged.
func (s *Server) invalidateUserAccess(userID string) {
	s.publishAccessInvalidated(watchevents.Notification{TargetUserID: userID})
}

// invalidateWorkspaceAccess is invalidateUserAccess for every connection on
// a workspace.
func (s *Server) invalidateWorkspaceAccess(workspaceID string) {
	s.publishAccessInvalidated(watchevents.Notification{WorkspaceID: workspaceID})
}

func (s *Server) publishAccessInvalidated(n watchevents.Notification) {
	if n.TargetUserID == "" && n.WorkspaceID == "" {
		return
	}
	n.Kind = watchevents.KindAccessInvalidated
	n.Timestamp = time.Now().Unix()
	if s.watchEvents == nil {
		// No bus: this instance is the only one, so kick directly.
		s.applyAccessKick(n)
		return
	}
	if err := s.watchEvents.Publish(n); err != nil {
		// Other instances are left to their tick; THIS instance's
		// connections can still be reached, so kick them directly.
		slog.Warn("access kicks: publish failed; other instances fall back to the revalidation tick",
			"user_id", n.TargetUserID, "workspace_id", n.WorkspaceID, "error", err)
		s.applyAccessKick(n)
	}
}
