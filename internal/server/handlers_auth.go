package server

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/xarmian/pad/internal/models"
)

const (
	sessionCookie  = "pad_session"
	webSessionTTL  = 7 * 24 * time.Hour  // 7 days for web sessions
	cliSessionTTL  = 30 * 24 * time.Hour // 30 days for CLI tokens
)

var emailRegexp = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

// handleRegister creates a new user account.
// When no users exist, anyone can register (first user becomes admin).
// When users exist, registration is restricted to admins or users with a
// valid invitation code (so that invitees can create an account via the
// /join/[code] flow).
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

	// Check if this is the first user (becomes admin)
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

	role := "member"
	if count == 0 {
		role = "admin"
	} else {
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
		Role:     role,
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

	// Create session and set cookie
	token, err := s.store.CreateSession(user.ID, "web", webSessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create session")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(webSessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"user": map[string]interface{}{
			"id":    user.ID,
			"email": user.Email,
			"name":  user.Name,
			"role":  user.Role,
		},
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
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"authenticated": false,
			"needs_setup":   true,
		})
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

	// Create session
	token, err := s.store.CreateSession(user.ID, "web", webSessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create session")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(webSessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user": map[string]interface{}{
			"id":    user.ID,
			"email": user.Email,
			"name":  user.Name,
			"role":  user.Role,
		},
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
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"authenticated": false,
			"needs_setup":   true,
		})
		return
	}

	// Try to resolve user from context (set by middleware)
	user := currentUser(r)
	if user != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"authenticated": true,
			"user": map[string]interface{}{
				"id":    user.ID,
				"email": user.Email,
				"name":  user.Name,
				"role":  user.Role,
			},
		})
		return
	}

	// Try session cookie directly (since auth endpoints are exempt from middleware)
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		user, _ := s.store.ValidateSession(cookie.Value)
		if user != nil {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"authenticated": true,
				"user": map[string]interface{}{
					"id":    user.ID,
					"email": user.Email,
					"name":  user.Name,
					"role":  user.Role,
				},
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"authenticated": false,
	})
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
