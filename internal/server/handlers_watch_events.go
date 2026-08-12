package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
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

	watches, werr := s.loadWatchPredicates(r, user.ID)
	if werr != nil {
		slog.Warn("watch-events: failed to load caller's watches, denying watch-scoped matches",
			"user_id", user.ID, "error", werr)
		watches = map[string]string{} // fail closed, not open
	}
	// TASK-2533 codex round 2 finding 2: EVERY notification — watch-
	// matched or addressed-to-you — must pass the same current-access
	// check before delivery, not just watch-matched ones. visCache
	// resolves per-workspace visibility lazily (a notification's
	// workspace isn't known in advance the way `watches`' workspaces
	// are) and is cleared on the same reval tick as `watches` below, so
	// revoked access takes effect within one tick without a reconnect.
	visCache := newWatchVisCache(s, r, user)

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
				if !watchNotificationVisible(watches, visCache.forWorkspace(n.WorkspaceID), user.ID, n) {
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
			if !watchNotificationVisible(watches, visCache.forWorkspace(n.WorkspaceID), user.ID, n) {
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
			fresh, ferr := s.loadWatchPredicates(r, user.ID)
			if ferr != nil {
				slog.Warn("watch-events: watch-list reload failed, keeping previous snapshot",
					"user_id", user.ID, "error", ferr)
				continue
			}
			watches = fresh
			// Same cadence as the watches reload above: a revoked grant
			// or membership must stop widening EITHER visibility source
			// within one tick, not just the watch-matched one.
			visCache.reset()
		}
	}
}

// watchVisCache resolves per-workspace watchAccessVisibility snapshots
// lazily and caches them for the lifetime of one connection between
// reval ticks (TASK-2533 codex round 2 finding 2). A notification's
// workspace isn't known in advance the way the watches map's workspaces
// are — an addressed-to-you assignment can arrive from ANY workspace the
// caller has ever touched — so visibility is resolved on first sight of
// each workspace rather than precomputed. reset() is called on the same
// reval tick that reloads `watches`, so a revoked grant or membership
// takes effect within one tick without requiring a reconnect.
//
// TASK-2533 codex round 3: the round-2 version's reset() cleared only
// the per-workspace map — the cached *models.User itself was captured
// ONCE at connect time and never re-fetched, so a mid-stream admin
// demotion or disable left computeWatchAccessVisibility's admin bypass
// running on stale Role/disabled data until reconnect, on BOTH delivery
// paths (watch-matched and addressed-to-you alike, since both go through
// this same cache). refreshUser (called by both the constructor and
// reset, so the cadence matches computeSSEVisibility's own — see below)
// closes that: it re-fetches the user fresh from the store every reval
// tick, exactly like computeSSEVisibility does on ITS revalidation tick
// in handlers_events.go (that function is invoked once at connect and
// again only on each membershipCheck tick — never per event — so "per
// cache reset" here IS "per tick" there; this is not a narrower cadence
// than the thing it mirrors).
type watchVisCache struct {
	s    *Server
	r    *http.Request
	user *models.User
	// deny, once true, makes forWorkspace return a deny-all
	// watchAccessVisibility{} for every workspace without even calling
	// computeWatchAccessVisibility — set by refreshUser when the user
	// can't be confirmed live and unrestricted. Re-checked (and cleared,
	// if the user becomes valid again) on every subsequent reset(), so a
	// re-enabled or un-demoted user recovers on the next tick rather than
	// staying stuck for the rest of the connection.
	deny bool
	m    map[string]watchAccessVisibility
}

func newWatchVisCache(s *Server, r *http.Request, user *models.User) *watchVisCache {
	c := &watchVisCache{s: s, r: r, user: user, m: make(map[string]watchAccessVisibility)}
	c.refreshUser()
	return c
}

func (c *watchVisCache) forWorkspace(workspaceID string) watchAccessVisibility {
	if c.deny {
		return watchAccessVisibility{}
	}
	if v, ok := c.m[workspaceID]; ok {
		return v
	}
	v := c.s.computeWatchAccessVisibility(c.r, c.user, workspaceID)
	c.m[workspaceID] = v
	return v
}

func (c *watchVisCache) reset() {
	c.m = make(map[string]watchAccessVisibility)
	c.refreshUser()
}

// refreshUser re-fetches c.user fresh from the store, mirroring
// computeSSEVisibility's re-fetch (handlers_events.go) with ONE
// deliberate, documented difference: computeSSEVisibility falls back to
// the STALE cached user on a transient GetUser error or a deleted user,
// reasoned as "a stale-but-previously-valid snapshot can't widen
// visibility beyond what it already had, so don't punish a DB blip."
// This function fails CLOSED instead (sets deny = true) on any of
// {fetch error, user gone, user disabled} — not a blanket "mirror
// exactly" claim, and it shouldn't be described as one. A nudge
// stream's wrong failure mode is different from a browser SSE tab's:
// delivering a fact (an assignment, a watched status change) to someone
// who shouldn't see it is worse here than the stream going briefly
// silent until the next successful tick, so this trades the SSE
// handler's availability-leaning fallback for a stricter one.
func (c *watchVisCache) refreshUser() {
	fresh, err := c.s.store.GetUser(c.user.ID)
	if err != nil {
		slog.Warn("watch-events: GetUser failed during visibility recompute, denying until next tick",
			"user_id", c.user.ID, "error", err)
		c.deny = true
		return
	}
	if fresh == nil {
		// Deleted mid-connection.
		c.deny = true
		return
	}
	if fresh.IsDisabled() {
		c.deny = true
		return
	}
	c.user = fresh
	c.deny = false
}

// loadWatchPredicates returns the caller's active watches as
// itemID -> predicate ("" = unconditional). Filtered through
// filterWatchesByCurrentAccess (TASK-2533 codex round 1 finding 1) so a
// watch on an item the caller can no longer see — workspace membership
// or grant revoked after the watch was created — doesn't keep delivering
// live nudges for it.
func (s *Server) loadWatchPredicates(r *http.Request, userID string) (map[string]string, error) {
	list, err := s.store.ListWatchesForUser(userID)
	if err != nil {
		return nil, err
	}
	list = s.filterWatchesByCurrentAccess(r, userID, list)
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
func watchNotificationVisible(watches map[string]string, vis watchAccessVisibility, userID string, n watchevents.Notification) bool {
	// TASK-2533 codex round 2 finding 2: the caller's CURRENT access to
	// the notification's collection/item is checked FIRST and applies
	// UNIFORMLY to every kind below — watch-matched and addressed-to-you
	// alike. The addressed-to-you branch used to skip this entirely: an
	// item can be assigned to a "specific"-access member whose granted
	// collections don't include it at all (validateAssignmentScope only
	// checks WORKSPACE membership, never collection access), and nothing
	// clears an existing assignment when membership is later revoked —
	// both are live, no-timing-required paths to leaking item_ref/
	// workspace/summary via a bare "you were assigned" fact.
	if !vis.allows(n.CollectionID, n.ItemID) {
		return false
	}

	// Addressed-to-you: the item was just assigned to the caller. Fires
	// regardless of whether the caller also holds an explicit watch on
	// the item (a watch and an addressed-to-you event are independent
	// reasons to be told) — gated above by the SAME access check a
	// watch-matched notification gets, not a bespoke one.
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
