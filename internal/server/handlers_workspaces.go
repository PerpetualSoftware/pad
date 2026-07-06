package server

import (
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
)

func normalizeWorkspaceInput(input *models.WorkspaceCreate) error {
	if input == nil {
		return nil
	}

	settings, err := models.NormalizeWorkspaceSettings(input.Settings)
	if err != nil {
		return fmt.Errorf("invalid settings JSON: %w", err)
	}
	if input.Context != nil {
		settings, err = models.ApplyWorkspaceContext(settings, input.Context)
		if err != nil {
			return fmt.Errorf("invalid workspace context: %w", err)
		}
	}
	input.Settings = settings
	return nil
}

func normalizeWorkspaceUpdateInput(input *models.WorkspaceUpdate) error {
	if input == nil {
		return nil
	}

	if input.Settings != nil {
		settings, err := models.NormalizeWorkspaceSettings(*input.Settings)
		if err != nil {
			return fmt.Errorf("invalid settings JSON: %w", err)
		}
		input.Settings = &settings
	}

	if input.Context != nil {
		base := "{}"
		if input.Settings != nil {
			base = *input.Settings
		}
		settings, err := models.ApplyWorkspaceContext(base, input.Context)
		if err != nil {
			return fmt.Errorf("invalid workspace context: %w", err)
		}
		input.Settings = &settings
	}

	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{"status": "ok"}
	if s.version != "" {
		resp["version"] = s.version
	}
	if s.commit != "" {
		resp["commit"] = s.commit
	}
	if s.buildTime != "" {
		resp["build_time"] = s.buildTime
	}
	resp["cloud_mode"] = s.cloudMode
	writeJSON(w, http.StatusOK, resp)
}

// handleHealthLive is a lightweight liveness probe — always returns 200 if the
// process is running. Kubernetes uses this to decide whether to restart the pod.
func (s *Server) handleHealthLive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleHealthReady is a readiness probe — returns 200 only when the service
// can accept traffic (DB connection healthy). Kubernetes uses this to decide
// whether to route traffic to the pod.
func (s *Server) handleHealthReady(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not ready",
			"error":  "database unavailable",
		})
		return
	}

	resp := map[string]interface{}{
		"status": "ready",
	}

	// Include connection pool stats (useful for debugging, not required for pass/fail).
	dbStats := s.store.DB().Stats()
	resp["db"] = map[string]interface{}{
		"open_connections": dbStats.OpenConnections,
		"in_use":           dbStats.InUse,
		"idle":             dbStats.Idle,
		"driver":           string(s.store.D().Driver()),
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	type templateInfo struct {
		Name        string   `json:"name"`
		Category    string   `json:"category"`
		Description string   `json:"description"`
		Icon        string   `json:"icon"`
		Collections []string `json:"collections"`
	}
	templates := collections.ListTemplates()
	result := make([]templateInfo, 0, len(templates))
	for _, t := range templates {
		colls := make([]string, 0, len(t.Collections))
		for _, c := range t.Collections {
			colls = append(colls, c.Icon+" "+c.Name)
		}
		result = append(result, templateInfo{
			Name:        t.Name,
			Category:    t.Category,
			Description: t.Description,
			Icon:        t.Icon,
			Collections: colls,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)

	// Authenticated users — including admins — see only workspaces they're
	// a member of (which includes ones they own, since owners get a
	// workspace_members row at creation time). Server admins previously got
	// the unfiltered list here, which leaked workspace metadata into their
	// "shared with me" switcher even though they weren't members
	// (BUG-982). Cross-tenant visibility for admins is available through
	// the admin panel routes (/api/v1/admin/...), which call
	// ListWorkspaces() directly with the appropriate auth gate.
	if user != nil {
		workspaces, err := s.store.GetUserWorkspaces(user.ID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if workspaces == nil {
			workspaces = []models.Workspace{}
		}
		writeJSON(w, http.StatusOK, workspaces)
		return
	}

	// Pre-auth / fresh-install bootstrap: list everything so the setup
	// flow can find any seeded workspace.
	workspaces, err := s.store.ListWorkspaces()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if workspaces == nil {
		workspaces = []models.Workspace{}
	}
	writeJSON(w, http.StatusOK, workspaces)
}

func (s *Server) handleReorderWorkspaces(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	var input []struct {
		Slug      string `json:"slug"`
		SortOrder int    `json:"sort_order"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	for _, item := range input {
		ws, err := s.store.GetWorkspaceBySlug(item.Slug)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if ws == nil {
			continue
		}
		// Skip silently if the user is not a member of this workspace.
		// UpdateWorkspaceSortOrder is scoped to the caller's own
		// workspace_members row (WHERE user_id = ? AND workspace_id = ?),
		// so a non-member's PATCH touches zero rows and returns
		// sql.ErrNoRows — there's no cross-workspace write or leak here
		// even for an attacker-supplied slug list. (The old comment cited
		// "admin sees all workspaces"; that rationale is stale —
		// handleListWorkspaces returns membership-only for all
		// authenticated users including admins since BUG-982, so the
		// switcher never surfaces non-member workspaces to reorder.
		// BUG-1618 confirmed this site needs no auth gate.)
		if err := s.store.UpdateWorkspaceSortOrder(userID, ws.ID, item.SortOrder); err != nil {
			if err == sql.ErrNoRows {
				continue
			}
			writeInternalError(w, err)
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var input models.WorkspaceCreate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Name is required")
		return
	}
	if err := normalizeWorkspaceInput(&input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// Attribute the creation surface AUTHORITATIVELY from the request's auth
	// shape — never from the request body (WorkspaceCreate.Source is
	// `json:"-"` for exactly this reason). CLI and Remote MCP callers
	// present a bearer token / api-token context => "cli"; cookie-session
	// web callers => "web". A cli/mcp origin is what tells the dashboard an
	// agent is already connected right after `pad init`, so a web client
	// must not be able to spoof it to suppress the connect-agent/onboarding
	// prompts (BUG-1557).
	_, input.Source = actorFromRequest(r)

	// Set owner to the authenticated user
	if userID := currentUserID(r); userID != "" {
		input.OwnerID = userID
	}

	// Enforce workspace count limit (user-scoped)
	if userID := currentUserID(r); userID != "" {
		if !s.enforceUserPlanLimit(w, userID, "workspaces") {
			return
		}
	}

	ws, err := s.store.CreateWorkspace(input)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Seed collections for the new workspace using the requested template
	if err := s.store.SeedCollectionsFromTemplate(ws.ID, input.Template); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Workspace created but failed to seed collections: "+err.Error())
		return
	}

	// Add the creator as workspace owner
	if userID := currentUserID(r); userID != "" {
		_ = s.store.AddWorkspaceMember(ws.ID, userID, "owner")
	}

	// OAuth connection auto-add (PLAN-1519 / TASK-1521 / IDEA-1517 §1):
	// when the creating call came over an OAuth-bound MCP session AND
	// that connection has `may_create_workspaces=true`, immediately
	// add the new workspace to the connection's allow-list with
	// added_by='agent-create'. The agent can then use the workspace
	// without a re-auth round-trip — the whole point of the "agent
	// creates and immediately uses" flow.
	//
	// Best-effort: any error here logs but does NOT fail the response.
	// The workspace already exists; failing the response would be
	// confusing ("create succeeded but you got an error") and the user
	// can always re-grant via the Connect-project modal. PAT auth has
	// no request_id and the no-op short-circuits at the kind check.
	s.maybeAutoAddCreatorConnection(r, ws.ID)

	writeJSON(w, http.StatusCreated, ws)
}

// maybeAutoAddCreatorConnection inserts the newly-created workspace
// into the calling OAuth connection's allow-list when the grant has
// `may_create_workspaces=true`. No-op when:
//
//   - The calling token isn't an OAuth grant (PAT, CLI session token —
//     they don't carry a request_id).
//   - The grant's connection row doesn't exist (pre-Phase-C tokens
//     fall here until backfill).
//   - The flag is off (user explicitly scoped out creation power at
//     consent time or via the connections-page mutation UI).
//
// Errors are logged at WARN, never propagated. The caller's response
// must not fail because of an auth-bookkeeping issue post-creation.
func (s *Server) maybeAutoAddCreatorConnection(r *http.Request, workspaceID string) {
	kind, requestID := MCPTokenIdentityFromContext(r.Context())
	if kind != "oauth" || requestID == "" {
		return
	}
	conn, err := s.store.GetOAuthConnection(requestID)
	if err != nil || conn == nil {
		// Includes ErrOAuthConnectionNotFound (pre-Phase-C grant) and
		// any I/O error. Silent — the workspace is already created,
		// the auto-add is a convenience the user can recover via the
		// Connect modal.
		return
	}
	if !conn.MayCreateWorkspaces {
		// User declined creation power at consent. Respect that —
		// the workspace exists but doesn't auto-join the connection;
		// the user can claim it post-hoc via the Connect modal if
		// they change their mind.
		return
	}
	if err := s.store.AddConnectionWorkspace(requestID, workspaceID, store.AddedByAgentCreate); err != nil {
		// Idempotent on the store side — re-creation through the
		// same connection (very unlikely with fresh IDs) would no-op.
		// Any error here is genuinely unexpected; log so ops sees it.
		slog.Warn("auto-add workspace to OAuth connection failed",
			"request_id", requestID,
			"workspace_id", workspaceID,
			"error", err,
		)
	}
}

func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.getWorkspace(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handleUpdateWorkspace(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	existing, ok := s.getWorkspace(w, r)
	if !ok {
		return
	}

	var input models.WorkspaceUpdate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := normalizeWorkspaceUpdateInput(&input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	ws, err := s.store.UpdateWorkspace(existing.Slug, input)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if ws == nil {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
		return
	}

	s.publishEvent(events.WorkspaceUpdated, ws.ID, "", ws.Name, "", "", "")

	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	ws, ok := s.getWorkspace(w, r)
	if !ok {
		return
	}
	err := s.store.DeleteWorkspace(ws.Slug)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deletedWorkspaceResponse is one entry in the GET /workspaces/deleted
// payload: the soft-deleted workspace plus purge-window fields the UI
// renders as "N days left" before it's permanently removed. PurgeAt and
// DaysLeft are BOTH derived from workspacePurgeRetention so restore and
// the purge sweeper (workspace_purge.go) share exactly one window — no
// drift.
type deletedWorkspaceResponse struct {
	models.Workspace
	// PurgeAt is when the retention sweeper will hard-delete the
	// workspace (deleted_at + workspacePurgeRetention).
	PurgeAt time.Time `json:"purge_at"`
	// DaysLeft is whole days remaining until PurgeAt, rounded up and
	// clamped at 0. 0 means it's eligible for purge on the next sweep.
	DaysLeft int `json:"days_left"`
}

// handleListDeletedWorkspaces lists the soft-deleted workspaces the
// caller OWNS that are still restorable — i.e. not yet past the purge
// retention window. Owner-scoped inside the store query; the cutoff is
// derived from workspacePurgeRetention so this list and the purge
// sweeper agree on exactly which workspaces are recoverable.
func (s *Server) handleListDeletedWorkspaces(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	if userID == "" {
		// No authenticated user (fresh install / pre-auth) has no owned
		// workspaces to restore — return an empty list rather than 401
		// so the switcher's "recently deleted" section just renders
		// nothing.
		writeJSON(w, http.StatusOK, []deletedWorkspaceResponse{})
		return
	}

	// Use the retention the purge sweeper actually enforces (which may be
	// overridden via SetWorkspacePurgeConfig / PAD_WORKSPACE_PURGE_RETENTION)
	// so the restore window + purge_at/days_left never drift from the
	// horizon at which the workspace is really hard-deleted.
	retention := s.effectivePurgeRetention()
	cutoff := time.Now().UTC().Add(-retention)
	workspaces, err := s.store.ListDeletedWorkspaces(userID, cutoff)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	now := time.Now().UTC()
	out := make([]deletedWorkspaceResponse, 0, len(workspaces))
	for _, ws := range workspaces {
		entry := deletedWorkspaceResponse{Workspace: ws}
		if ws.DeletedAt != nil {
			entry.PurgeAt = ws.DeletedAt.Add(retention)
			days := int(math.Ceil(entry.PurgeAt.Sub(now).Hours() / 24))
			if days < 0 {
				days = 0
			}
			entry.DaysLeft = days
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRestoreWorkspace un-soft-deletes a workspace, resurfacing it (and
// everything transitively hidden underneath) intact. Owner-only, mirroring
// the Danger-Zone gating on handleDeleteWorkspace — but it can't lean on
// RequireWorkspaceAccess/requireMinRole because those resolve only LIVE
// workspaces (`deleted_at IS NULL`), so a soft-deleted workspace 404s
// before any handler runs. Instead it resolves the soft-deleted row
// directly and checks ownership here.
//
// Status codes:
//   - 404 — no restorable soft-deleted workspace with that slug (already
//     live, unknown, or hard-purged).
//   - 403 — the workspace exists but the caller doesn't own it.
//   - 200 — restored; returns the now-live workspace.
func (s *Server) handleRestoreWorkspace(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	ws, err := s.store.GetDeletedWorkspaceBySlug(slug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if ws == nil {
		// Not soft-deleted (live), unknown, or already hard-purged —
		// nothing to restore.
		writeError(w, http.StatusNotFound, "not_found", "No restorable workspace found")
		return
	}

	// Owner-only, with NO fresh-install / UserCount==0 bypass. A
	// soft-deleted workspace existing while zero users remain is precisely
	// the account-deletion case (DeleteAccountAtomic soft-deletes the
	// owner's workspaces, then removes the owner) — a bypass there would
	// let an unauthenticated caller restore an account-deleted workspace by
	// guessing its slug and resurface data whose owner is gone. On a
	// genuine fresh install there are no soft-deleted workspaces to restore
	// anyway, so requiring the owner unconditionally loses nothing.
	userID := currentUserID(r)
	if userID == "" || ws.OwnerID != userID {
		// Not the owner. If the owner user no longer exists — the
		// account-deletion case, where DeleteAccountAtomic soft-deleted
		// this workspace and then removed its owner — NO live user could
		// ever restore it, so return the same 404 as "not restorable"
		// instead of 403. That stops a guessed slug from confirming an
		// account-deleted workspace row still exists. A genuine non-owner
		// attempt on a live-owned workspace still gets 403. (GetUser only
		// runs on this rejection path, never the owner's success path.)
		if owner, _ := s.store.GetUser(ws.OwnerID); owner == nil {
			writeError(w, http.StatusNotFound, "not_found", "No restorable workspace found")
			return
		}
		writeError(w, http.StatusForbidden, "forbidden", "Only the workspace owner can restore it")
		return
	}

	// Enforce the SAME purge horizon the deleted-list uses so restore and
	// the list agree: a workspace older than the retention window is
	// already eligible for hard-purge and is hidden from the list, so it
	// must not be restorable by slug either (the sweeper may not have run
	// yet — no attachments registry, deferred blob, or startup lag). Checked
	// after the owner gate so a non-owner can't probe window state.
	cutoff := time.Now().UTC().Add(-s.effectivePurgeRetention())
	if ws.DeletedAt == nil || !ws.DeletedAt.After(cutoff) {
		writeError(w, http.StatusNotFound, "not_found", "No restorable workspace found")
		return
	}

	if err := s.store.RestoreWorkspace(slug); err != nil {
		if err == sql.ErrNoRows {
			// Raced with a concurrent restore/purge between the lookup and
			// the update — treat as nothing-to-restore.
			writeError(w, http.StatusNotFound, "not_found", "No restorable workspace found")
			return
		}
		writeInternalError(w, err)
		return
	}

	// Re-fetch the now-live row so the response carries the fully hydrated,
	// un-deleted workspace.
	restored, err := s.store.GetWorkspaceBySlug(slug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if restored == nil {
		// Extremely unlikely (just restored), but don't lie about success.
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
		return
	}

	// Log the restore in the (now live) workspace's activity feed, mirroring
	// how item restore logs a "restored" action.
	s.logActivity(restored.ID, "", "restored", r)
	s.publishEvent(events.WorkspaceUpdated, restored.ID, "", restored.Name, "", "", "")

	writeJSON(w, http.StatusOK, restored)
}

func (s *Server) handleExportWorkspace(w http.ResponseWriter, r *http.Request) {
	// `?format=tar` switches to the tar.gz bundle that includes
	// attachment blobs (TASK-884). Default stays JSON for backward
	// compat — existing automation hitting this endpoint without a
	// query param keeps working unchanged. The CLI's
	// `pad workspace export` opts into the bundle by default.
	if strings.EqualFold(r.URL.Query().Get("format"), "tar") {
		s.handleExportWorkspaceBundle(w, r)
		return
	}

	if !requireMinRole(w, r, "owner") {
		return
	}
	ws, ok := s.getWorkspace(w, r)
	if !ok {
		return
	}
	if !s.requireUnrestrictedExportAccess(w, r, ws.ID) {
		return
	}
	export, err := s.store.ExportWorkspace(ws.Slug)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-export.json"`, ws.Slug))
	writeJSON(w, http.StatusOK, export)
}

func (s *Server) handleImportWorkspace(w http.ResponseWriter, r *http.Request) {
	// Content-Type dispatch:
	//   application/gzip / application/x-gzip / application/x-tar
	//     → tar.gz bundle path (TASK-885) — handles attachments.
	//   anything else → JSON path (legacy items-only).
	//
	// We prefer Content-Type over file-magic sniffing so a misnamed
	// upload fails fast with a clear error rather than silently going
	// through the wrong code path. The CLI's pad import command sets
	// the right header based on the file extension; web UI does the
	// same when uploading a .tar.gz.
	ct := strings.TrimSpace(r.Header.Get("Content-Type"))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "application/gzip" || ct == "application/x-gzip" || ct == "application/x-tar" {
		s.handleImportWorkspaceBundle(w, r)
		return
	}

	var data models.WorkspaceExport
	// WorkspaceExport contains all collections, items, comments, and item
	// versions for the workspace — even a modest project export blows past
	// the default 2 MiB decodeJSON cap. 64 MiB is well above any realistic
	// single-workspace backup while still far from the heap-exhaustion
	// range the default cap protects against.
	if err := decodeJSONWithLimit(r, &data, 64<<20); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid export data: "+err.Error())
		return
	}

	// Optional: override workspace name via query param
	newName := r.URL.Query().Get("name")

	// Set the authenticated user as owner so the imported workspace is
	// accessible and has correct owner_username for URL routing.
	userID := currentUserID(r)
	ws, err := s.store.ImportWorkspace(&data, newName, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "import_failed", err.Error())
		return
	}

	// Add the importer as workspace owner (mirrors handleCreateWorkspace)
	if userID != "" {
		_ = s.store.AddWorkspaceMember(ws.ID, userID, "owner")
	}

	writeJSON(w, http.StatusCreated, ws)
}
