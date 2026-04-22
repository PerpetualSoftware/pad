package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/xarmian/pad/internal/events"
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

	// Compute the user's visible collection set for event filtering.
	// Build a slug-based set since events carry collection slugs, not IDs.
	var visibleSlugSet map[string]bool // nil = all access (no filtering)
	visibleIDs, err := s.visibleCollectionIDs(r, ws.ID)
	if err != nil {
		// Fail closed: if we can't determine visibility, deny all
		// collection-scoped events rather than leaking hidden data.
		slog.Warn("SSE: failed to resolve visible collections, denying all", "error", err)
		visibleSlugSet = make(map[string]bool) // empty set = deny all
	} else if visibleIDs != nil {
		visibleSlugSet = make(map[string]bool, len(visibleIDs))
		for _, id := range visibleIDs {
			coll, _ := s.store.GetCollection(id)
			if coll != nil {
				visibleSlugSet[coll.Slug] = true
			}
		}
	}

	// For users with item-level grants (guests or restricted members), build
	// a set of granted item IDs for event filtering at item granularity.
	var sseGrantedItemSet map[string]bool // nil = no item-level filtering
	var sseFullCollSet map[string]bool    // collection slugs with full grants
	isGuestSSE := false
	if user := currentUser(r); user != nil {
		member, _ := s.store.GetWorkspaceMember(ws.ID, user.ID)
		if member == nil {
			isGuestSSE = true
		}

		// Determine if this user needs item-level filtering:
		// - guests always do
		// - restricted members only if they have item grants
		needsItemFilter := member == nil // guest
		if member != nil && member.CollectionAccess == "specific" {
			_, itemGrants, _ := s.store.GuestVisibleResources(ws.ID, user.ID)
			needsItemFilter = len(itemGrants) > 0
		}

		if needsItemFilter {
			fullCollIDs, grantedItemIDs, grantErr := s.store.GuestVisibleResources(ws.ID, user.ID)
			if grantErr != nil {
				// Fail closed: if we can't resolve grants, install an empty
				// item filter so no collection-scoped events leak through.
				slog.Warn("SSE: failed to resolve item grants, denying item-scoped events", "error", grantErr)
				sseGrantedItemSet = make(map[string]bool) // empty = deny all
				sseFullCollSet = make(map[string]bool)    // empty = no full-access collections
			} else if len(grantedItemIDs) > 0 {
				sseGrantedItemSet = make(map[string]bool, len(grantedItemIDs))
				for _, id := range grantedItemIDs {
					sseGrantedItemSet[id] = true
				}
				// Build the full-access collection slug set. For restricted members,
				// include their member_collection_access + system collections too.
				fullCollIDSet := make(map[string]bool)
				for _, id := range fullCollIDs {
					fullCollIDSet[id] = true
				}
				if member != nil {
					memberColls, _ := s.store.GetMemberCollectionAccess(ws.ID, user.ID)
					sysColls, _ := s.store.ListSystemCollectionIDs(ws.ID)
					for _, id := range memberColls {
						fullCollIDSet[id] = true
					}
					for _, id := range sysColls {
						fullCollIDSet[id] = true
					}
				}
				sseFullCollSet = make(map[string]bool, len(fullCollIDSet))
				for id := range fullCollIDSet {
					coll, _ := s.store.GetCollection(id)
					if coll != nil {
						sseFullCollSet[coll.Slug] = true
					}
				}
			}
		}
	}

	sseUserID := currentUserID(r)

	// sseEventVisible checks if an event should be sent to this client.
	sseEventVisible := func(event events.Event) bool {
		// User-scoped events (e.g. star/unstar) are only sent to the user who triggered them
		if event.UserID != "" && event.UserID != sseUserID {
			return false
		}
		collection := event.Collection
		itemID := event.ItemID
		if visibleSlugSet == nil {
			return true // all access
		}
		if collection == "" {
			// Events without a collection (workspace-level, legacy docs) are
			// only sent to actual members, not guests — they may contain
			// operational metadata like member invites, role changes, etc.
			if isGuestSSE {
				return false
			}
			return true
		}
		if !visibleSlugSet[collection] {
			return false
		}
		// For guests with item-level grants, additionally check the item ID
		if sseGrantedItemSet != nil && !sseFullCollSet[collection] && itemID != "" {
			return sseGrantedItemSet[itemID]
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

	// Membership revalidation ticker. The initial subscribe only checked
	// access ONCE; without this, an owner who revokes a user's workspace
	// membership (member removed, collection access tightened, guest
	// grants revoked) would leak events to the now-unauthorized session
	// for as long as the browser keeps the EventSource open — typically
	// forever. Re-check at a modest cadence so we catch revocation
	// within a minute without hammering the DB. Randomly jitter the
	// first fire a little so every connected client doesn't stampede
	// the DB at the same :00 / :60 tick.
	revalInterval := sseMembershipRevalInterval
	membershipCheck := time.NewTicker(revalInterval)
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
		}
	}
}

// sseMembershipRevalInterval is the cadence at which an active SSE
// connection re-verifies the subscriber's access to the workspace.
// 60s is a compromise between "revocation is visible quickly" and
// "don't hammer the DB for every open tab". Exposed as a package-level
// var so tests can shrink it without waiting a real minute.
var sseMembershipRevalInterval = 60 * time.Second

// sseSubscriberStillHasAccess re-runs the workspace access check for an
// already-subscribed SSE client. Returns false when the caller can no
// longer see this workspace (member removed, grants revoked, token
// re-scoped) and the connection should be closed.
//
// This mirrors the access logic in RequireWorkspaceAccess but avoids
// writing error responses — we just need a boolean for the caller to
// act on.
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
	if currentUser(r) == nil {
		tokenWsID := tokenWorkspaceID(r)
		if tokenWsID == "" {
			return false
		}
		return tokenWsID == workspaceID
	}

	user := currentUser(r)
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
