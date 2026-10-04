package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Connected-apps endpoints (PLAN-943 TASK-954).
//
// Two endpoints:
//
//   - GET    /api/v1/connected-apps       — list active OAuth connections
//                                            (one per grant chain) for
//                                            the requesting user.
//   - DELETE /api/v1/connected-apps/{id}  — revoke one connection.
//                                            id is the OAuth request_id
//                                            chain identifier.
//
// Auth: inherits the standard /api/v1 chain (TokenAuth + SessionAuth +
// RequireAuth). Owner-scoped via the store: every method takes user.ID
// so a malicious caller can't see / revoke another user's connections.
//
// Gated at the route level on oauthAvailable (PLAN-2310 DR-5): a
// deployment that does not serve OAuth has no grants to manage, so the
// routes answer 404 there rather than empty lists (server.go wraps them
// in requireOAuthAvailable). The per-connection audit route is the
// exception: it is history, and stays available to its owner.

// connectedAppDTO is the wire-form for one connection. Distinct from
// models.OAuthConnection so the API contract stays decoupled from
// internal field names.
//
// Field rationale:
//   - id = OAuth request_id chain identifier. Public face of the
//     connection — used as the URL path param for revoke + audit
//     drilldown.
//   - allowed_workspaces is sent verbatim — nil + ["*"] both mean
//     "any workspace"; the UI handles the rendering. Sending the
//     raw shape lets the page show the actual chip list when an
//     explicit subset was granted.
//   - scope_string is the raw fosite-style scope string (e.g.
//     "pad:read pad:write"). Surfaced in the per-connection expand
//     drilldown so power users can see the exact policy.
//   - capability_tier is the coarse-bucketed tier the page renders
//     as a badge. Mapping documented in store.classifyCapabilityTier.
type connectedAppDTO struct {
	ID                string   `json:"id"`
	ClientID          string   `json:"client_id"`
	ClientName        string   `json:"client_name"`
	LogoURI           string   `json:"logo_uri,omitempty"`
	RedirectURIs      []string `json:"redirect_uris,omitempty"`
	AllowedWorkspaces []string `json:"allowed_workspaces"`
	ScopeString       string   `json:"scope_string"`
	CapabilityTier    string   `json:"capability_tier"`
	ConnectedAt       string   `json:"connected_at"`
	LastUsedAt        string   `json:"last_used_at,omitempty"`
	Calls30d          int      `json:"calls_30d"`

	// PLAN-1519 / TASK-1524 / IDEA-1517 §3: connection-level state
	// surfaced for the connections-page mutation UI. Read-only on
	// older clients; new patch handlers below let the page mutate.
	Name                    string `json:"name"`
	MayCreateWorkspaces     bool   `json:"may_create_workspaces"`
	AllCurrentWorkspaces    bool   `json:"all_current_workspaces"`
	IncludeFutureWorkspaces bool   `json:"include_future_workspaces"`
}

func connectionToDTO(c models.OAuthConnection, stats *models.MCPConnectionStats) connectedAppDTO {
	dto := connectedAppDTO{
		ID:                      c.RequestID,
		ClientID:                c.ClientID,
		ClientName:              c.ClientName,
		LogoURI:                 c.LogoURL,
		RedirectURIs:            c.RedirectURIs,
		AllowedWorkspaces:       c.AllowedWorkspaces,
		ScopeString:             c.GrantedScopes,
		CapabilityTier:          string(c.CapabilityTier),
		ConnectedAt:             c.ConnectedAt.UTC().Format(time.RFC3339),
		Calls30d:                0,
		Name:                    c.Name,
		MayCreateWorkspaces:     c.MayCreateWorkspaces,
		AllCurrentWorkspaces:    c.AllCurrentWorkspaces,
		IncludeFutureWorkspaces: c.IncludeFutureWorkspaces,
	}
	if stats != nil {
		dto.Calls30d = stats.Calls30d
		if stats.LastUsedAt != nil {
			dto.LastUsedAt = stats.LastUsedAt.UTC().Format(time.RFC3339)
		}
	}
	return dto
}

// handleListConnectedApps returns every active OAuth connection the
// requesting user has authorized, enriched with last-used + 30-day
// call counts from the MCP audit log (TASK-960).
//
// Single response shape; no pagination. A user's connection count
// is bounded by the small number of MCP clients they've authorized
// (Claude Desktop, Cursor, occasional one-offs) — paging would add
// noise without solving any real load problem.
func (s *Server) handleListConnectedApps(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}

	conns, err := s.store.ListUserOAuthConnections(user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Enrich with audit-log aggregates. One bulk query — see
	// MCPConnectionStatsForUser's doc comment for the rationale.
	stats, err := s.store.MCPConnectionStatsForUser(user.ID)
	if err != nil {
		// Soft-fail on the audit lookup: the page is more useful
		// with empty last-used columns than not at all. Log so ops
		// notice if the audit table is broken, but proceed.
		slog.Warn("connected-apps: audit stats lookup failed", "error", err, "user_id", user.ID)
		stats = nil
	}

	out := make([]connectedAppDTO, 0, len(conns))
	for _, c := range conns {
		var statPtr *models.MCPConnectionStats
		if stats != nil {
			if v, ok := stats[store.MCPConnectionStatsKey(models.TokenKindOAuth, c.RequestID)]; ok {
				statPtr = &v
			}
		}
		out = append(out, connectionToDTO(c, statPtr))
	}

	// Delegated grants given to installed apps (TASK-3399): listed on their
	// own, read-only plus revoke, never as MCP connections (no
	// oauth_connections row, lead ruling Q1). Additive member.
	grants, err := s.store.ListUserAppGrants(user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	appGrants := make([]appGrantDTO, 0, len(grants))
	for _, g := range grants {
		appGrants = append(appGrants, appGrantDTO{
			ID: g.RequestID, AppName: g.AppName, Origin: g.Origin, Access: g.Access, GrantedAt: g.GrantedAt,
			Workspace: appGrantWorkspaceDTO{Slug: g.WorkspaceSlug, Name: g.WorkspaceName},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items":      out,
		"app_grants": appGrants,
	})
}

// appGrantDTO is one delegated grant to an installed app on the Connected
// Apps page: who, where, what access, since when. It can only be revoked.
type appGrantDTO struct {
	ID        string               `json:"id"`
	AppName   string               `json:"app_name"`
	Origin    string               `json:"origin"`
	Workspace appGrantWorkspaceDTO `json:"workspace"`
	Access    string               `json:"access"`
	GrantedAt time.Time            `json:"granted_at"`
}

type appGrantWorkspaceDTO struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// handleRevokeConnectedApp invalidates every token in the OAuth
// grant chain identified by the URL id parameter, after verifying
// ownership.
//
// 404 vs 403:
//   - Unknown chain → 404 ("not_found").
//   - Chain belongs to a DIFFERENT user → also 404 — distinct from
//     403 to prevent enumeration ("can I tell from the response
//     code that THIS request_id exists, just not for me?"). The
//     store returns ErrConnectionNotFound for both cases for the
//     same reason.
//
// Idempotent: the store's Revoke* methods don't error on already-
// revoked chains, so a double-click on the Revoke button just
// returns a clean 204 the second time.
//
// Records an audit_trail entry via CreateActivity so the existing
// /api/v1/audit-log page picks up the revoke. action="oauth_connection_revoked",
// actor="user", metadata carries the client_id for forensics.
func (s *Server) handleRevokeConnectedApp(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}

	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "connection id required")
		return
	}

	// A delegated grant to an installed app (TASK-3399) is revoked through
	// its binding: the tokens deactivated, an unused code and PKCE row
	// deleted, and the binding deleted, which introspection reads.
	if isGrant, err := s.store.IsUserAppGrant(user.ID, id); err != nil {
		writeInternalError(w, err)
		return
	} else if isGrant {
		installID, err := s.store.RevokeUserAppGrant(user.ID, id)
		if errors.Is(err, store.ErrAppGrantNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Connection not found.")
			return
		}
		if err != nil {
			writeInternalError(w, err)
			return
		}
		metaJSON, _ := json.Marshal(map[string]string{"connection_id": id, "app_install_id": installID})
		if _, err := s.store.CreateActivity(models.Activity{
			Action: "oauth_connection_revoked", Actor: "user", Source: "web", UserID: user.ID,
			Metadata: string(metaJSON), IPAddress: clientIP(r), UserAgent: requestUserAgent(r),
		}); err != nil {
			slog.Warn("connected-apps: audit log write failed", "error", err, "user_id", user.ID, "connection_id", id)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Pre-revoke lookup so the audit trail can carry the client_id.
	// Cheap (we'd need to hit the table anyway via Revoke). Failure
	// here doesn't block the revoke — we'll just record an audit row
	// without the client metadata.
	var clientID string
	if conns, err := s.store.ListUserOAuthConnections(user.ID); err == nil {
		for _, c := range conns {
			if c.RequestID == id {
				clientID = c.ClientID
				break
			}
		}
	}

	if err := s.store.RevokeUserOAuthConnection(user.ID, id); err != nil {
		if errors.Is(err, store.ErrConnectionNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Connection not found.")
			return
		}
		writeInternalError(w, err)
		return
	}
	// TASK-3365: connections opened with this grant re-check now.
	s.invalidateUserAccess(user.ID)

	// Best-effort audit-trail entry. Failure here doesn't undo the
	// revoke — the OAuth token chain is already inactive.
	meta := map[string]string{"connection_id": id}
	if clientID != "" {
		meta["client_id"] = clientID
	}
	metaJSON, _ := json.Marshal(meta)
	if _, err := s.store.CreateActivity(models.Activity{
		Action:    "oauth_connection_revoked",
		Actor:     "user",
		Source:    "web",
		UserID:    user.ID,
		Metadata:  string(metaJSON),
		IPAddress: clientIP(r),
		UserAgent: requestUserAgent(r),
	}); err != nil {
		slog.Warn("connected-apps: audit log write failed", "error", err, "user_id", user.ID, "connection_id", id)
	}

	w.WriteHeader(http.StatusNoContent)
}

// Connection mutation endpoints (PLAN-1519 / TASK-1524 / IDEA-1517 §3).
//
// All four mutations follow the same shape:
//
//   1. Resolve current user from context (RequireAuth in the chain).
//   2. Verify the connection belongs to that user — same 404
//      envelope as Revoke for non-owned chains so request_ids
//      aren't enumerable by 403-vs-404 distinction.
//   3. Apply the mutation via the dedicated store method.
//   4. Return the updated DTO so the page can re-render without a
//      separate list refresh.
//
// The mutations are intentionally per-field rather than a single
// general PATCH: the connections-page UI submits one mutation per
// user interaction (toggle a flag, edit the name, add a workspace),
// and field-scoped routes give cleaner error envelopes + audit
// trails than a sprawling general PATCH.

// connectionOwnedByUser is a helper that returns the connection if
// it belongs to userID, or (nil, ErrConnectionNotFound) otherwise.
// All mutation handlers route their ownership check through this so
// the 404 envelope stays uniform.
func (s *Server) connectionOwnedByUser(userID, requestID string) (*store.OAuthConnection, error) {
	conn, err := s.store.GetOAuthConnection(requestID)
	if err != nil {
		if errors.Is(err, store.ErrOAuthConnectionNotFound) {
			return nil, store.ErrConnectionNotFound
		}
		return nil, err
	}
	if conn.UserID != userID {
		return nil, store.ErrConnectionNotFound
	}
	return conn, nil
}

// requireConnectionOwner is the per-mutation prologue. Returns the
// (currentUser, requestID, ok) triple. On `ok=false` the response
// has already been written.
func (s *Server) requireConnectionOwner(w http.ResponseWriter, r *http.Request) (*models.User, string, bool) {
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return nil, "", false
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "connection id required")
		return nil, "", false
	}
	if _, err := s.connectionOwnedByUser(user.ID, id); err != nil {
		if errors.Is(err, store.ErrConnectionNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Connection not found.")
			return nil, "", false
		}
		writeInternalError(w, err)
		return nil, "", false
	}
	return user, id, true
}

// respondWithConnection re-fetches + writes the connection DTO post-
// mutation so the page can re-render without a separate list refresh.
// Stats lookup is best-effort — failure logs but doesn't fail the
// response, matching handleListConnectedApps' posture.
func (s *Server) respondWithConnection(w http.ResponseWriter, userID, requestID string) {
	conns, err := s.store.ListUserOAuthConnections(userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	for _, c := range conns {
		if c.RequestID != requestID {
			continue
		}
		stats, _ := s.store.MCPConnectionStatsForUser(userID)
		var statPtr *models.MCPConnectionStats
		if stats != nil {
			if v, ok := stats[store.MCPConnectionStatsKey(models.TokenKindOAuth, requestID)]; ok {
				statPtr = &v
			}
		}
		writeJSON(w, http.StatusOK, connectionToDTO(c, statPtr))
		return
	}
	// ListUserOAuthConnections filters to active chains; an inactive
	// chain's mutations remain valid but won't appear in the list
	// (and tests that seed an oauth_connections row directly without
	// also seeding token rows fall here too). Re-fetch directly from
	// oauth_connections + the access projection so the post-mutation
	// DTO carries the right scope-flag + allow-list shape regardless.
	conn, err := s.store.GetOAuthConnection(requestID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// Always read the staged slugs from the join table — even when
	// wildcard is on, the UI needs to render pre-staged rows so the
	// user can remove a mistake before flipping wildcard off. Codex
	// review #585 round 2 caught the gap where wildcard hid the
	// staged list, leaving the user no way to manage it.
	allowedSlugs, err := s.store.ListConnectionWorkspaceSlugs(requestID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, connectionToDTO(models.OAuthConnection{
		RequestID:               conn.RequestID,
		Name:                    conn.Name,
		MayCreateWorkspaces:     conn.MayCreateWorkspaces,
		AllCurrentWorkspaces:    conn.AllCurrentWorkspaces,
		IncludeFutureWorkspaces: conn.IncludeFutureWorkspaces,
		AllowedWorkspaces:       allowedSlugs,
	}, nil))
}

// handleRenameConnectedApp: PATCH /api/v1/connected-apps/{id}/name
// Body: {"name": "..."} — trimmed + capped at 120 BYTES, rune-safe
// (matches the consent-screen suggested-name cap). Empty string is
// valid (clears the name, the connections-page UI prompts again).
func (s *Server) handleRenameConnectedApp(w http.ResponseWriter, r *http.Request) {
	user, id, ok := s.requireConnectionOwner(w, r)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if len(name) > 120 {
		name = truncateBindableText(name, 120)
	}
	if err := s.store.RenameConnection(id, name); err != nil {
		writeInternalError(w, err)
		return
	}
	s.respondWithConnection(w, user.ID, id)
}

// handleUpdateConnectedAppFlags: PATCH /api/v1/connected-apps/{id}/flags
// Body: {may_create_workspaces, all_current_workspaces}. Both are written
// atomically by the store.
//
// include_future_workspaces is no longer a control (BUG-3338). The
// wildcard (all_current_workspaces) is live and covers workspaces joined
// later, and nothing ever auto-added on the flag, so the checkbox that set
// it promised something false either way. A body that still carries it
// (an older page) is accepted and the field ignored; the stored value
// follows all_current_workspaces, which is what the consent screen has
// always written. "Only my current workspaces" is the limit-to-current
// action below.
//
// Invariant: a connection with all_current_workspaces=false MUST have
// at least one workspace in oauth_connection_workspaces — otherwise
// the user has orphaned the connection (the agent can't see any
// workspace at all). Validate here and reject the flag toggle with
// a clear error rather than letting the page silently break.
func (s *Server) handleUpdateConnectedAppFlags(w http.ResponseWriter, r *http.Request) {
	user, id, ok := s.requireConnectionOwner(w, r)
	if !ok {
		return
	}
	var body struct {
		MayCreateWorkspaces  bool `json:"may_create_workspaces"`
		AllCurrentWorkspaces bool `json:"all_current_workspaces"`
		// Accepted for older pages and ignored (BUG-3338).
		IncludeFutureWorkspaces bool `json:"include_future_workspaces"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// Empty-allow-list guard (IDEA-1517 §3): switching all_current off
	// with no workspace in the list would leave the connection reaching
	// nothing. The store checks it under the connection's lock, so a
	// concurrent removal cannot empty the list between the check and the
	// write (BUG-3338). Rows staged while the wildcard is on count, which
	// is how a user moves from wildcard to a list (codex review #585).
	if err := s.store.SetScopeFlagsGuarded(id, body.MayCreateWorkspaces, body.AllCurrentWorkspaces, body.AllCurrentWorkspaces); err != nil {
		if errors.Is(err, store.ErrLastConnectionWorkspace) {
			writeError(w, http.StatusBadRequest, "empty_allowlist",
				"Cannot switch to 'specific workspaces' with no workspaces selected. Add at least one workspace first.")
			return
		}
		writeInternalError(w, err)
		return
	}
	// Turning the wildcard off narrows the connection; live connections
	// re-check now, as a removal does (TASK-3365).
	s.invalidateUserAccess(user.ID)
	s.respondWithConnection(w, user.ID, id)
}

// handleLimitConnectedAppToCurrent: POST /api/v1/connected-apps/{id}/limit-to-current
// Turns a wildcard connection into a specific list of the workspaces the
// user is a member of now (BUG-3338): the one-shot, explicit form of
// "only my current workspaces". Workspaces joined later are not covered
// until the user adds them. Rows already staged in the list are kept.
func (s *Server) handleLimitConnectedAppToCurrent(w http.ResponseWriter, r *http.Request) {
	user, id, ok := s.requireConnectionOwner(w, r)
	if !ok {
		return
	}
	if _, err := s.store.LimitConnectionToCurrentWorkspaces(id, user.ID); err != nil {
		switch {
		case errors.Is(err, store.ErrConnectionNoWorkspaces):
			writeError(w, http.StatusBadRequest, "empty_allowlist",
				"You are not a member of any workspace, so there is nothing to limit this app to.")
		case errors.Is(err, store.ErrOAuthConnectionNotFound):
			writeError(w, http.StatusNotFound, "not_found", "Connection not found.")
		default:
			writeInternalError(w, err)
		}
		return
	}
	s.invalidateUserAccess(user.ID)
	s.respondWithConnection(w, user.ID, id)
}

// handleAddConnectedAppWorkspace: POST /api/v1/connected-apps/{id}/workspaces
// Body: {"workspace": "slug-or-id"}. Resolves to a workspace_id,
// verifies the requesting user is a member (defense in depth — they
// shouldn't be able to add workspaces they don't belong to), inserts
// with added_by='user'.
func (s *Server) handleAddConnectedAppWorkspace(w http.ResponseWriter, r *http.Request) {
	user, id, ok := s.requireConnectionOwner(w, r)
	if !ok {
		return
	}
	var body struct {
		Workspace string `json:"workspace"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	body.Workspace = strings.TrimSpace(body.Workspace)
	if body.Workspace == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "workspace slug or id required")
		return
	}
	ws, err := s.store.GetWorkspaceBySlug(body.Workspace)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if ws == nil {
		writeWorkspaceNotFound(w, "Workspace not found.")
		return
	}
	// Membership check: the user can only grant their own connection
	// access to workspaces they themselves can access.
	member, err := s.store.GetWorkspaceMember(ws.ID, user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if member == nil {
		writeWorkspaceNotFound(w, "Workspace not found.")
		return
	}
	if err := s.store.AddConnectionWorkspace(id, ws.ID, store.AddedByUser); err != nil {
		writeInternalError(w, err)
		return
	}
	s.respondWithConnection(w, user.ID, id)
}

// handleRemoveConnectedAppWorkspace: DELETE /api/v1/connected-apps/{id}/workspaces/{slug}
// Slug-path version (URL-friendlier than a body-payload DELETE).
//
// Empty-allow-list guard: if removing this workspace would leave a
// connection with all_current_workspaces=false AND zero join rows,
// the connection becomes useless to the agent. Same invariant the
// flags handler enforces — the page should prevent this at the
// UI level but the API enforces too.
func (s *Server) handleRemoveConnectedAppWorkspace(w http.ResponseWriter, r *http.Request) {
	user, id, ok := s.requireConnectionOwner(w, r)
	if !ok {
		return
	}
	slug := strings.TrimSpace(chi.URLParam(r, "slug"))
	if slug == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "workspace slug required")
		return
	}
	ws, err := s.store.GetWorkspaceBySlug(slug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if ws == nil {
		// Slug not resolvable — treat as a no-op delete (idempotent)
		// rather than 404. The user's intent was "this slug shouldn't
		// be in my allow-list," and if it's not resolvable it isn't
		// there anyway.
		s.respondWithConnection(w, user.ID, id)
		return
	}

	// The last workspace of a specific list cannot be removed: that would
	// leave the connection reaching nothing. Checked and deleted under the
	// connection's lock, so a concurrent limit or flags change cannot slip
	// between the two (BUG-3338).
	if err := s.store.RemoveConnectionWorkspaceUnlessLast(id, ws.ID); err != nil {
		if errors.Is(err, store.ErrLastConnectionWorkspace) {
			writeError(w, http.StatusBadRequest, "empty_allowlist",
				"Removing the last workspace would orphan this connection. Switch to 'All my workspaces' or revoke the connection instead.")
			return
		}
		writeInternalError(w, err)
		return
	}
	// TASK-3365: a connection narrowed off this workspace re-checks now.
	s.invalidateUserAccess(user.ID)
	s.respondWithConnection(w, user.ID, id)
}
