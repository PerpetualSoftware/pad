package server

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/PerpetualSoftware/pad/internal/email"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// stepUpFactor selects the strongest factor established before the session. A sender needs no
// public base URL because the email carries a code rather than a link.
func (s *Server) stepUpFactor(user *models.User, session *store.SessionInfo) (string, *email.Sender) {
	sender := s.email
	switch {
	case user.TOTPEnabled && (user.TOTPEnabledAt == nil || session.SignedInAt.After(*user.TOTPEnabledAt)):
		return "totp", sender
	case sender != nil:
		return "email", sender
	case user.HasPassword():
		return "password", nil
	default:
		return "unavailable", nil
	}
}

func (s *Server) stepUpSession(w http.ResponseWriter, r *http.Request) *store.SessionInfo {
	session := s.requestSessionInfo(r)
	if session == nil || session.User == nil || session.User.ID != currentUserID(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return nil
	}
	return session
}

func (s *Server) handleStepUpStatus(w http.ResponseWriter, r *http.Request) {
	if !requireInteractiveSession(w, r) {
		return
	}
	session := s.stepUpSession(w, r)
	if session == nil {
		return
	}
	factor, _ := s.stepUpFactor(session.User, session)
	active := session.StepUpUntil
	if active != nil && !active.After(time.Now()) {
		active = nil
	}
	writeJSON(w, http.StatusOK, struct {
		Factor      string     `json:"factor"`
		ActiveUntil *time.Time `json:"active_until"`
	}{factor, active})
}

func (s *Server) handleStepUpEmail(w http.ResponseWriter, r *http.Request) {
	if !requireInteractiveSession(w, r) {
		return
	}
	session := s.stepUpSession(w, r)
	if session == nil {
		return
	}
	factor, sender := s.stepUpFactor(session.User, session)
	if factor != "email" {
		writeError(w, http.StatusConflict, "step_up_factor_mismatch", "Email is not the current verification factor")
		return
	}
	id, code, expires, err := s.store.CreateStepUpCode(session.User.ID, session.ID)
	if err != nil {
		var limit *store.ErrStepUpSendLimit
		if errors.As(err, &limit) {
			seconds := max(1, int(math.Ceil(limit.RetryAfter.Seconds())))
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many verification emails; try again later")
		} else {
			writeInternalError(w, err)
		}
		return
	}
	// Detached from the client: an abort mid-send must not cut a delivery
	// short, and the provider may deliver even when it reports an error.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
	defer cancel()
	if err := sender.SendStepUpCode(ctx, session.User.Email, code); err != nil {
		// Retired, not deleted: the row still counts toward the hourly send
		// cap, so repeated failures cannot refund it and loop sends.
		if retireErr := s.store.RetireStepUpCode(id); retireErr != nil {
			writeInternalError(w, retireErr)
			return
		}
		writeError(w, http.StatusServiceUnavailable, "email_send_failed", "Could not send the verification email; please try again")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Sent      bool      `json:"sent"`
		ExpiresAt time.Time `json:"expires_at"`
	}{true, expires})
}

func (s *Server) handleStepUpVerify(w http.ResponseWriter, r *http.Request) {
	if !requireInteractiveSession(w, r) {
		return
	}
	session := s.stepUpSession(w, r)
	if session == nil {
		return
	}
	user := session.User
	factor, _ := s.stepUpFactor(user, session)
	if factor == "unavailable" {
		writeError(w, http.StatusConflict, "step_up_unavailable", "Turn on two-factor authentication to change key access.")
		return
	}
	var input struct {
		Code         string `json:"code"`
		RecoveryCode string `json:"recovery_code"`
		Password     string `json:"password"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	failed := func() { s.logAuditEvent(models.ActionStepUpFailed, r, auditMeta(map[string]string{"factor": factor})) }
	switch factor {
	case "totp":
		if input.Code == "" && input.RecoveryCode == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "code or recovery_code is required")
			return
		}
		verified, answered := s.checkSecondFactor(w, user, "user:"+user.ID, input.Code, input.RecoveryCode)
		if !verified {
			failed()
			if !answered {
				writeError(w, http.StatusForbidden, "invalid_code", "Invalid verification code")
			}
			return
		}
	case "email":
		if input.Code == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "code is required")
			return
		}
		verified, err := s.store.VerifyStepUpCode(user.ID, session.ID, input.Code)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if !verified {
			failed()
			writeError(w, http.StatusForbidden, "invalid_code", "Invalid verification code")
			return
		}
	case "password":
		if input.Password == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "password is required")
			return
		}
		if s.rateLimiters != nil && s.rateLimiters.RecoveryCode != nil && !s.rateLimiters.RecoveryCode.allow("pw:user:"+user.ID) {
			failed()
			writeRateLimitResponse(w, s.rateLimiters.RecoveryCode.config)
			return
		}
		verified, err := s.store.ValidatePassword(user.Email, input.Password)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if verified == nil {
			failed()
			time.Sleep(500 * time.Millisecond)
			writeError(w, http.StatusForbidden, "invalid_password", "Incorrect password")
			return
		}
	}
	if err := s.store.GrantSessionStepUp(session.ID, time.Now().UTC().Add(10*time.Minute)); err != nil {
		writeInternalError(w, err)
		return
	}
	// Read the stored deadline: a concurrent verification may have granted it
	// first, and an already live grant must never be extended.
	updated := s.stepUpSession(w, r)
	if updated == nil {
		return
	}
	s.logAuditEvent(models.ActionStepUpVerified, r, auditMeta(map[string]string{"factor": factor}))
	writeJSON(w, http.StatusOK, struct {
		ActiveUntil *time.Time `json:"active_until"`
	}{updated.StepUpUntil})
}
