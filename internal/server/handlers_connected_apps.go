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
// Cloud-mode-gated at the route level: self-hosted deployments don't
// run the OAuth surface, so these endpoints would return empty lists
// + ErrConnectionNotFound 404s. Mounting outside cloud mode would be
// confusing — the routes are mounted under requireCloudMode at the
// registration site (server.go).

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
}

func connectionToDTO(c models.OAuthConnection, stats *models.MCPConnectionStats) connectedAppDTO {
	dto := connectedAppDTO{
		ID:                c.RequestID,
		ClientID:          c.ClientID,
		ClientName:        c.ClientName,
		LogoURI:           c.LogoURL,
		RedirectURIs:      c.RedirectURIs,
		AllowedWorkspaces: c.AllowedWorkspaces,
		ScopeString:       c.GrantedScopes,
		CapabilityTier:    string(c.CapabilityTier),
		ConnectedAt:       c.ConnectedAt.UTC().Format(time.RFC3339),
		Calls30d:          0,
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

	writeJSON(w, http.StatusOK, map[string]any{
		"items": out,
	})
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
		UserAgent: r.UserAgent(),
	}); err != nil {
		slog.Warn("connected-apps: audit log write failed", "error", err, "user_id", user.ID, "connection_id", id)
	}

	w.WriteHeader(http.StatusNoContent)
}
