package server

import (
	"context"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/xarmian/pad/internal/models"
)

const (
	sessionCookie = "pad_session"
	webSessionTTL = 7 * 24 * time.Hour  // 7 days for web sessions
	cliSessionTTL = 30 * 24 * time.Hour // 30 days for CLI tokens

	authMethodPassword    = "password"
	authMethodCloud       = "cloud"
	setupMethodLocalCLI   = "local_cli"
	setupMethodDockerExec = "docker_exec"
	setupMethodCloud      = "cloud"
)

var emailRegexp = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

func sessionUserPayload(user *models.User) map[string]interface{} {
	if user == nil {
		return nil
	}
	return map[string]interface{}{
		"id":    user.ID,
		"email": user.Email,
		"name":  user.Name,
		"role":  user.Role,
	}
}

func setupStatePayload(setupMethod string) map[string]interface{} {
	return map[string]interface{}{
		"authenticated":  false,
		"setup_required": true,
		"setup_method":   setupMethod,
		"auth_method":    authMethodPassword,
	}
}

func sessionStatePayload(authenticated bool, user *models.User) map[string]interface{} {
	payload := map[string]interface{}{
		"authenticated":  authenticated,
		"setup_required": false,
		"auth_method":    authMethodPassword,
	}
	if authenticated {
		payload["user"] = sessionUserPayload(user)
	}
	return payload
}

func requestIsLoopback(r *http.Request) bool {
	host := r.RemoteAddr
	if parsedHost, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = parsedHost
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func (s *Server) createAuthSession(w http.ResponseWriter, user *models.User, ttl time.Duration) (string, error) {
	token, err := s.store.CreateSession(user.ID, "web", ttl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create session")
		return "", err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	// Set CSRF cookie alongside the session cookie
	setCSRFCookie(w, int(ttl.Seconds()))

	return token, nil
}

// handleBootstrap creates the first admin account for a fresh instance.
// It is only allowed from loopback-local requests so setup must happen
// on the server host or from inside the container.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if !requestIsLoopback(r) {
		writeError(w, http.StatusForbidden, "forbidden", "Bootstrap is only allowed from localhost on the server host")
		return
	}

	var input struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	input.Email = strings.TrimSpace(input.Email)
	input.Name = strings.TrimSpace(input.Name)

	if input.Email == "" || !emailRegexp.MatchString(input.Email) {
		writeError(w, http.StatusBadRequest, "validation_error", "Valid email is required")
		return
	}
	if input.Name == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "Name is required")
		return
	}
	if len(input.Password) < 8 {
		writeError(w, http.StatusBadRequest, "validation_error", "Password must be at least 8 characters")
		return
	}

	count, err := s.store.UserCount()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check user count")
		return
	}
	if count > 0 {
		writeError(w, http.StatusConflict, "conflict", "This Pad instance has already been initialized")
		return
	}

	existing, err := s.store.GetUserByEmail(input.Email)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check email")
		return
	}
	if existing != nil {
		writeError(w, http.StatusConflict, "conflict", "A user with this email already exists")
		return
	}

	user, err := s.store.CreateUser(models.UserCreate{
		Email:    input.Email,
		Name:     input.Name,
		Password: input.Password,
		Role:     "admin",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create user")
		return
	}

	token, err := s.createAuthSession(w, user, cliSessionTTL)
	if err != nil {
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"user":  sessionUserPayload(user),
		"token": token,
	})
}

// handleRegister creates a new user account.
// Registration is restricted to admins or users with a valid invitation code
// so invitees can create an account via the /join/[code] flow.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email          string `json:"email"`
		Name           string `json:"name"`
		Password       string `json:"password"`
		InvitationCode string `json:"invitation_code"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	// Validate
	input.Email = strings.TrimSpace(input.Email)
	input.Name = strings.TrimSpace(input.Name)
	input.InvitationCode = strings.TrimSpace(input.InvitationCode)

	if input.Email == "" || !emailRegexp.MatchString(input.Email) {
		writeError(w, http.StatusBadRequest, "validation_error", "Valid email is required")
		return
	}
	if input.Name == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "Name is required")
		return
	}
	if len(input.Password) < 8 {
		writeError(w, http.StatusBadRequest, "validation_error", "Password must be at least 8 characters")
		return
	}

	count, err := s.store.UserCount()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check user count")
		return
	}

	// Validate invitation code if provided (look it up before the auth gate
	// so we can give a clear error for invalid codes).
	var invitation *models.WorkspaceInvitation
	if input.InvitationCode != "" {
		inv, err := s.store.GetInvitationByCode(input.InvitationCode)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to validate invitation")
			return
		}
		if inv == nil {
			writeError(w, http.StatusBadRequest, "invalid_invitation", "Invalid or expired invitation code")
			return
		}
		invitation = inv
	}

	if count == 0 {
		writeError(w, http.StatusForbidden, "forbidden", "This Pad instance must be initialized with pad auth setup")
		return
	}

	// When users exist, allow registration if:
	// 1. The requester is an admin, OR
	// 2. A valid invitation code was provided
	if invitation == nil {
		reqUser := currentUser(r)
		if reqUser == nil || reqUser.Role != "admin" {
			writeError(w, http.StatusForbidden, "forbidden", "Registration is restricted")
			return
		}
	}

	// Check for duplicate email
	existing, err := s.store.GetUserByEmail(input.Email)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check email")
		return
	}
	if existing != nil {
		writeError(w, http.StatusConflict, "conflict", "A user with this email already exists")
		return
	}

	// Create user
	user, err := s.store.CreateUser(models.UserCreate{
		Email:    input.Email,
		Name:     input.Name,
		Password: input.Password,
		Role:     "member",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create user")
		return
	}

	// If registering via invitation, automatically add the user to the
	// workspace and mark the invitation as accepted.
	if invitation != nil {
		_ = s.store.AddWorkspaceMember(invitation.WorkspaceID, user.ID, invitation.Role)
		_ = s.store.AcceptInvitation(invitation.ID)
	}

	token, err := s.createAuthSession(w, user, webSessionTTL)
	if err != nil {
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"user":  sessionUserPayload(user),
		"token": token,
	})
}

// handleLogin validates email/password and creates a session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// If no users exist, no login needed
	count, err := s.store.UserCount()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check user count")
		return
	}
	if count == 0 {
		writeError(w, http.StatusConflict, "setup_required", "This Pad instance must be initialized with pad auth setup")
		return
	}

	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	user, err := s.store.ValidatePassword(input.Email, input.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Authentication failed")
		return
	}
	if user == nil {
		// Slow down brute force attempts
		time.Sleep(500 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid email or password")
		return
	}

	token, err := s.createAuthSession(w, user, webSessionTTL)
	if err != nil {
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user":  sessionUserPayload(user),
		"token": token,
	})
}

// handleSessionCheck returns current auth status.
func (s *Server) handleSessionCheck(w http.ResponseWriter, r *http.Request) {
	// Check if any users exist
	count, err := s.store.UserCount()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check user count")
		return
	}

	// No users → needs setup (first-time experience)
	if count == 0 {
		writeJSON(w, http.StatusOK, setupStatePayload(setupMethodLocalCLI))
		return
	}

	// Try to resolve user from context (set by middleware)
	user := currentUser(r)
	if user != nil {
		writeJSON(w, http.StatusOK, sessionStatePayload(true, user))
		return
	}

	// Try session cookie directly (since auth endpoints are exempt from middleware)
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		user, _ := s.store.ValidateSession(cookie.Value)
		if user != nil {
			writeJSON(w, http.StatusOK, sessionStatePayload(true, user))
			return
		}
	}

	writeJSON(w, http.StatusOK, sessionStatePayload(false, nil))
}

// handleLogout destroys the session and clears the cookie.
// It handles both cookie-based sessions (web) and Bearer token sessions (CLI).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Revoke cookie-based session
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(cookie.Value)
	}

	// Revoke Bearer session token (CLI auth uses Authorization: Bearer padsess_...)
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if strings.HasPrefix(token, "padsess_") {
			_ = s.store.DeleteSession(token)
		}
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	// Clear CSRF cookie on logout
	clearCSRFCookie(w)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
	})
}

// handleGetCurrentUser returns the full profile of the authenticated user.
func (s *Server) handleGetCurrentUser(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		// Try cookie directly
		if cookie, err := r.Cookie(sessionCookie); err == nil {
			user, _ = s.store.ValidateSession(cookie.Value)
		}
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":         user.ID,
		"email":      user.Email,
		"name":       user.Name,
		"role":       user.Role,
		"avatar_url": user.AvatarURL,
		"created_at": user.CreatedAt,
		"updated_at": user.UpdatedAt,
	})
}

// handleUpdateCurrentUser updates the authenticated user's profile.
// Supports updating name and/or password. Password changes require the
// current password for verification.
func (s *Server) handleUpdateCurrentUser(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		// Try cookie directly (auth endpoints are exempt from middleware)
		if cookie, err := r.Cookie(sessionCookie); err == nil {
			user, _ = s.store.ValidateSession(cookie.Value)
		}
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	var input struct {
		Name            *string `json:"name,omitempty"`
		CurrentPassword string  `json:"current_password,omitempty"`
		NewPassword     string  `json:"new_password,omitempty"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	// Validate name if provided
	if input.Name != nil {
		trimmed := strings.TrimSpace(*input.Name)
		if trimmed == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "Name cannot be empty")
			return
		}
		input.Name = &trimmed
	}

	// Validate password change
	if input.NewPassword != "" {
		if input.CurrentPassword == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "Current password is required to set a new password")
			return
		}
		if len(input.NewPassword) < 8 {
			writeError(w, http.StatusBadRequest, "validation_error", "New password must be at least 8 characters")
			return
		}

		// Verify current password
		valid, err := s.store.ValidatePassword(user.Email, input.CurrentPassword)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to validate password")
			return
		}
		if valid == nil {
			time.Sleep(500 * time.Millisecond) // Slow down brute force
			writeError(w, http.StatusForbidden, "invalid_password", "Current password is incorrect")
			return
		}
	}

	// Build update
	update := models.UserUpdate{
		Name: input.Name,
	}
	if input.NewPassword != "" {
		update.Password = &input.NewPassword
	}

	updated, err := s.store.UpdateUser(user.ID, update)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to update profile")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":         updated.ID,
		"email":      updated.Email,
		"name":       updated.Name,
		"role":       updated.Role,
		"avatar_url": updated.AvatarURL,
		"created_at": updated.CreatedAt,
		"updated_at": updated.UpdatedAt,
	})
}

// handleForgotPassword generates a password reset token and sends it via email.
// Always returns 200 regardless of whether the email exists (prevents enumeration).
func (s *Server) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email string `json:"email"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	input.Email = strings.TrimSpace(input.Email)

	// Always return the same response to prevent email enumeration
	okResponse := map[string]interface{}{
		"ok":      true,
		"message": "If an account with that email exists, a password reset link has been sent.",
	}

	if input.Email == "" || !emailRegexp.MatchString(input.Email) {
		writeJSON(w, http.StatusOK, okResponse)
		return
	}

	user, err := s.store.GetUserByEmail(input.Email)
	if err != nil || user == nil {
		// Don't reveal whether the email exists
		writeJSON(w, http.StatusOK, okResponse)
		return
	}

	// Generate reset token
	token, err := s.store.CreatePasswordReset(user.ID)
	if err != nil {
		log.Printf("Failed to create password reset for %s: %v", input.Email, err)
		writeJSON(w, http.StatusOK, okResponse)
		return
	}

	// Send reset email
	if s.email != nil && s.baseURL != "" {
		resetURL := s.baseURL + "/reset-password/" + token
		go func() {
			if err := s.email.SendPasswordReset(context.Background(), user.Email, user.Name, resetURL); err != nil {
				log.Printf("Failed to send password reset email to %s: %v", user.Email, err)
			}
		}()
	} else {
		resetURL := s.baseURL + "/reset-password/" + token
		log.Printf("Password reset token generated for %s (email not configured). Reset URL: %s", input.Email, resetURL)
	}

	writeJSON(w, http.StatusOK, okResponse)
}

// handleResetPassword validates a reset token and sets a new password.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	if input.Token == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "Reset token is required")
		return
	}
	if len(input.Password) < 8 {
		writeError(w, http.StatusBadRequest, "validation_error", "Password must be at least 8 characters")
		return
	}

	// Atomically validate and consume the reset token (prevents race conditions)
	user, err := s.store.ConsumePasswordReset(input.Token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to validate reset token")
		return
	}
	if user == nil {
		writeError(w, http.StatusBadRequest, "invalid_token", "Invalid or expired reset link. Please request a new one.")
		return
	}

	// Update password
	password := input.Password
	update := models.UserUpdate{Password: &password}
	if _, err := s.store.UpdateUser(user.ID, update); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to update password")
		return
	}

	// Invalidate all existing sessions (force logout everywhere)
	if err := s.store.DeleteUserSessions(user.ID); err != nil {
		log.Printf("Failed to invalidate sessions for user %s after password reset: %v", user.ID, err)
	}

	// Create a fresh session so the user is logged in
	sessionToken, err := s.store.CreateSession(user.ID, "web", webSessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Password updated but failed to create session")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sessionToken,
		Path:     "/",
		MaxAge:   int(webSessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	// Set CSRF cookie alongside the new session
	setCSRFCookie(w, int(webSessionTTL.Seconds()))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
		"user": map[string]interface{}{
			"id":    user.ID,
			"email": user.Email,
			"name":  user.Name,
			"role":  user.Role,
		},
		"token": sessionToken,
	})
}
