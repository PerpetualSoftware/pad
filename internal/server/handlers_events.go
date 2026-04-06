package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// handleSSE streams Server-Sent Events for a workspace.
// GET /api/v1/events?workspace=<slug>
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

	ws, err := s.store.GetWorkspaceBySlug(slug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if ws == nil {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
		return
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

	// Check SSE connection limits before subscribing
	if s.sseMaxConnections > 0 && s.events.SubscriberCount() >= s.sseMaxConnections {
		slog.Warn("SSE global connection limit reached", "current", s.events.SubscriberCount(), "max", s.sseMaxConnections)
		writeError(w, http.StatusTooManyRequests, "sse_limit_exceeded", "Global SSE connection limit reached")
		return
	}
	if s.sseMaxPerWorkspace > 0 && s.events.WorkspaceSubscriberCount(ws.ID) >= s.sseMaxPerWorkspace {
		slog.Warn("SSE per-workspace connection limit reached", "workspace", ws.Slug, "current", s.events.WorkspaceSubscriberCount(ws.ID), "max", s.sseMaxPerWorkspace)
		writeError(w, http.StatusTooManyRequests, "sse_workspace_limit_exceeded", "Workspace SSE connection limit reached")
		return
	}

	// Subscribe to events for this workspace
	ch := s.events.Subscribe(ws.ID)
	defer s.events.Unsubscribe(ch)

	// Log warning at 80% global capacity
	if s.sseMaxConnections > 0 {
		total := s.events.SubscriberCount()
		if total >= s.sseMaxConnections*80/100 {
			slog.Warn("SSE connections approaching global limit", "current", total, "max", s.sseMaxConnections)
		}
	}

	// Send initial connected event
	writeSSEEvent(w, "connected", map[string]string{
		"workspace_id": ws.ID,
		"workspace":    ws.Slug,
	})
	flusher.Flush()

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
			writeSSEEvent(w, event.Type, event)
			flusher.Flush()

		case <-keepalive.C:
			// Send keepalive comment to prevent proxy/LB timeouts
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// writeSSEEvent writes a single SSE event to the response writer.
func writeSSEEvent(w http.ResponseWriter, eventType string, data interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error("failed to marshal SSE event", "error", err)
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, jsonData)
}
