package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
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

	// sseEventVisible checks if an event should be sent to this client.
	sseEventVisible := func(collection string) bool {
		if visibleSlugSet == nil {
			return true // all access
		}
		if collection == "" {
			return true // events without a collection are always sent
		}
		return visibleSlugSet[collection]
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
					if sseEventVisible(event.Collection) {
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
			if sseEventVisible(event.Collection) {
				writeSSEEvent(w, event.Type, event.ID, event)
				flusher.Flush()
			}

		case <-keepalive.C:
			// Send keepalive comment to prevent proxy/LB timeouts
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
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
