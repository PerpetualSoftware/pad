package server

import (
	"errors"
	"log/slog"
	"math/rand"
	"net/http"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
)

// distantFuture is used as a "prune everything" cutoff for the
// item_yjs_updates table. PruneYjsUpdatesBefore takes a strict-less-
// than cutoff, so passing a far-future time sweeps the whole row set.
var distantFuture = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// collabMembershipRevalInterval is how often an active collab WS
// re-runs authorizeCollabAccess to catch a mid-stream revocation
// (member removed, role demoted, item-grant revoked, etc.). 60s
// matches the SSE membership-revalidation cadence and trades
// "promptness of revocation visibility" against "per-conn DB
// load". Exposed as a package var so tests can shrink it without
// waiting a real minute.
var collabMembershipRevalInterval = 60 * time.Second

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
	if s.collab == nil {
		// RoomManager wiring is optional — a self-host build that
		// doesn't enable collab still exposes the route but should
		// fail loud rather than silently accepting the upgrade and
		// dropping every byte. 503 mirrors the SSE handler's
		// "events bus not configured" path.
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"Collaboration is not available on this server")
		return
	}

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

	// Periodic auth revalidation: catch member-removed /
	// role-demoted / grant-revoked mid-stream and force-close the
	// WS. Mirrors handlers_events.go's sseSubscriberStillHasAccess
	// pattern but routed through the room manager so the close
	// frame goes out under writeMu (no concurrent-write panics
	// against the room's writeLoop / replay path).
	revalDone := make(chan struct{})
	defer close(revalDone)
	go s.collabRevalidationLoop(r, item, conn, itemID, userID, revalDone)

	// Hand the connection to the RoomManager. It owns the
	// op-log replay, fan-out, and lifecycle bookkeeping (lazy create
	// + grace-TTL reclaim). Returns when the WS closes for any reason.
	if err := s.collab.Join(itemID, conn); err != nil {
		// Normal closure paths surface here as websocket.CloseError
		// values that aren't worth logging. Anything unexpected
		// (transport failure, room manager hard error) gets a warn.
		if websocket.IsUnexpectedCloseError(err,
			websocket.CloseNormalClosure,
			websocket.CloseGoingAway,
			websocket.CloseNoStatusReceived,
		) {
			slog.Warn("collab: websocket session ended unexpectedly",
				"item_id", itemID,
				"user_id", userID,
				"error", err,
			)
		}
	}
}

// collabRevalidationLoop ticks every collabMembershipRevalInterval
// while the WebSocket is open and re-runs authorizeCollabAccess. On
// access loss it sends a close frame with ClosePolicyViolation +
// "Your access to this item was revoked." and closes the conn,
// which propagates through the room manager's read loop and tears
// the session down cleanly.
//
// First fire is jittered across [0, interval) so a fleet of clients
// that all reconnected after a deploy don't synchronise their reval
// ticks and storm the auth path together.
//
// Stops when stop is closed (handler returning), so a finished
// session doesn't leak the goroutine + a long-lived ticker.
func (s *Server) collabRevalidationLoop(
	r *http.Request,
	item *models.Item,
	conn *websocket.Conn,
	itemID string,
	userID string,
	stop <-chan struct{},
) {
	interval := collabMembershipRevalInterval

	// First-fire jitter: rand.Int63n is fine for spread purposes —
	// the security argument doesn't depend on unpredictability.
	first := time.Duration(rand.Int63n(int64(interval)))
	timer := time.NewTimer(first)
	defer timer.Stop()

	for {
		select {
		case <-stop:
			return
		case <-timer.C:
			// Re-fetch the item every tick so a mid-session move
			// (item collection changed to one the user can't see)
			// or hard-delete is honoured as an access change. The
			// snapshot captured at upgrade time isn't enough.
			fresh, ferr := s.store.GetItem(itemID)
			if ferr != nil {
				slog.Warn("collab: revalidation GetItem failed; keeping connection open",
					"item_id", itemID,
					"user_id", userID,
					"error", ferr,
				)
				timer.Reset(interval)
				continue
			}
			if fresh == nil {
				slog.Info("collab: item disappeared mid-stream, closing connection",
					"item_id", itemID,
					"user_id", userID,
				)
				s.collab.CloseConn(
					itemID, conn,
					websocket.ClosePolicyViolation,
					"This item is no longer available.",
				)
				return
			}

			err := s.authorizeCollabAccess(r, fresh)
			switch {
			case err == nil:
				// Still authorised. Re-arm at the regular cadence;
				// the connect-time jitter has already spread the
				// fleet so subsequent fires can be evenly spaced.
				timer.Reset(interval)

			case isAccessDenial(err):
				// Real revocation — close the conn.
				slog.Info("collab: access revoked mid-stream, closing connection",
					"item_id", itemID,
					"user_id", userID,
				)
				s.collab.CloseConn(
					itemID, conn,
					websocket.ClosePolicyViolation,
					"Your access to this item was revoked.",
				)
				return

			default:
				// Transient internal error (DB blip on GetUser /
				// grant lookup, etc.). Logging at warn so an
				// operator notices a sustained pattern, but we
				// MUST NOT close the conn — a single failed
				// query shouldn't punt every active editor.
				// Re-arm and try again on the next tick.
				slog.Warn("collab: revalidation error; keeping connection open",
					"item_id", itemID,
					"user_id", userID,
					"error", err,
				)
				timer.Reset(interval)
			}
		}
	}
}

// isAccessDenial reports whether the given error from
// authorizeCollabAccess represents a real authorization decision
// (member removed, role demoted, item-grant revoked) versus an
// internal / transient error (DB blip on a lookup). Only access
// denials should close the live WebSocket; transient errors must
// fall through so a single failed query doesn't punt every active
// editor in the workspace.
//
// authorizeCollabAccess returns *statusError for every "we
// know they don't have access" branch, and a plain error (without
// the statusError wrap) for store / internal errors. errors.As is
// the canonical way to discriminate.
func isAccessDenial(err error) bool {
	var sErr *statusError
	return errors.As(err, &sErr)
}

// applyContentViaCollab routes an external content update through a
// connected browser tab via the collab room manager's
// designated-applier protocol. Returns nil when an applier acked
// successfully (caller should suppress the direct items.content
// write); any error means "fall back to direct write".
//
// Errors are categorised + logged at the right level so operators
// can see when degraded paths fire. Returning the error rather than
// silently swallowing lets callers add their own telemetry / metrics
// later without re-deriving the categorisation.
//
// Retries internally when the no-room classification races a fresh
// Join (PruneAndApply returns ErrRoomActiveDuringPrune): the room
// is now active, so we re-call ApplyExternalContent against the
// live peer. Capped at applyContentMaxRetries to prevent runaway
// loops if joins keep landing during the prune attempts.
const applyContentMaxRetries = 3

// directWriteFn is the caller's items.content writer. Invoked only
// on the no-room/no-applier paths, INSIDE the per-item setup lock
// (so a fresh Join cannot replay stale op-log between prune and
// content write).
type directWriteFn func() error

func (s *Server) applyContentViaCollab(r *http.Request, itemID, markdown string, directWrite directWriteFn) error {
	if s.collab == nil {
		return errors.New("collab not configured")
	}

	for attempt := 0; attempt < applyContentMaxRetries; attempt++ {
		err := s.applyContentViaCollabOnce(r, itemID, markdown, directWrite)
		if !errors.Is(err, collab.ErrRoomActiveDuringPrune) {
			return err
		}
		// A fresh Join slipped in during PruneAndApply's check.
		// Loop and re-try ApplyExternalContent against the now-
		// active room rather than direct-writing past the live
		// peer (whose Y.Doc would otherwise outvote the direct
		// write on next flush). Per Codex review round 6.
	}
	slog.Warn("collab: exhausted prune retries; falling back to direct write",
		"item_id", itemID,
	)
	return collab.ErrRoomActiveDuringPrune
}

func (s *Server) applyContentViaCollabOnce(r *http.Request, itemID, markdown string, directWrite directWriteFn) error {
	err := s.collab.ApplyExternalContent(itemID, markdown)
	switch {
	case err == nil:
		// Caller suppresses the direct write.
		return nil

	case errors.Is(err, collab.ErrNoActiveRoom),
		errors.Is(err, collab.ErrNoApplierAvailable):
		// No live editors — direct write is the right thing. We
		// also prune the op-log here: any prior collab state is
		// strictly older than the items.content the caller is
		// about to write, and replaying it on the next collab
		// session would resurrect stale content and silently
		// overwrite this update on the next 5s flush. Common
		// triggers for this path are (a) CLI/MCP/API updates
		// outside any co-edit session, (b) raw-mode toggles that
		// destroy the in-tab provider before saving, and (c) raw
		// saves that hit the server while the room is still in
		// its 60s grace TTL with zero conns (returns
		// ErrNoApplierAvailable).
		//
		// PruneAndApply runs the prune under the per-item setup
		// lock so a fresh Join racing in this exact window can't
		// load the soon-to-be-pruned op-log under our feet.
		// Pruning is safe in both no-conn cases — there are no
		// peers in memory whose Y.Doc would diverge.
		// ErrAllAppliersTimedOut is intentionally NOT pruned
		// because peers may still be alive there.
		paErr := s.collab.PruneAndApply(itemID, func() error {
			// Prune the (now-stale) op-log. Failure here is logged
			// but does NOT block the content write — the prune
			// matters for FUTURE collab sessions, while the
			// content write is the user's actual intent.
			if _, perr := s.store.PruneYjsUpdatesBefore(itemID, distantFuture); perr != nil {
				slog.Warn("collab: failed to prune op-log on direct-write fallback",
					"item_id", itemID,
					"error", perr,
				)
			}
			// Write items.content under the same per-item lock so
			// a concurrent Join can't slip in between prune and
			// write, replay an empty op-log, then later overwrite
			// our fresh write from its stale Y.Doc state. Per
			// Codex review round 8.
			return directWrite()
		})
		switch {
		case paErr == nil:
			// Prune + direct write completed atomically.
			// Caller suppresses any subsequent items.content write.
			return nil
		case errors.Is(paErr, collab.ErrRoomActiveDuringPrune):
			// A peer joined between ApplyExternalContent's check
			// and PruneAndApply's re-check. Surface the error so
			// the outer retry loop in applyContentViaCollab
			// re-routes through the now-active applier — direct-
			// writing past a live peer would let its Y.Doc
			// outvote our update on the next flush. Per Codex
			// review round 6.
			return paErr
		default:
			slog.Warn("collab: failed to prune op-log on direct-write fallback",
				"item_id", itemID,
				"error", paErr,
			)
			return err
		}

	case errors.Is(err, collab.ErrAllAppliersTimedOut):
		slog.Warn("collab: all designated appliers timed out; falling back to direct items.content write",
			"item_id", itemID,
			"actor", actorIDFromRequest(r),
		)
		return err

	default:
		slog.Warn("collab: applier path failed; falling back to direct items.content write",
			"item_id", itemID,
			"error", err,
		)
		return err
	}
}

// actorIDFromRequest returns a non-empty identity string for the
// caller when one is available — user id, token id, or empty. Used
// in slog calls where we want SOMETHING actor-shaped without
// caring about the precise auth path.
func actorIDFromRequest(r *http.Request) string {
	if u := currentUser(r); u != nil {
		return u.ID
	}
	if tw := tokenWorkspaceID(r); tw != "" {
		return "token-ws:" + tw
	}
	return ""
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

	// Workspace-level gate: any access at all? Membership OR guest grants.
	// Without this, a logged-in user with no relationship to this
	// workspace would silently fall into the item-visibility check below
	// and 404, which would leak whether the item exists. Reject with
	// 403 first so non-members see the same shape they always have.
	member, err := s.store.GetWorkspaceMember(wsID, fresh.ID)
	if err != nil {
		return err
	}
	hasWorkspaceLevelAccess := member != nil
	if !hasWorkspaceLevelAccess {
		hasGrants, err := s.store.UserHasGrantsInWorkspace(wsID, fresh.ID)
		if err != nil {
			return err
		}
		hasWorkspaceLevelAccess = hasGrants
	}
	if !hasWorkspaceLevelAccess {
		s.recordMCPAuthzDenial(r, "not_a_member")
		return newStatusError(http.StatusForbidden, "forbidden",
			"You are not a member of this workspace")
	}

	// Item-level visibility check. Mirrors requireItemVisible +
	// guestResourceFilter without depending on middleware-set request
	// context — the WS path doesn't go through RequireWorkspaceAccess.
	//
	// Two-stage check:
	//   1. Coarse: the item's collection must be in the user's
	//      visible set. VisibleCollectionIDs returns nil for "all"
	//      access; that's the easy grant. A non-nil slice may include
	//      collections "anchored" by an item-level grant (so the
	//      collection appears in the nav even though the user only
	//      has access to a single item in it) — that's NOT enough to
	//      grant collab access to other items in the same collection.
	//   2. Strict (only when the user has item-level grants): require
	//      one of (a) full collection grant, (b) member's "specific"
	//      access list including this collection, (c) item grant on
	//      THIS exact item. Otherwise the visible-IDs hit was
	//      anchored by a sibling's grant and we must 404.
	//
	// Without the strict stage, a guest with `item:A` grant could
	// upgrade /api/v1/collab/{B} for a sibling B in the same
	// collection — the bug Codex found in round 3.
	visibleIDs, err := s.store.VisibleCollectionIDs(wsID, fresh.ID)
	if err != nil {
		return err
	}
	if visibleIDs == nil {
		return nil // "all" access
	}
	collectionIsVisible := false
	for _, id := range visibleIDs {
		if id == item.CollectionID {
			collectionIsVisible = true
			break
		}
	}
	if !collectionIsVisible {
		return newStatusError(http.StatusNotFound, "not_found", "Item not found")
	}

	// Visible-set hit. If the user has NO item-level grants, the
	// visibility came from full collection access (member's "specific"
	// access list, or a full collection grant) — grant access to any
	// item in the collection.
	collGrants, itemGrants, err := s.store.ListUserGrants(wsID, fresh.ID)
	if err != nil {
		return err
	}
	if len(itemGrants) == 0 {
		return nil
	}

	// User has item grants. The visible-set hit may have been anchored
	// by a sibling item's grant, so we need a strict check.

	// (a) Full collection grant on this collection.
	for _, g := range collGrants {
		if g.CollectionID == item.CollectionID {
			return nil
		}
	}

	// (b) Member's "specific" access list including this collection.
	// guestResourceFilter only consults this branch for non-guests.
	if member != nil {
		memberColls, err := s.store.GetMemberCollectionAccess(wsID, fresh.ID)
		if err != nil {
			return err
		}
		for _, id := range memberColls {
			if id == item.CollectionID {
				return nil
			}
		}
	}

	// (c) Item-level grant on this exact item.
	for _, g := range itemGrants {
		if g.ItemID == item.ID {
			return nil
		}
	}

	// Visibility was anchored by a sibling grant — 404, mirroring
	// requireItemVisible's "don't leak existence" pattern.
	return newStatusError(http.StatusNotFound, "not_found", "Item not found")
}
