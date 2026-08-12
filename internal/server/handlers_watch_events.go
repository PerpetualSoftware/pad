package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// watchEventsKeepaliveInterval mirrors sseKeepaliveInterval
// (handlers_events.go) — same rationale, same httpIdleTimeout invariant,
// already enforced by that file's init() guard.
const watchEventsKeepaliveInterval = sseKeepaliveInterval

// watchListRevalInterval is how often an open watch-events stream
// reloads the caller's watch list from the store, mirroring
// sseMembershipRevalInterval's role in handlers_events.go: a watch
// created or removed after the stream connected takes effect within one
// tick instead of requiring a reconnect.
var watchListRevalInterval = 60 * time.Second

// watchEventPayload is the wire shape GET /api/v1/events/stream emits,
// per DOC-2479's contract exactly: {id, ts, workspace, item_ref, kind,
// actor, summary}.
type watchEventPayload struct {
	ID        int64  `json:"id"`
	Ts        int64  `json:"ts"`
	Workspace string `json:"workspace"`
	ItemRef   string `json:"item_ref"`
	Kind      string `json:"kind"`
	Actor     string `json:"actor"`
	Summary   string `json:"summary"`
}

// handleWatchEventsStream streams the caller's watch/nudge notifications.
// GET /api/v1/events/stream (TASK-2533, per DOC-2479's SSE-endpoint
// design).
//
// Unlike handleSSE (GET /api/v1/events, workspace-scoped), this stream is
// USER-scoped and spans every workspace the caller belongs to — a watch
// or an assignment can land in any of them. It is filtered, server-side,
// to exactly two things (DR-2: no firehose, no wildcard subscriptions):
//
//  1. Notifications on an item the caller has an explicit watch on
//     (internal/store's watches table), gated by that watch's optional
//     `--until field=value` predicate.
//  2. "Addressed to you": the item was JUST assigned to the caller.
//     (Phase 1 narrowing, confirmed with the dispatcher: DOC-2479 also
//     describes a "human-gate-shaped collection targets your ... active
//     role" half of addressed-to-you, but this codebase has neither a
//     Collection.Kind field nor any user→active-role binding to ground
//     that mechanically — `pad session register` is the natural future
//     hook for a session-carried role identity, see cmd_session.go. Only
//     the assignment half is implemented; watchevents.KindAsk stays in
//     the wire-contract enum with no producer until that exists.)
func (s *Server) handleWatchEventsStream(w http.ResponseWriter, r *http.Request) {
	if s.watchEvents == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Watch event streaming is not available")
		return
	}

	user := currentUser(r)
	if user == nil {
		// Unlike the workspace-scoped SSE stream, there is no coherent
		// "my watches" concept for a legacy workspace-scoped token or the
		// fresh-install no-auth window — this endpoint requires a
		// resolved user identity, full stop.
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "Streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Subscribe FIRST, replay (if resuming) as part of the SAME atomic
	// call when a Last-Event-ID was supplied (codex round 1 finding 3):
	// subscribing and separately reading the replay buffer left a window
	// where a Notification published in between would be delivered
	// TWICE — once from the replay loop below, once again from the live
	// channel in the main select loop. SubscribeAndReplaySince closes
	// that window structurally; see its doc comment in
	// internal/watchevents.
	var ch chan watchevents.Notification
	var missed []watchevents.Notification
	var lastID int64
	if lastIDStr := r.Header.Get("Last-Event-ID"); lastIDStr != "" {
		if parsed, perr := strconv.ParseInt(lastIDStr, 10, 64); perr == nil && parsed > 0 {
			lastID = parsed
			ch, missed = s.watchEvents.SubscribeAndReplaySince(lastID)
		}
	}
	if ch == nil {
		ch = s.watchEvents.Subscribe()
	}
	defer s.watchEvents.Unsubscribe(ch)

	watches, werr := s.loadWatchPredicates(user.ID)
	if werr != nil {
		slog.Warn("watch-events: failed to load caller's watches, denying watch-scoped matches",
			"user_id", user.ID, "error", werr)
		watches = map[string]string{} // fail closed, not open
	}

	if err := writeSSEEvent(w, "connected", 0, map[string]string{"user_id": user.ID}); err != nil {
		slog.Debug("watch-events: initial connected write failed, closing", "user_id", user.ID, "error", err)
		return
	}
	flusher.Flush()

	if lastID > 0 {
		if missed == nil {
			if err := writeSSEEvent(w, "sync_required", 0, map[string]string{
				"reason": "Notification buffer exceeded. Reconnect without Last-Event-ID to resync.",
			}); err != nil {
				slog.Debug("watch-events: sync_required write failed, closing", "user_id", user.ID, "error", err)
				return
			}
			flusher.Flush()
		} else {
			for _, n := range missed {
				if !watchNotificationVisible(watches, user.ID, n) {
					continue
				}
				if err := writeSSEEvent(w, "notification", n.ID, watchEventPayloadFor(s, n)); err != nil {
					slog.Debug("watch-events: replay write failed, closing", "user_id", user.ID, "error", err)
					return
				}
				flusher.Flush()
			}
		}
	}

	keepalive := time.NewTicker(watchEventsKeepaliveInterval)
	defer keepalive.Stop()
	reval := time.NewTicker(watchListRevalInterval)
	defer reval.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return

		case n, ok := <-ch:
			if !ok {
				return
			}
			if !watchNotificationVisible(watches, user.ID, n) {
				continue
			}
			if err := writeSSEEvent(w, "notification", n.ID, watchEventPayloadFor(s, n)); err != nil {
				slog.Debug("watch-events: notification write failed, closing", "user_id", user.ID, "error", err)
				return
			}
			flusher.Flush()

		case <-keepalive.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				slog.Debug("watch-events: keepalive write failed, closing", "user_id", user.ID, "error", err)
				return
			}
			flusher.Flush()

		case <-reval.C:
			fresh, ferr := s.loadWatchPredicates(user.ID)
			if ferr != nil {
				slog.Warn("watch-events: watch-list reload failed, keeping previous snapshot",
					"user_id", user.ID, "error", ferr)
				continue
			}
			watches = fresh
		}
	}
}

// loadWatchPredicates returns the caller's active watches as
// itemID -> predicate ("" = unconditional). Filtered through
// filterWatchesByCurrentAccess (TASK-2533 codex round 1 finding 1) so a
// watch on an item the caller can no longer see — workspace membership
// or grant revoked after the watch was created — doesn't keep delivering
// live nudges for it.
func (s *Server) loadWatchPredicates(userID string) (map[string]string, error) {
	list, err := s.store.ListWatchesForUser(userID)
	if err != nil {
		return nil, err
	}
	list = s.filterWatchesByCurrentAccess(userID, list)
	m := make(map[string]string, len(list))
	for _, w := range list {
		m[w.ItemID] = w.Predicate
	}
	return m, nil
}

// watchNotificationVisible decides whether a Notification should be
// delivered to a caller holding the given watch map. Extracted from the
// handler so the DR-2 filtering rules are unit-testable without a live
// SSE connection — mirrors sseEventVisibleFor's role in
// handlers_events.go.
func watchNotificationVisible(watches map[string]string, userID string, n watchevents.Notification) bool {
	// Addressed-to-you: the item was just assigned to the caller. Fires
	// regardless of whether the caller also holds an explicit watch on
	// the item (a watch and an addressed-to-you event are independent
	// reasons to be told).
	if n.Kind == watchevents.KindAssignment && n.AssignedUserID != "" && n.AssignedUserID == userID {
		return true
	}

	predicate, watched := watches[n.ItemID]
	if !watched {
		return false
	}
	if predicate == "" {
		return true // unconditional watch: any notification on this item
	}

	// A `--until field=value` predicate (TASK-2533 plan interpretation,
	// confirmed with the dispatcher): "watch this item UNTIL <condition>"
	// reads as a threshold, not "notify me of everything AND also flag
	// this condition" — so a predicated watch fires ONLY on the matching
	// status-change, not on every comment/assignment touching the item.
	field, value, ok := parseWatchPredicate(predicate)
	if !ok {
		// Malformed predicate should never reach here — CreateWatch
		// validates at write time. Fail open to "unconditional" rather
		// than silently dropping a watch the user believes is active.
		return true
	}
	return n.Kind == watchevents.KindStatusChange && n.StatusFieldKey == field && n.ToStatus == value
}

// parseWatchPredicate splits a `field=value` predicate string. DOC-2479
// specs only this single-pair grammar — no boolean combinators.
func parseWatchPredicate(raw string) (field, value string, ok bool) {
	raw = strings.TrimSpace(raw)
	idx := strings.IndexByte(raw, '=')
	if idx <= 0 {
		return "", "", false
	}
	return raw[:idx], raw[idx+1:], true
}

// watchEventPayloadFor resolves a Notification's WorkspaceID to its
// slug (DOC-2479's payload carries a human-facing "workspace", matching
// item_ref's human-facing shape rather than an internal UUID). A
// resolution failure degrades to the raw ID rather than dropping the
// event — the caller still gets everything else in the payload.
//
// Actor mapping (TASK-2533 plan interpretation): DOC-2479's payload
// lists a single `actor` field, but the monitor's own example output —
// `PAD TASK-214 → done (Dave): fix verified` — names a PERSON, not a
// source category ("web"/"cli"/"agent"). Notification carries both
// (Actor = category, ActorName = display name, mirroring the existing
// events.Event shape); this maps the wire `actor` field to ActorName
// with a fallback to the category for actors that have no display name
// (e.g. a webhook or system-sourced mutation), rather than adding a
// second field DOC-2479 doesn't specify.
func watchEventPayloadFor(s *Server, n watchevents.Notification) watchEventPayload {
	workspaceSlug := n.WorkspaceID
	if ws, err := s.store.GetWorkspaceByID(n.WorkspaceID); err == nil && ws != nil {
		workspaceSlug = ws.Slug
	}
	actor := n.ActorName
	if actor == "" {
		actor = n.Actor
	}
	return watchEventPayload{
		ID:        n.ID,
		Ts:        n.Timestamp,
		Workspace: workspaceSlug,
		ItemRef:   n.ItemRef,
		Kind:      n.Kind,
		Actor:     actor,
		Summary:   n.Summary,
	}
}
