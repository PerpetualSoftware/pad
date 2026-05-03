package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// MCP audit log read endpoints (PLAN-943 TASK-960).
//
// Two surfaces:
//
//   - GET /api/v1/connected-apps/{id}/audit  — per-connection drilldown.
//     id is the OAuth request_id (chain identifier). Owner-only:
//     enforces that a user can only see audit rows for their own
//     connections by passing user_id into the store query.
//
//   - GET /api/v1/admin/mcp-audit            — admin full-table view.
//     Backs the /console/admin/mcp-audit page. Pagination via
//     limit + offset query params.
//
// The handlers are intentionally minimal — pagination, no filtering
// beyond user/connection. Filter expressions (by tool_name, by
// result_status, by date range) are an obvious follow-up but the
// spec explicitly defers them.

// mcpAuditEntryDTO is the wire-form for one row. We don't return
// models.MCPAuditEntry directly because:
//
//   - Fields are unexported-friendly (Go field names map to JSON via
//     reflect tags), but we want stable snake_case JSON keys.
//   - Pointers (*string for nullable columns) need explicit "" /
//     omitempty handling.
//   - The token_ref field IS the OAuth request_id — surfacing it as
//     a separate field name (`connection_id`) keeps the public API
//     decoupled from the internal column name in case we ever
//     refactor the store side.
type mcpAuditEntryDTO struct {
	ID           string `json:"id"`
	Timestamp    string `json:"timestamp"`
	UserID       string `json:"user_id"`
	WorkspaceID  string `json:"workspace_id,omitempty"`
	TokenKind    string `json:"token_kind"`    // "oauth" | "pat"
	ConnectionID string `json:"connection_id"` // OAuth request_id, or PAT id
	ToolName     string `json:"tool_name"`
	ArgsHash     string `json:"args_hash,omitempty"`
	ResultStatus string `json:"result_status"`
	ErrorKind    string `json:"error_kind,omitempty"`
	LatencyMs    int    `json:"latency_ms"`
	RequestID    string `json:"request_id"`
}

func mcpAuditEntryToDTO(e models.MCPAuditEntry) mcpAuditEntryDTO {
	dto := mcpAuditEntryDTO{
		ID:           e.ID,
		Timestamp:    e.Timestamp.UTC().Format(time.RFC3339),
		UserID:       e.UserID,
		TokenKind:    string(e.TokenKind),
		ConnectionID: e.TokenRef,
		ToolName:     e.ToolName,
		ArgsHash:     e.ArgsHash,
		ResultStatus: string(e.ResultStatus),
		LatencyMs:    e.LatencyMs,
		RequestID:    e.RequestID,
	}
	if e.WorkspaceID != nil {
		dto.WorkspaceID = *e.WorkspaceID
	}
	if e.ErrorKind != nil {
		dto.ErrorKind = *e.ErrorKind
	}
	return dto
}

// parseMCPAuditPaging extracts limit + offset from the query string,
// applying sane caps. Bound limit at 200 — large enough for a "scan
// the recent audit" UX, small enough that a runaway client can't DoS
// the DB by scrolling forever.
func parseMCPAuditPaging(r *http.Request) (limit, offset int) {
	limit = 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > 200 {
		limit = 200
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if v, err := strconv.Atoi(o); err == nil && v >= 0 {
			offset = v
		}
	}
	return limit, offset
}

// handleMCPConnectionAudit returns the audit rows for one of the
// requesting user's MCP connections.
//
// id is the OAuth request_id (chain identifier — the same value the
// connected-apps page revokes by). PATs aren't currently surfaced
// here because the connected-apps page lists OAuth-only — adding
// `?token_kind=pat` later is a one-line change in the store call.
//
// Returns 404 if the user has no audit entries for that connection
// (consistent with "the connection isn't yours" — explicit owner-
// scoped 404 prevents enumeration).
func (s *Server) handleMCPConnectionAudit(w http.ResponseWriter, r *http.Request) {
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

	limit, offset := parseMCPAuditPaging(r)

	rows, err := s.store.ListMCPAuditByConnection(user.ID, models.TokenKindOAuth, id, limit, offset)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	out := make([]mcpAuditEntryDTO, 0, len(rows))
	for _, e := range rows {
		out = append(out, mcpAuditEntryToDTO(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  out,
		"limit":  limit,
		"offset": offset,
	})
}

// handleAdminMCPAudit returns the full audit log. Admin-only.
//
// Supports limit + offset paging; no other filters in v1. The admin
// UI can layer client-side filtering on top — at <= 200 rows per
// page that stays cheap.
func (s *Server) handleAdminMCPAudit(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden", "Admin access required")
		return
	}

	limit, offset := parseMCPAuditPaging(r)

	rows, err := s.store.ListAllMCPAudit(limit, offset)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	out := make([]mcpAuditEntryDTO, 0, len(rows))
	for _, e := range rows {
		out = append(out, mcpAuditEntryToDTO(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":   out,
		"limit":   limit,
		"offset":  offset,
		"dropped": s.mcpAuditDroppedSnapshot(),
	})
}
