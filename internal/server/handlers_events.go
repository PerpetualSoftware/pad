package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// handleSSE streams Server-Sent Events for a workspace.
// GET /api/v1/events?workspace=<slug>
//
// Supports Last-Event-ID: when a client reconnects with a Last-Event-ID header
// (set automatically by the browser's EventSource), the server replays any
// missed events from its in-memory replay buffer before entering the live
// stream. If the requested ID is too old (evicted from the buffer), the server
// sends a "sync_required" event so the client knows to do a full refresh.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	// SSE requires the event bus
	if s.events == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Event streaming is not available")
		return
	}

	// Resolve workspace
	slug := r.URL.Query().Get("workspace")
	if slug == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "workspace query parameter is required")
		return
	}

	ws, err := s.resolveWorkspace(slug, currentUser(r))
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if ws == nil {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
		return
	}

	// Verify workspace access for legacy API tokens (no user context).
	// User-based access is checked by resolveWorkspace above, but
	// legacy tokens store the workspace ID in context — verify it matches.
	if currentUser(r) == nil {
		tokenWsID := tokenWorkspaceID(r)
		if tokenWsID != "" && tokenWsID != ws.ID {
			writeError(w, http.StatusForbidden, "forbidden", "Token not authorized for this workspace")
			return
		}
	}

	// Verify streaming support
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "Streaming not supported")
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering

	// Atomically check SSE connection limits and subscribe in one step.
	// This prevents TOCTOU races where two concurrent requests both pass the
	// limit check before either subscribes.
	ch, ok := s.events.SubscribeIfAllowed(ws.ID, s.sseMaxConnections, s.sseMaxPerWorkspace)
	if !ok {
		slog.Warn("SSE connection limit reached", "workspace", ws.Slug,
			"global_current", s.events.SubscriberCount(), "global_max", s.sseMaxConnections,
			"ws_current", s.events.WorkspaceSubscriberCount(ws.ID), "ws_max", s.sseMaxPerWorkspace)
		writeError(w, http.StatusTooManyRequests, "sse_limit_exceeded", "SSE connection limit reached")
		return
	}
	defer s.events.Unsubscribe(ch)

	// Log warning at 80% global capacity
	if s.sseMaxConnections > 0 {
		total := s.events.SubscriberCount()
		if total >= s.sseMaxConnections*80/100 {
			slog.Warn("SSE connections approaching global limit", "current", total, "max", s.sseMaxConnections)
		}
	}

	// Compute initial visibility. The filter maps are recomputed on every
	// membership revalidation tick so that permission-tightening changes
	// (role downgrade, collection-access narrowed to "specific", item
	// grants revoked) take effect mid-stream — otherwise a connection
	// established when the user was an editor would keep emitting events
	// from collections they no longer see even after access was tightened.
	vis := s.computeSSEVisibility(r, ws.ID)

	sseUserID := currentUserID(r)

	// sseEventVisible checks if an event should be sent to this client.
	// Reads from the current `vis` snapshot so recomputes on each
	// revalidation tick take effect immediately for the next event.
	sseEventVisible := func(event events.Event) bool {
		// User-scoped events (e.g. star/unstar) are only sent to the user who triggered them
		if event.UserID != "" && event.UserID != sseUserID {
			return false
		}
		collection := event.Collection
		itemID := event.ItemID
		if vis.visibleSlugSet == nil {
			return true // all access
		}
		if collection == "" {
			// Events without a collection (workspace-level, legacy docs) are
			// only sent to actual members, not guests — they may contain
			// operational metadata like member invites, role changes, etc.
			if vis.isGuest {
				return false
			}
			return true
		}
		if !vis.visibleSlugSet[collection] {
			return false
		}
		// For guests with item-level grants, additionally check the item ID
		if vis.grantedItemSet != nil && !vis.fullCollSet[collection] && itemID != "" {
			return vis.grantedItemSet[itemID]
		}
		return true
	}

	// Send initial connected event
	writeSSEEvent(w, "connected", 0, map[string]string{
		"workspace_id": ws.ID,
		"workspace":    ws.Slug,
	})
	flusher.Flush()

	// Replay missed events if the client provided Last-Event-ID.
	// The browser's EventSource sends this automatically on reconnect.
	if lastIDStr := r.Header.Get("Last-Event-ID"); lastIDStr != "" {
		lastID, parseErr := strconv.ParseInt(lastIDStr, 10, 64)
		if parseErr == nil && lastID > 0 {
			missed := s.events.EventsSince(ws.ID, lastID)
			if missed == nil {
				// Gap too large — buffer evicted. Tell client to do a full sync.
				slog.Info("SSE replay gap too large, sending sync_required",
					"workspace", ws.Slug, "last_event_id", lastID)
				writeSSEEvent(w, "sync_required", 0, map[string]string{
					"reason": "Event buffer exceeded. Full sync required.",
				})
				flusher.Flush()
			} else if len(missed) > 0 {
				slog.Info("SSE replaying missed events",
					"workspace", ws.Slug, "last_event_id", lastID, "count", len(missed))
				for _, event := range missed {
					if sseEventVisible(event) {
						writeSSEEvent(w, event.Type, event.ID, event)
						flusher.Flush()
					}
				}
			}
			// If len(missed) == 0: client is caught up, nothing to replay.
		}
	}

	// Keepalive ticker
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	// Membership revalidation timer. The initial subscribe only checked
	// access ONCE; without this, an owner who revokes a user's workspace
	// membership (member removed, collection access tightened, guest
	// grants revoked) would leak events to the now-unauthorized session
	// for as long as the browser keeps the EventSource open — typically
	// forever. Re-check at a modest cadence so we catch revocation
	// within a minute without hammering the DB.
	//
	// We use a Timer (rather than a Ticker) so the FIRST fire can be
	// jittered into a random [0, revalInterval) window. Without jitter,
	// a wave of connections made in the same second — a post-deploy
	// reconnect storm, a login wave, a cron-driven dashboard — would
	// all land their revalidation DB reads on the same :00 / :60 tick
	// and spike load once a minute forever after. After the first fire
	// we reset to the regular interval for a steady cadence.
	revalInterval := sseMembershipRevalInterval
	firstDelay := revalInterval
	if revalInterval > 0 {
		firstDelay = time.Duration(rand.Int63n(int64(revalInterval)))
	}
	membershipCheck := time.NewTimer(firstDelay)
	defer membershipCheck.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			// Client disconnected
			return

		case event, ok := <-ch:
			if !ok {
				// Channel closed (unsubscribed)
				return
			}
			if sseEventVisible(event) {
				writeSSEEvent(w, event.Type, event.ID, event)
				flusher.Flush()
			}

		case <-keepalive.C:
			// Send keepalive comment to prevent proxy/LB timeouts
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()

		case <-membershipCheck.C:
			// Re-verify access. If the subscriber has been removed from
			// the workspace (or had all grants revoked) since the SSE
			// connection was established, stop streaming to them.
			if !s.sseSubscriberStillHasAccess(r, ws.ID) {
				slog.Info("SSE: subscriber access revoked mid-stream, closing connection",
					"workspace", ws.Slug, "user_id", sseUserID)
				// Tell the client why — the frontend's EventSource handler
				// can use this to redirect to login / dismiss the workspace
				// instead of reconnecting in a tight loop.
				writeSSEEvent(w, "unauthorized", 0, map[string]string{
					"reason": "Your access to this workspace was revoked.",
				})
				flusher.Flush()
				return
			}
			// Still a member — but the scope of that membership may have
			// narrowed since the stream opened (role downgraded, collection
			// access tightened to "specific", item grants revoked). Rebuild
			// the filter set so the next event dispatched respects the
			// current grants rather than the ones captured at connect time.
			vis = s.computeSSEVisibility(r, ws.ID)
			// Re-arm the timer at the regular cadence. The first fire was
			// jittered; subsequent fires can be evenly spaced without
			// introducing a stampede because connect-time variance has
			// already spread the clients.
			membershipCheck.Reset(revalInterval)
		}
	}
}

// sseMembershipRevalInterval is the cadence at which an active SSE
// connection re-verifies the subscriber's access to the workspace and
// recomputes the per-event visibility filter set. 60s is a compromise
// between "revocation and role-downgrade are visible quickly" and
// "don't hammer the DB for every open tab". Exposed as a package-level
// var so tests can shrink it without waiting a real minute.
var sseMembershipRevalInterval = 60 * time.Second

// sseVisibility is the per-connection event-filter state. All four
// fields are rebuilt atomically on each revalidation tick so that
// permission-tightening changes take effect for the very next event
// without tearing down and rebuilding the stream.
type sseVisibility struct {
	// visibleSlugSet == nil → user has unrestricted access (admin / owner /
	// editor with no collection scope). A non-nil empty map means deny all
	// collection-scoped events (fail-closed on grant-resolution errors).
	visibleSlugSet map[string]bool
	// grantedItemSet is populated for users whose effective access is
	// item-level (guests, restricted members). nil = no item filtering
	// needed beyond visibleSlugSet.
	grantedItemSet map[string]bool
	// fullCollSet carries the collection slugs where a guest/restricted
	// member has FULL access; grantedItemSet still gates narrower
	// collection accesses.
	fullCollSet map[string]bool
	// isGuest is true when the user is not a direct workspace member —
	// they only reach the SSE stream via per-collection / per-item grants.
	// Used to hide workspace-level events that have no collection attached.
	isGuest bool
}

// computeSSEVisibility resolves the caller's current visibility snapshot.
// Safe to call repeatedly on a live SSE connection — each call fetches
// the latest state from the store so revoked grants or narrowed roles
// stop leaking on the next event dispatch.
//
// Unlike the request-scoped helpers (visibleCollectionIDs)
// this function re-fetches the authenticated user
// from the store so mid-stream role changes (admin demotion,
// user.disabled flips) take effect on the next tick. The cached
// currentUser(r) snapshot is only used to look up which user to
// refresh.
func (s *Server) computeSSEVisibility(r *http.Request, workspaceID string) sseVisibility {
	var v sseVisibility

	// Resolve the current user fresh from the store. Fall back to the
	// cached snapshot if the lookup fails so transient DB errors don't
	// widen visibility unexpectedly.
	var user *models.User
	if cached := currentUser(r); cached != nil {
		if fresh, err := s.store.GetUser(cached.ID); err == nil && fresh != nil {
			user = fresh
		} else {
			if err != nil {
				slog.Warn("SSE: GetUser failed during visibility recompute; using cached snapshot",
					"user_id", cached.ID, "error", err)
			}
			user = cached
		}
	}

	// Collection-level visibility first. Mirrors visibleCollectionIDs
	// but with a freshly-fetched user so a mid-stream admin-to-member
	// demotion immediately applies the collection filter.
	var visibleIDs []string
	var err error
	if user != nil && user.Role != "admin" {
		visibleIDs, err = s.store.VisibleCollectionIDs(workspaceID, user.ID)
	}
	// (user == nil || user.Role == "admin") falls through with nil
	// visibleIDs — nil means "no filtering" in the snapshot below.
	if err != nil {
		// Fail closed: if we can't determine visibility, deny all
		// collection-scoped events rather than leaking hidden data.
		slog.Warn("SSE: failed to resolve visible collections, denying all", "error", err)
		v.visibleSlugSet = make(map[string]bool) // empty = deny
	} else if visibleIDs != nil {
		v.visibleSlugSet = make(map[string]bool, len(visibleIDs))
		for _, id := range visibleIDs {
			coll, _ := s.store.GetCollection(id)
			if coll != nil {
				v.visibleSlugSet[coll.Slug] = true
			}
		}
	}

	// Item-level visibility only applies to users with a resolved identity
	// (guest or restricted member). Legacy workspace-scoped tokens and
	// fresh-install bypass skip this branch and inherit full access via
	// visibleSlugSet == nil.
	if user == nil {
		return v
	}

	member, _ := s.store.GetWorkspaceMember(workspaceID, user.ID)
	if member == nil {
		v.isGuest = true
	}

	// Determine whether this caller actually needs item-level filtering:
	// - guests always do
	// - restricted members ("specific" collection access) do iff they have
	//   item-level grants, otherwise they're filtered by visibleSlugSet alone
	needsItemFilter := member == nil
	if member != nil && member.CollectionAccess == "specific" {
		_, itemGrants, _ := s.store.GuestVisibleResources(workspaceID, user.ID)
		needsItemFilter = len(itemGrants) > 0
	}
	if !needsItemFilter {
		return v
	}

	fullCollIDs, grantedItemIDs, grantErr := s.store.GuestVisibleResources(workspaceID, user.ID)
	if grantErr != nil {
		// Fail closed: if we can't resolve grants, install an empty item
		// filter so no collection-scoped events leak through.
		slog.Warn("SSE: failed to resolve item grants, denying item-scoped events", "error", grantErr)
		v.grantedItemSet = make(map[string]bool)
		v.fullCollSet = make(map[string]bool)
		return v
	}
	if len(grantedItemIDs) == 0 {
		return v
	}

	v.grantedItemSet = make(map[string]bool, len(grantedItemIDs))
	for _, id := range grantedItemIDs {
		v.grantedItemSet[id] = true
	}

	fullCollIDSet := make(map[string]bool)
	for _, id := range fullCollIDs {
		fullCollIDSet[id] = true
	}
	if member != nil {
		memberColls, _ := s.store.GetMemberCollectionAccess(workspaceID, user.ID)
		sysColls, _ := s.store.ListSystemCollectionIDs(workspaceID)
		for _, id := range memberColls {
			fullCollIDSet[id] = true
		}
		for _, id := range sysColls {
			fullCollIDSet[id] = true
		}
	}
	v.fullCollSet = make(map[string]bool, len(fullCollIDSet))
	for id := range fullCollIDSet {
		coll, _ := s.store.GetCollection(id)
		if coll != nil {
			v.fullCollSet[coll.Slug] = true
		}
	}
	return v
}

// sseSubscriberStillHasAccess re-runs the workspace access check for an
// already-subscribed SSE client. Returns false when the caller can no
// longer see this workspace (member removed, grants revoked, token
// re-scoped, admin role demoted) and the connection should be closed.
//
// This mirrors the access logic in RequireWorkspaceAccess but avoids
// writing error responses. Unlike that middleware, we DO NOT trust the
// user struct captured in the request context at connect time — it's a
// snapshot that would not reflect a mid-stream role change (e.g. admin
// demotion). We always GetUser fresh from the store so privilege
// revocation closes streams within one tick.
func (s *Server) sseSubscriberStillHasAccess(r *http.Request, workspaceID string) bool {
	// Fresh-install escape hatch: no users exist → everyone has access.
	// Matches RequireWorkspaceAccess. Cheap to recheck.
	if count, _ := s.store.UserCount(); count == 0 {
		return true
	}

	// Legacy workspace-scoped API token (no user context). The token's
	// scope was baked in at creation time and can only change by token
	// revocation, which nukes the row and fails ValidateToken on the
	// next request — but the SSE connection was already established.
	// We verify the token context is still set + targets this workspace.
	cachedUser := currentUser(r)
	if cachedUser == nil {
		tokenWsID := tokenWorkspaceID(r)
		if tokenWsID == "" {
			return false
		}
		return tokenWsID == workspaceID
	}

	// Re-fetch the user FRESH — the cached snapshot in request context
	// is only as current as session validation. An admin demoted mid-
	// stream should lose workspace-wide access on the next tick.
	user, err := s.store.GetUser(cachedUser.ID)
	if err != nil {
		slog.Warn("SSE revalidation: GetUser failed; keeping connection open",
			"user_id", cachedUser.ID, "error", err)
		return true
	}
	if user == nil {
		// User deleted → revoke.
		return false
	}
	// Disabled user → revoke.
	if user.IsDisabled() {
		return false
	}
	// Admins can access any workspace.
	if user.Role == "admin" {
		return true
	}
	// Direct workspace member?
	member, err := s.store.GetWorkspaceMember(workspaceID, user.ID)
	if err != nil {
		// DB error — err on the side of letting the client stay
		// connected. A persistent failure will be caught on the next
		// HTTP request anyway, and a transient blip shouldn't bounce
		// every open tab.
		slog.Warn("SSE revalidation: GetWorkspaceMember failed; keeping connection open",
			"workspace_id", workspaceID, "user_id", user.ID, "error", err)
		return true
	}
	if member != nil {
		return true
	}
	// Not a member — check guest grants.
	has, err := s.store.UserHasGrantsInWorkspace(workspaceID, user.ID)
	if err != nil {
		slog.Warn("SSE revalidation: UserHasGrantsInWorkspace failed; keeping connection open",
			"workspace_id", workspaceID, "user_id", user.ID, "error", err)
		return true
	}
	return has
}

// writeSSEEvent writes a single SSE event to the response writer.
// If eventID > 0, an "id:" field is included for Last-Event-ID support.
func writeSSEEvent(w http.ResponseWriter, eventType string, eventID int64, data interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error("failed to marshal SSE event", "error", err)
		return
	}
	if eventID > 0 {
		fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", eventID, eventType, jsonData)
	} else {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, jsonData)
	}
}
