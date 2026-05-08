package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
)

// collabUpgrader is the gorilla/websocket Upgrader used by handleCollab.
//
// CheckOrigin defaults: gorilla returns true when the Origin header is
// absent OR when Origin's host equals Request.Host. The pad web UI is
// served by this Go binary, so production traffic is always
// same-origin and the default policy is exactly what we want — no
// extra CORS-style allow-list to keep in sync with the SSE handler.
//
// Buffer sizes left at 4 KiB (gorilla's default) — Yjs binary updates
// produced by typical keystroke-rate edits fit comfortably; large
// initial sync messages (full-document state) get fragmented across
// reads automatically.
var collabUpgrader = websocket.Upgrader{}

// collabMaxMessageBytes caps the size of a single WebSocket message the
// server will accept on a collab connection. Without a cap, an
// authenticated client could send an arbitrarily large frame and force
// the server to buffer it before ReadMessage returns — the auth
// middleware's HTTP body limit no longer applies once the connection
// is upgraded.
//
// 1 MiB is generous for everyday Yjs ops (keystroke-rate updates are
// in the tens-of-bytes range) and still big enough to absorb a full
// initial-sync state for a typical document. If a future workload
// needs more headroom (e.g. very large Y.Doc snapshots), bump this
// alongside any matching CLAUDE.md note.
const collabMaxMessageBytes = 1 << 20 // 1 MiB

// handleCollab is the WebSocket entry point for real-time collab on a
// single item.
//
//	GET /api/v1/collab/{itemID}
//
// Auth + access checks run BEFORE the protocol upgrade (they need to
// be able to write a JSON error response). Once upgraded, this
// handler is intentionally bare — the room manager (TASK-1255) is the
// piece that wires reads + the OpBus together. For now the handler
// just spins up the connection, logs it, drains incoming frames, and
// closes cleanly when the client disconnects. That's enough surface
// area to validate the auth path end-to-end without coupling to
// in-flight room-manager work.
//
// Authorisation re-creates the workspace-access logic from
// RequireWorkspaceAccess but keyed on the item's workspace ID rather
// than a {slug} path param — the WebSocket URL takes only itemID. We
// also re-check freshness of the user (via store.GetUser) so a
// mid-session admin demotion or member removal closes the upgrade
// path immediately, mirroring sseSubscriberStillHasAccess. The
// periodic per-connection revalidation lives in TASK-1256.
func (s *Server) handleCollab(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemID")
	if itemID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "itemID is required")
		return
	}

	item, err := s.store.GetItem(itemID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if item == nil {
		// 404 — same surface as any other item-not-found path.
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}

	if err := s.authorizeCollabAccess(r, item); err != nil {
		var sErr *statusError
		if errors.As(err, &sErr) {
			writeError(w, sErr.code, sErr.kind, sErr.message)
			return
		}
		writeInternalError(w, err)
		return
	}

	// Upgrade. After this returns successfully w/r are hijacked — we
	// MUST NOT touch them; only conn.WriteMessage / conn.Close.
	conn, err := collabUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade itself emits the right HTTP status (e.g. 400 on
		// missing Sec-WebSocket-Key). Just log and bail.
		slog.Warn("collab: websocket upgrade failed",
			"item_id", itemID,
			"error", err,
		)
		return
	}
	defer conn.Close()

	// Cap incoming message size before any read to bound server-side
	// memory pressure from a misbehaving / malicious peer. ReadMessage
	// returns an error when this is exceeded, which our loop handles
	// like any other read error (close the connection cleanly).
	conn.SetReadLimit(collabMaxMessageBytes)

	// Identify the connecting principal in logs. currentUser is nil
	// for legacy workspace-scoped API tokens, fresh-install setups,
	// and similar non-user callers — leave the field empty in that
	// case so log readers can tell the connection came in via a
	// non-user path.
	var userID string
	if u := currentUser(r); u != nil {
		userID = u.ID
	}

	slog.Info("collab: websocket connected",
		"item_id", itemID,
		"workspace_id", item.WorkspaceID,
		"user_id", userID,
		"remote_addr", r.RemoteAddr,
	)
	defer slog.Info("collab: websocket disconnected",
		"item_id", itemID,
		"user_id", userID,
	)

	// Drain reads until the client disconnects. No protocol logic in
	// this task — TASK-1255 (room manager) plumbs reads into the
	// OpBus + op-log. We still need to read so the connection can
	// detect close cleanly: gorilla/websocket only surfaces close
	// frames via ReadMessage, and a control frame (ping/pong) only
	// gets handled if we're actively reading.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			// Normal closure paths (1000, 1001, 1005) are not errors
			// from the user's perspective — log only the unexpected
			// codes so operators don't see noise on every disconnect.
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
				websocket.CloseNoStatusReceived,
			) {
				slog.Warn("collab: websocket read ended unexpectedly",
					"item_id", itemID,
					"user_id", userID,
					"error", err,
				)
			}
			return
		}
	}
}

// statusError lets authorizeCollabAccess return a typed error that
// carries the HTTP status + payload pieces handleCollab should write.
// Keeping it private to this file — a separate utility might emerge
// once another WS handler needs the same shape.
type statusError struct {
	code    int
	kind    string
	message string
}

func (e *statusError) Error() string { return e.message }

func newStatusError(code int, kind, message string) *statusError {
	return &statusError{code: code, kind: kind, message: message}
}

// authorizeCollabAccess mirrors RequireWorkspaceAccess but keyed on
// the item's workspace ID (the WS URL path doesn't carry a workspace
// slug). It checks:
//
//   - Fresh install (no users) → grant.
//   - Legacy workspace-scoped API token → grant if the token's
//     workspace matches the item's workspace.
//   - OAuth token allow-list (TASK-953) → reject when the workspace
//     isn't on the consented list, even for valid members.
//   - Authenticated user → admin OR member OR has guest grants.
//   - Anything else → 403 / 401 as appropriate.
//
// Returns nil on success, a *statusError on a known denial, or a
// non-statusError for store errors.
func (s *Server) authorizeCollabAccess(r *http.Request, item *models.Item) error {
	wsID := item.WorkspaceID

	// Workspace lookup is needed for the OAuth-allow-list slug compare
	// AND so a "vanished workspace" condition surfaces as 404 rather
	// than a confusing 403.
	ws, err := s.store.GetWorkspaceByID(wsID)
	if err != nil {
		return err
	}
	if ws == nil {
		return newStatusError(http.StatusNotFound, "not_found", "Workspace not found")
	}

	// OAuth token allow-list gate.
	if !tokenAllowedWorkspaceMatches(r.Context(), ws.Slug) {
		s.recordMCPAuthzDenial(r, "workspace_not_in_allowlist")
		return newStatusError(http.StatusForbidden, "permission_denied",
			"Token is not authorized for this workspace")
	}

	// Fresh-install escape hatch.
	if count, _ := s.store.UserCount(); count == 0 {
		return nil
	}

	// Legacy API token (workspace-scoped, no user context).
	if tokenWsID := tokenWorkspaceID(r); tokenWsID != "" && currentUser(r) == nil {
		if tokenWsID == wsID {
			return nil
		}
		return newStatusError(http.StatusForbidden, "forbidden",
			"Token not authorized for this workspace")
	}

	user := currentUser(r)
	if user == nil {
		return newStatusError(http.StatusUnauthorized, "unauthorized",
			"Authentication required")
	}

	// Re-fetch the user fresh so a mid-session role demotion is
	// reflected immediately. Mirrors sseSubscriberStillHasAccess.
	fresh, err := s.store.GetUser(user.ID)
	if err != nil {
		return err
	}
	if fresh == nil {
		return newStatusError(http.StatusForbidden, "forbidden", "User not found")
	}
	if fresh.IsDisabled() {
		return newStatusError(http.StatusForbidden, "forbidden", "User is disabled")
	}
	if fresh.Role == "admin" {
		return nil
	}

	// Direct workspace member?
	member, err := s.store.GetWorkspaceMember(wsID, fresh.ID)
	if err != nil {
		return err
	}
	if member != nil {
		return nil
	}

	// Guest via grants?
	hasGrants, err := s.store.UserHasGrantsInWorkspace(wsID, fresh.ID)
	if err != nil {
		return err
	}
	if hasGrants {
		return nil
	}

	s.recordMCPAuthzDenial(r, "not_a_member")
	return newStatusError(http.StatusForbidden, "forbidden",
		"You are not a member of this workspace")
}
