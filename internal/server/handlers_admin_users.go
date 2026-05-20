package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
)

// --- Admin User Management (TASK-502) ---

// handleAdminListUsers returns a paginated list of users with plan info,
// cheap aggregations (workspace_count, storage_bytes, last_write_at,
// status), and sort/filter support. PLAN-1542 / TASK-1544.
//
// GET /api/v1/admin/users?q=search&plan=free&offset=0&limit=50
//
//	Additional query params:
//	  role=admin|member               filter by role
//	  disabled=true|false             filter disabled state (omit = both)
//	  active_within_days=N            only users with last_write_at within N days
//	  has_workspaces=true|false       filter on workspace_count > 0 (omit = both)
//	  sort=email|last_write|last_active|storage|workspaces|created
//	  order=asc|desc                  default desc
func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	q := r.URL.Query()

	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	params := store.AdminUserSearchParams{
		Query:  q.Get("q"),
		Plan:   q.Get("plan"),
		Role:   q.Get("role"),
		Limit:  limit,
		Offset: offset,
		Sort:   q.Get("sort"),
		Order:  q.Get("order"),
	}

	// Tri-state bool filters: only set the pointer when the param is
	// present, so omitting it means "no filter" (not "false"). Uses
	// strconv.ParseBool so the full set of canonical truthy/falsy values
	// is accepted ("true"/"True"/"TRUE"/"1"/"t" and the parallel falses);
	// anything else is treated as "no filter" rather than silently false
	// (Codex review on PR #599).
	if v := q.Get("disabled"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			params.Disabled = &b
		}
	}
	if v := q.Get("has_workspaces"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			params.HasWorkspaces = &b
		}
	}
	if v := q.Get("active_within_days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			params.ActiveWithinDays = &n
		}
	}

	result, err := s.store.SearchUsers(params)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	type adminUser struct {
		ID             string `json:"id"`
		Email          string `json:"email"`
		Username       string `json:"username"`
		Name           string `json:"name"`
		Role           string `json:"role"`
		Plan           string `json:"plan"`
		PlanExpiresAt  string `json:"plan_expires_at,omitempty"`
		PlanOverrides  string `json:"plan_overrides,omitempty"`
		TOTPEnabled    bool   `json:"totp_enabled"`
		DisabledAt     string `json:"disabled_at,omitempty"`
		LastActiveAt   string `json:"last_active_at,omitempty"`
		LastWriteAt    string `json:"last_write_at,omitempty"`
		CreatedAt      string `json:"created_at"`
		UpdatedAt      string `json:"updated_at"`
		WorkspaceCount int    `json:"workspace_count"`
		StorageBytes   int64  `json:"storage_bytes"`
		Status         string `json:"status"`
	}

	users := make([]adminUser, 0, len(result.Users))
	for _, u := range result.Users {
		users = append(users, adminUser{
			ID:             u.ID,
			Email:          u.Email,
			Username:       u.Username,
			Name:           u.Name,
			Role:           u.Role,
			Plan:           u.Plan,
			PlanExpiresAt:  u.PlanExpiresAt,
			PlanOverrides:  u.PlanOverrides,
			TOTPEnabled:    u.TOTPEnabled,
			DisabledAt:     u.DisabledAt,
			LastActiveAt:   u.LastActiveAt,
			LastWriteAt:    u.LastWriteAt,
			CreatedAt:      u.CreatedAt.Format("2006-01-02T15:04:05Z"),
			UpdatedAt:      u.UpdatedAt.Format("2006-01-02T15:04:05Z"),
			WorkspaceCount: u.WorkspaceCount,
			StorageBytes:   u.StorageBytes,
			Status:         u.Status,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"users": users,
		"total": result.Total,
	})
}

// handleAdminGetUser returns a single user with full detail.
// GET /api/v1/admin/users/{userID}
func (s *Server) handleAdminGetUser(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	userID := chi.URLParam(r, "userID")
	user, err := s.store.GetUser(userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if user == nil {
		writeError(w, http.StatusNotFound, "not_found", "User not found")
		return
	}

	// Get workspace count for this user
	workspaces, err := s.store.GetUserWorkspaces(user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Aggregations for the list-row merge. The list endpoint returns
	// these per-row; this single-user endpoint is hit after PATCH/POST
	// mutations to refresh a row, so it must return the same shape or
	// the merged row goes stale (Codex review on PR #603).
	storageBytes, err := s.store.UserStorageUsage(user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	wsCount := len(workspaces)
	status := store.ComputeAdminUserStatusValue(user.DisabledAt, user.LastWriteAt, wsCount)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":              user.ID,
		"email":           user.Email,
		"username":        user.Username,
		"name":            user.Name,
		"role":            user.Role,
		"plan":            user.Plan,
		"plan_expires_at": user.PlanExpiresAt,
		"plan_overrides":  user.PlanOverrides,
		"totp_enabled":    user.TOTPEnabled,
		"disabled_at":     user.DisabledAt,
		"last_active_at":  user.LastActiveAt,
		"last_write_at":   user.LastWriteAt,
		"created_at":      user.CreatedAt,
		"updated_at":      user.UpdatedAt,
		"workspace_count": wsCount,
		"storage_bytes":   storageBytes,
		"status":          status,
	})
}

// handleAdminGetUserMetrics returns the windowed engagement metrics for
// a single user — days_since_write, writes_7d, collections_touched_30d.
// Powers the metric tiles on the admin user modal's Overview tab (T1553).
//
// Intentionally a small set of cheap signals. Per-request API tracking
// is filed as a follow-up (IDEA-1556) and will surface as an additive
// api_requests_7d field once the request-log table exists.
//
// PLAN-1542 / TASK-1547. GET /api/v1/admin/users/{userID}/metrics.
func (s *Server) handleAdminGetUserMetrics(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	userID := chi.URLParam(r, "userID")
	if user, err := s.store.GetUser(userID); err != nil {
		writeInternalError(w, err)
		return
	} else if user == nil {
		writeError(w, http.StatusNotFound, "not_found", "User not found")
		return
	}

	metrics, err := s.store.GetUserMetrics(userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, metrics)
}

// handleAdminGetUserActivity returns a paginated chronological feed of
// activities originated by the user — item writes, comments, logins,
// account changes the user made themselves. Powers the Activity tab of
// the admin user modal (T1554). PLAN-1542 / TASK-1546.
//
// GET /api/v1/admin/users/{userID}/activity?limit=20&offset=0&action=...
//
//	Returns: {"events":[...], "next_offset": int|null}
//	next_offset is set when more results may exist (limit+1 lookup); null
//	when the page is the last.
func (s *Server) handleAdminGetUserActivity(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	userID := chi.URLParam(r, "userID")
	if user, err := s.store.GetUser(userID); err != nil {
		writeInternalError(w, err)
		return
	} else if user == nil {
		writeError(w, http.StatusNotFound, "not_found", "User not found")
		return
	}

	params := models.ActivityListParams{
		Action: r.URL.Query().Get("action"),
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 50 {
		limit = 50
	}
	// Ask for limit+1 so we can detect "more available" without a second
	// COUNT query. Trim the extra before responding.
	params.Limit = limit + 1
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			params.Offset = n
		}
	}

	activities, err := s.store.ListUserActivity(userID, params)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	hasMore := len(activities) > limit
	if hasMore {
		activities = activities[:limit]
	}

	resp := map[string]interface{}{
		"events": activities,
	}
	if hasMore {
		resp["next_offset"] = params.Offset + limit
	} else {
		resp["next_offset"] = nil
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAdminGetUserDetail returns a combined per-user payload: the user
// vitals plus a per-workspace breakdown enriched with aggregations
// (collections_count, items_open, items_total, members_count,
// storage_bytes, last_activity_at). Powers the Workspaces tab of the
// admin user modal (T1552) in a single round-trip.
//
// PLAN-1542 / TASK-1545. GET /api/v1/admin/users/{userID}/detail.
func (s *Server) handleAdminGetUserDetail(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	userID := chi.URLParam(r, "userID")
	user, err := s.store.GetUser(userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if user == nil {
		writeError(w, http.StatusNotFound, "not_found", "User not found")
		return
	}

	workspaces, err := s.store.GetUserWorkspacesDetailed(userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if workspaces == nil {
		// Defensive: ensure JSON serializes as `[]` rather than `null`.
		workspaces = []store.AdminUserWorkspaceDetail{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user": map[string]interface{}{
			"id":              user.ID,
			"email":           user.Email,
			"username":        user.Username,
			"name":            user.Name,
			"role":            user.Role,
			"plan":            user.Plan,
			"plan_expires_at": user.PlanExpiresAt,
			"plan_overrides":  user.PlanOverrides,
			"totp_enabled":    user.TOTPEnabled,
			"disabled_at":     user.DisabledAt,
			"last_active_at":  user.LastActiveAt,
			"last_write_at":   user.LastWriteAt,
			"created_at":      user.CreatedAt,
			"updated_at":      user.UpdatedAt,
		},
		"workspaces": workspaces,
	})
}

// handleAdminGetUserWorkspaces returns workspace memberships for a user.
// GET /api/v1/admin/users/{userID}/workspaces
func (s *Server) handleAdminGetUserWorkspaces(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	userID := chi.URLParam(r, "userID")
	memberships, err := s.store.GetUserWorkspaceMemberships(userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if memberships == nil {
		memberships = []store.AdminUserWorkspace{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"workspaces": memberships,
	})
}

// handleAdminUpdateUser updates a user's plan, overrides, or role.
// PATCH /api/v1/admin/users/{userID}
func (s *Server) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	userID := chi.URLParam(r, "userID")
	user, err := s.store.GetUser(userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if user == nil {
		writeError(w, http.StatusNotFound, "not_found", "User not found")
		return
	}

	var input struct {
		Role          *string `json:"role"`
		Plan          *string `json:"plan"`
		PlanExpiresAt *string `json:"plan_expires_at"`
		PlanOverrides *string `json:"plan_overrides"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	if input.Role != nil {
		validRoles := map[string]bool{"admin": true, "member": true}
		if !validRoles[*input.Role] {
			writeError(w, http.StatusBadRequest, "bad_request", "role must be 'admin' or 'member'")
			return
		}

		// Guard: cannot demote yourself
		caller := currentUser(r)
		if caller != nil && caller.ID == userID {
			writeError(w, http.StatusBadRequest, "bad_request", "Cannot change your own role")
			return
		}

		// SetUserRole atomically guards against demoting the last admin.
		if err := s.store.SetUserRole(userID, *input.Role); err != nil {
			if err == store.ErrLastAdmin {
				writeError(w, http.StatusBadRequest, "bad_request", "Cannot demote the last admin")
				return
			}
			writeInternalError(w, err)
			return
		}

		s.logAuditEvent(models.ActionRoleChanged, r, auditMeta(map[string]string{
			"target_user_id": userID,
			"old_role":       user.Role,
			"new_role":       *input.Role,
		}))
	}

	if input.Plan != nil {
		validPlans := map[string]bool{"free": true, "pro": true, "self-hosted": true}
		if !validPlans[*input.Plan] {
			writeError(w, http.StatusBadRequest, "bad_request", "plan must be 'free', 'pro', or 'self-hosted'")
			return
		}
		expiresAt := ""
		if input.PlanExpiresAt != nil {
			expiresAt = *input.PlanExpiresAt
		}
		if err := s.store.SetUserPlan(userID, *input.Plan, expiresAt); err != nil {
			writeInternalError(w, err)
			return
		}

		s.logAuditEvent(models.ActionPlanChanged, r, auditMeta(map[string]string{
			"target_user_id": userID,
			"old_plan":       user.Plan,
			"new_plan":       *input.Plan,
		}))
	}

	if input.PlanOverrides != nil {
		// Validate JSON
		if *input.PlanOverrides != "" {
			var overrides map[string]int
			if err := json.Unmarshal([]byte(*input.PlanOverrides), &overrides); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "plan_overrides must be valid JSON (map of feature → limit)")
				return
			}
		}
		if err := s.store.SetUserPlanOverrides(userID, *input.PlanOverrides); err != nil {
			writeInternalError(w, err)
			return
		}

		// Audit-log the change. Stored as before/after JSON strings so
		// an operator reviewing the feed can see exactly which key
		// flipped (e.g. storage_bytes 524288000 → 10737418240). The
		// per-key diff is small and the strings are bounded (one row
		// per override key); cheaper than a separate event-per-key
		// fanout.
		s.logAuditEvent(models.ActionPlanOverridesChanged, r, auditMeta(map[string]string{
			"target_user_id": userID,
			"old_overrides":  user.PlanOverrides,
			"new_overrides":  *input.PlanOverrides,
		}))
	}

	// Return updated user
	updated, err := s.store.GetUser(userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":              updated.ID,
		"email":           updated.Email,
		"username":        updated.Username,
		"name":            updated.Name,
		"role":            updated.Role,
		"plan":            updated.Plan,
		"plan_overrides":  updated.PlanOverrides,
		"plan_expires_at": updated.PlanExpiresAt,
		"totp_enabled":    updated.TOTPEnabled,
		"created_at":      updated.CreatedAt,
		"updated_at":      updated.UpdatedAt,
		"ok":              true,
	})
}

// handleAdminResetPassword force-resets a user's password.
// If email is configured, sends a reset link. Otherwise returns a temporary password.
// POST /api/v1/admin/users/{userID}/reset-password
func (s *Server) handleAdminResetPassword(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	userID := chi.URLParam(r, "userID")
	user, err := s.store.GetUser(userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if user == nil {
		writeError(w, http.StatusNotFound, "not_found", "User not found")
		return
	}

	if s.email != nil && s.baseURL != "" {
		// Email configured: generate reset token and send link
		token, err := s.store.CreatePasswordReset(user.ID)
		if err != nil {
			writeInternalError(w, err)
			return
		}

		resetURL := s.baseURL + "/reset-password/" + token
		if err := s.email.SendPasswordReset(r.Context(), user.Email, user.Name, resetURL); err != nil {
			writeError(w, http.StatusInternalServerError, "email_failed", "Failed to send password reset email")
			return
		}

		s.logAuditEvent(models.ActionPasswordResetByAdmin, r, auditMeta(map[string]string{
			"target_user_id": userID,
			"method":         "email",
		}))

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":      true,
			"method":  "email",
			"message": "Password reset email sent to " + user.Email,
		})
		return
	}

	// No email: generate a temporary password
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		writeInternalError(w, err)
		return
	}
	tempPassword := hex.EncodeToString(raw)

	pwd := tempPassword
	if _, err := s.store.UpdateUser(userID, models.UserUpdate{Password: &pwd}); err != nil {
		writeInternalError(w, err)
		return
	}

	// Invalidate all existing sessions so the user must log in with the new password
	if err := s.store.DeleteUserSessions(userID); err != nil {
		writeInternalError(w, err)
		return
	}

	s.logAuditEvent(models.ActionPasswordResetByAdmin, r, auditMeta(map[string]string{
		"target_user_id": userID,
		"method":         "temporary_password",
	}))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":            true,
		"method":        "temporary_password",
		"temp_password": tempPassword,
		"message":       "Temporary password generated. The user's existing sessions have been invalidated.",
	})
}

// handleAdminDisableUser soft-disables a user account.
// POST /api/v1/admin/users/{userID}/disable
func (s *Server) handleAdminDisableUser(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	userID := chi.URLParam(r, "userID")

	// Guard: cannot disable yourself
	caller := currentUser(r)
	if caller != nil && caller.ID == userID {
		writeError(w, http.StatusBadRequest, "bad_request", "Cannot disable your own account")
		return
	}

	user, err := s.store.GetUser(userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if user == nil {
		writeError(w, http.StatusNotFound, "not_found", "User not found")
		return
	}
	if err := s.store.DisableUser(userID); err != nil {
		writeInternalError(w, err)
		return
	}

	// Always invalidate sessions (also handles retry after partial failure)
	if err := s.store.DeleteUserSessions(userID); err != nil {
		writeInternalError(w, err)
		return
	}

	s.logAuditEvent(models.ActionUserDisabled, r, auditMeta(map[string]string{
		"target_user_id": userID,
	}))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"message": "User disabled and sessions invalidated",
	})
}

// handleAdminEnableUser re-enables a disabled user account.
// POST /api/v1/admin/users/{userID}/enable
func (s *Server) handleAdminEnableUser(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	userID := chi.URLParam(r, "userID")
	user, err := s.store.GetUser(userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if user == nil {
		writeError(w, http.StatusNotFound, "not_found", "User not found")
		return
	}
	if !user.IsDisabled() {
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "message": "User is already enabled"})
		return
	}

	if err := s.store.EnableUser(userID); err != nil {
		writeInternalError(w, err)
		return
	}

	s.logAuditEvent(models.ActionUserEnabled, r, auditMeta(map[string]string{
		"target_user_id": userID,
	}))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"message": "User re-enabled",
	})
}

// --- Admin Limits Management ---

// handleAdminGetLimits returns the current default plan limits.
// GET /api/v1/admin/limits
func (s *Server) handleAdminGetLimits(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	features := []string{
		"workspaces", "items_per_workspace", "members_per_workspace",
		"api_tokens", "storage_bytes", "webhooks", "automated_backups",
	}
	plans := []string{"free", "pro"}

	defaults := map[string]store.PlanLimits{
		"free": store.DefaultFreeLimits,
		"pro":  store.DefaultProLimits,
	}

	result := make(map[string]map[string]int)
	for _, plan := range plans {
		result[plan] = make(map[string]int)
		for _, feature := range features {
			key := "plan_limits_" + plan + "_" + feature
			val, err := s.store.GetPlatformSetting(key)
			if err != nil || val == "" {
				// Fall back to hardcoded default for this plan+feature
				result[plan][feature] = planLimitDefault(defaults[plan], feature)
				continue
			}
			v, _ := strconv.Atoi(val)
			result[plan][feature] = v
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// handleAdminUpdateLimits updates default plan limits.
// PATCH /api/v1/admin/limits
// Body: {"free": {"workspaces": 10, ...}, "pro": {"workspaces": -1, ...}}
func (s *Server) handleAdminUpdateLimits(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	var input map[string]map[string]int
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	validPlans := map[string]bool{"free": true, "pro": true}
	validFeatures := map[string]bool{
		"workspaces": true, "items_per_workspace": true, "members_per_workspace": true,
		"api_tokens": true, "storage_bytes": true, "webhooks": true, "automated_backups": true,
	}

	for plan, features := range input {
		if !validPlans[plan] {
			continue
		}
		for feature, value := range features {
			if !validFeatures[feature] {
				continue
			}
			key := "plan_limits_" + plan + "_" + feature
			if err := s.store.SetPlatformSetting(key, strconv.Itoa(value)); err != nil {
				writeInternalError(w, err)
				return
			}
		}
	}

	s.logAuditEvent(models.ActionSettingsChanged, r, auditMeta(map[string]string{"scope": "plan_limits"}))

	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// --- Admin Platform Stats ---

// handleAdminStats returns platform-level statistics.
// GET /api/v1/admin/stats
func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	userCount, err := s.store.UserCount()
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Count users by plan
	users, err := s.store.ListUsers()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	planCounts := map[string]int{}
	for _, u := range users {
		plan := u.Plan
		if plan == "" {
			plan = "free"
		}
		planCounts[plan]++
	}

	workspaces, err := s.store.ListWorkspaces()
	if err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"users":         userCount,
		"users_by_plan": planCounts,
		"workspaces":    len(workspaces),
		"cloud_mode":    s.cloudMode,
	})
}

// --- Helpers ---

func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	user := currentUser(r)
	if user == nil || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden", "Admin access required")
		return false
	}
	return true
}

// planLimitDefault returns the hardcoded default for a plan+feature pair.
func planLimitDefault(limits store.PlanLimits, feature string) int {
	switch feature {
	case "workspaces":
		return limits.Workspaces
	case "items_per_workspace":
		return limits.ItemsPerWorkspace
	case "members_per_workspace":
		return limits.MembersPerWorkspace
	case "api_tokens":
		return limits.APITokens
	case "storage_bytes":
		return limits.StorageBytes
	case "webhooks":
		return limits.Webhooks
	case "automated_backups":
		return limits.AutomatedBackups
	default:
		return 0
	}
}

// auditMeta is defined in handlers_documents.go
