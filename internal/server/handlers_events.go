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

// sseKeepaliveInterval is how often we write a comment line to an
// otherwise-idle SSE stream so (a) intermediate proxies/LBs don't
// idle the connection out, and (b) the kernel-level TCP keep-alive
// has traffic to ride. Must land well inside the http.Server's
// IdleTimeout (httpIdleTimeout in server.go) — the init() guard
// below enforces 3 × keepalive < IdleTimeout so a couple of dropped
// writes don't slip past the deadline.
//
// Bumping this past httpIdleTimeout / 3 without bumping
// httpIdleTimeout in lockstep is a programming error caught at
// process start, not at runtime. BUG-1532.
const sseKeepaliveInterval = 30 * time.Second

func init() {
	if 3*sseKeepaliveInterval >= httpIdleTimeout {
		panic(fmt.Sprintf(
			"invariant violated: 3 × sseKeepaliveInterval (%s) must be < httpIdleTimeout (%s); "+
				"bump httpIdleTimeout in server.go or reduce sseKeepaliveInterval. BUG-1532.",
			3*sseKeepaliveInterval, httpIdleTimeout))
	}
}

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

	// Admin-via-bearer membership gate (BUG-1616). resolveWorkspace
	// above uses a global slug lookup for admin users, so a bearer-
	// borne admin (CLI / PAT / MCP) would otherwise sail through and
	// subscribe to any workspace's event stream without ever appearing
	// in workspace_members. The cookie-borne web UI keeps the bypass
	// (admin can subscribe from /console/admin); bearer callers fall
	// through to a strict membership check, exactly matching the
	// policy enforced by RequireWorkspaceAccess on /api/v1/* and by
	// authorizeCollabAccess on the WS upgrade.
	if u := currentUser(r); u != nil && u.Role == "admin" && isBearerAuth(r) {
		member, err := s.store.GetWorkspaceMember(ws.ID, u.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check workspace access")
			return
		}
		if member == nil {
			s.recordMCPAuthzDenial(r, "not_a_member")
			writeError(w, http.StatusForbidden, "forbidden", "You are not a member of this workspace")
			return
		}
	}

	// Entry membership/grant gate for regular (non-admin) users (TASK-264).
	// resolveWorkspace resolves a UUID ?workspace= param via the GLOBAL,
	// unscoped GetWorkspaceByID lookup (server.go), so an authenticated
	// non-member passing another workspace's UUID reaches this point with a
	// fully-resolved ws — even though the slug form is membership-scoped
	// (GetWorkspacesBySlugForUser → nil → the 404 above). Events are still
	// fail-closed filtered by computeSSEVisibility and the stream is torn
	// down within ~60s by the revalidation tick, so this is not a data leak,
	// but the open connection is a connection-slot DoS and a
	// workspace-existence oracle (200+connected vs 404). Gate the entry
	// exactly like RequireWorkspaceAccess / authorizeCollabAccess: admit
	// only direct members or guest-grant holders.
	//
	// Scope of this branch:
	//   - admin-via-cookie (web UI) keeps its platform-wide bypass, matching
	//     resolveWorkspace's global admin slug lookup and BUG-1616's carve-out;
	//   - admin-via-bearer (CLI / PAT / MCP) was already gated by the branch
	//     above (membership-only), so we skip it here to avoid a redundant query;
	//   - the legacy workspace-scoped token (no user context) was gated by the
	//     branch above that;
	//   - the fresh-install / no-auth path has currentUser(r) == nil, so it
	//     falls through untouched.
	// On failure return 404 (not 403) so the stream reveals nothing about the
	// workspace's existence — matching the slug path and the not-found branch above.
	if u := currentUser(r); u != nil && u.Role != "admin" {
		member, err := s.store.GetWorkspaceMember(ws.ID, u.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check workspace access")
			return
		}
		if member == nil {
			hasGrants, grantErr := s.store.UserHasGrantsInWorkspace(ws.ID, u.ID)
			if grantErr != nil {
				writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check workspace access")
				return
			}
			if !hasGrants {
				s.recordMCPAuthzDenial(r, "not_a_member")
				writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
				return
			}
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
		return sseEventVisibleFor(vis, sseUserID, event)
	}

	// Send initial connected event. If even this first write fails the
	// client is already gone — bail before subscribing to anything
	// else. BUG-1532.
	if err := writeSSEEvent(w, "connected", 0, map[string]string{
		"workspace_id": ws.ID,
		"workspace":    ws.Slug,
	}); err != nil {
		slog.Debug("SSE: initial connected write failed, closing",
			"workspace", ws.Slug, "error", err)
		return
	}
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
				if err := writeSSEEvent(w, "sync_required", 0, map[string]string{
					"reason": "Event buffer exceeded. Full sync required.",
				}); err != nil {
					slog.Debug("SSE: sync_required write failed, closing",
						"workspace", ws.Slug, "error", err)
					return
				}
				flusher.Flush()
			} else if len(missed) > 0 {
				slog.Info("SSE replaying missed events",
					"workspace", ws.Slug, "last_event_id", lastID, "count", len(missed))
				for _, event := range missed {
					if sseEventVisible(event) {
						if err := writeSSEEvent(w, event.Type, event.ID, event); err != nil {
							slog.Debug("SSE: replay write failed, closing",
								"workspace", ws.Slug, "error", err)
							return
						}
						flusher.Flush()
					}
				}
			}
			// If len(missed) == 0: client is caught up, nothing to replay.
		}
	}

	// Keepalive ticker — see sseKeepaliveInterval doc for why the
	// interval is linked to httpIdleTimeout.
	keepalive := time.NewTicker(sseKeepaliveInterval)
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
				// Broken-pipe / EOF on the live event path means the
				// client has gone away. Exit the handler so the bus
				// subscription is released (the deferred Unsubscribe
				// runs) and we stop pulling events off the channel for
				// a peer that can't receive them. ctx.Done() will fire
				// on the next iteration anyway in most cases, but
				// surfacing the write error closes the gap where the
				// client TCP went away but the cancellation hasn't
				// propagated yet. BUG-1532.
				if err := writeSSEEvent(w, event.Type, event.ID, event); err != nil {
					slog.Debug("SSE: event write failed, closing",
						"workspace", ws.Slug, "event_type", event.Type, "error", err)
					return
				}
				flusher.Flush()
			}

		case <-keepalive.C:
			// Send keepalive comment to prevent proxy/LB timeouts.
			// Write error → client gone → exit. Same rationale as the
			// event-write path above. BUG-1532.
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				slog.Debug("SSE: keepalive write failed, closing",
					"workspace", ws.Slug, "error", err)
				return
			}
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
				// instead of reconnecting in a tight loop. Best-effort:
				// if the write fails the client is already gone, which
				// is operationally equivalent to "they got the message"
				// — either way we exit.
				if err := writeSSEEvent(w, "unauthorized", 0, map[string]string{
					"reason": "Your access to this workspace was revoked.",
				}); err != nil {
					slog.Debug("SSE: unauthorized write failed (client likely already gone)",
						"workspace", ws.Slug, "error", err)
				} else {
					flusher.Flush()
				}
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
// sseEventVisibleFor decides whether a single event should be delivered
// to a subscriber with the given visibility snapshot. Extracted from the
// per-connection closure so the rule matrix is unit-testable in
// isolation (the live handler just binds `vis` + `sseUserID`).
func sseEventVisibleFor(vis sseVisibility, sseUserID string, event events.Event) bool {
	// User-scoped events (e.g. star/unstar) only go to the triggering user.
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
	// Determine which slug this subscriber can actually see. For a rename
	// (NewSlug set, BUG-2265) the event is ROUTED by the OLD slug, but a
	// subscriber that reconnected / revalidated AFTER the rename only has the
	// NEW slug in its visibleSlugSet — accept EITHER so the client still learns
	// the rename mapping (Codex). Non-rename events have NewSlug == "", so this
	// reduces to the old single-slug check.
	visibleColl := ""
	if vis.visibleSlugSet[collection] {
		visibleColl = collection
	} else if event.NewSlug != "" && vis.visibleSlugSet[event.NewSlug] {
		visibleColl = event.NewSlug
	}
	if visibleColl == "" {
		return false
	}
	// For subscribers filtered to item-level grants in this collection
	// (no full-collection access):
	if vis.grantedItemSet != nil && !vis.fullCollSet[visibleColl] {
		// Item-scoped events: gate on the specific granted item.
		if itemID != "" {
			return vis.grantedItemSet[itemID]
		}
		// collection.updated (BUG-2265) is itemless but carries ONLY the
		// collection slug(s) — no per-item op/count/timing — and the collection
		// is already confirmed visible above, so it's safe to deliver to
		// item-grant subscribers. They need it to converge their ItemDetail's
		// schema/settings snapshot for the items they CAN see.
		if event.Type == events.CollectionUpdated {
			return true
		}
		// Other itemless collection-scoped events (e.g. the items_bulk_updated
		// batch event, TASK-1668) can't be item-grant-filtered — they'd
		// otherwise leak op/count/timing for items the subscriber can't
		// see. Suppress; these subscribers reconcile their granted items
		// via the next resume/reconnect /items-changes sync instead.
		return false
	}
	return true
}

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
	//
	// Admin bypass ("no filtering") fires for cookie session auth only
	// (BUG-1616). For bearer-borne admin (CLI / PAT / MCP) we compute
	// the real VisibleCollectionIDs based on their actual member /
	// grant rows — same policy as RequireWorkspaceAccess. Without
	// this, a bearer-admin who happens to be a "specific"-access
	// member would still see every collection's events.
	var visibleIDs []string
	var err error
	useAdminBypass := user != nil && user.Role == "admin" && !isBearerAuth(r)
	if user != nil && !useAdminBypass {
		visibleIDs, err = s.store.VisibleCollectionIDs(workspaceID, user.ID)
	}
	// (user == nil || useAdminBypass) falls through with nil
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
	// Admins can access any workspace — but only via cookie session
	// auth (BUG-1616). Bearer-borne admin (CLI / PAT / MCP) falls
	// through to the membership-only check below; revalidation must
	// match the entry-time policy or a live stream would survive past
	// what the entry handler would now reject on reconnect.
	if user.Role == "admin" && !isBearerAuth(r) {
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
	// Bearer-admin (BUG-1616): membership-only stance. Skip the
	// guest-grants fallback so a platform admin who happens to hold
	// a guest grant somewhere can't keep a bearer-borne SSE stream
	// open against a workspace they didn't join.
	if user.Role == "admin" && isBearerAuth(r) {
		return false
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
//
// Returns nil for both the happy path AND the JSON-marshal failure
// path: a marshal error is local to this event and shouldn't tear
// down a healthy stream. Returns the underlying Fprintf error only
// when the actual network write fails — the caller should treat that
// as "the stream is gone" and exit the handler so the bus
// subscription is released and we don't keep pulling events off the
// channel for a broken peer. BUG-1532.
func writeSSEEvent(w http.ResponseWriter, eventType string, eventID int64, data interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error("failed to marshal SSE event", "error", err, "event_type", eventType)
		return nil
	}
	if eventID > 0 {
		_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", eventID, eventType, jsonData)
	} else {
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, jsonData)
	}
	return err
}
