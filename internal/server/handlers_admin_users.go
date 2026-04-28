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

// handleAdminListUsers returns a paginated list of users with plan info.
// GET /api/v1/admin/users?q=search&plan=free&offset=0&limit=50
func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	result, err := s.store.SearchUsers(store.AdminUserSearchParams{
		Query:  r.URL.Query().Get("q"),
		Plan:   r.URL.Query().Get("plan"),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}

	type adminUser struct {
		ID            string `json:"id"`
		Email         string `json:"email"`
		Username      string `json:"username"`
		Name          string `json:"name"`
		Role          string `json:"role"`
		Plan          string `json:"plan"`
		PlanExpiresAt string `json:"plan_expires_at,omitempty"`
		PlanOverrides string `json:"plan_overrides,omitempty"`
		TOTPEnabled   bool   `json:"totp_enabled"`
		DisabledAt    string `json:"disabled_at,omitempty"`
		LastActiveAt  string `json:"last_active_at,omitempty"`
		CreatedAt     string `json:"created_at"`
		UpdatedAt     string `json:"updated_at"`
	}

	users := make([]adminUser, 0, len(result.Users))
	for _, u := range result.Users {
		users = append(users, adminUser{
			ID:            u.ID,
			Email:         u.Email,
			Username:      u.Username,
			Name:          u.Name,
			Role:          u.Role,
			Plan:          u.Plan,
			PlanExpiresAt: u.PlanExpiresAt,
			PlanOverrides: u.PlanOverrides,
			TOTPEnabled:   u.TOTPEnabled,
			DisabledAt:    u.DisabledAt,
			LastActiveAt:  u.LastActiveAt,
			CreatedAt:     u.CreatedAt.Format("2006-01-02T15:04:05Z"),
			UpdatedAt:     u.UpdatedAt.Format("2006-01-02T15:04:05Z"),
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
		"created_at":      user.CreatedAt,
		"updated_at":      user.UpdatedAt,
		"workspace_count": len(workspaces),
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
