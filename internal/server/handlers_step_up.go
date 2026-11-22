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
	"github.com/PerpetualSoftware/pad/internal/secret"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// sessionHasStepUp reports whether this user's authenticated session has a live grant.
func (s *Server) sessionHasStepUp(r *http.Request, userID string) bool {
	session := s.requestSessionInfo(r)
	return session != nil && session.User != nil && session.User.ID == userID && session.StepUpUntil != nil && session.StepUpUntil.After(time.Now())
}

// keyMintStepUpWindow is how recently the session must have been signed in
// to create, rotate or widen an API key without a step-up grant. It is the
// account-delete window, so one "sign in again within 10 minutes" rule holds.
const keyMintStepUpWindow = accountDeleteReauthWindow

// requireKeyMintStepUp gates every door that hands out or widens a key
// secret (TASK-4693): the session has a live step-up grant, or its own
// stored sign-in time is within keyMintStepUpWindow. A session with no
// stored sign-in time (a row from before the column) is never fresh. The
// rule holds for accounts without a verification factor too: they sign in
// again. Callers run requireInteractiveSession first.
func (s *Server) requireKeyMintStepUp(w http.ResponseWriter, r *http.Request, userID string) bool {
	if s.sessionHasStepUp(r, userID) {
		return true
	}
	session := s.requestSessionInfo(r)
	if session != nil && session.User != nil && session.User.ID == userID && session.SignInStamp != nil {
		age := time.Since(*session.SignInStamp)
		if age >= 0 && age <= keyMintStepUpWindow {
			return true
		}
	}
	writeError(w, http.StatusForbidden, "step_up_required",
		"Sign in again, or verify your identity, then retry within 10 minutes")
	return false
}

// requireTOTPEnrolStepUp gates both creating and confirming a pending secret.
// Accounts without a verification factor must instead have a stored sign-in
// time within accountDeleteReauthWindow. A session with no stored sign-in
// time (a row from before the column) is never fresh: its created_at does
// not stand in for a sign-in.
func (s *Server) requireTOTPEnrolStepUp(w http.ResponseWriter, r *http.Request, user *models.User) bool {
	if s.sessionHasStepUp(r, user.ID) {
		return true
	}
	session := s.requestSessionInfo(r)
	if session != nil && session.User != nil && session.User.ID == user.ID {
		factor, _ := s.stepUpFactor(user, session)
		if factor == "unavailable" {
			if session.SignInStamp != nil {
				age := time.Since(*session.SignInStamp)
				if age >= 0 && age <= accountDeleteReauthWindow {
					return true
				}
			}
			writeError(w, http.StatusForbidden, "reauth_required", "Sign in again, then turn on two-factor authentication within 10 minutes")
			return false
		}
	}
	writeError(w, http.StatusForbidden, "step_up_required", "Verify your identity before turning on two-factor authentication")
	return false
}

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

// stepUpUnavailableReason says why stepUpFactor returned "unavailable", and
// is "" for any other factor. With TOTP on, stepUpFactor is unavailable only
// when the session signed in before enrolment, so signing in again fixes it;
// otherwise the account has no factor at all.
func stepUpUnavailableReason(user *models.User, factor string) string {
	switch {
	case factor != "unavailable":
		return ""
	case user.TOTPEnabled:
		return "sign_in_again"
	default:
		return "no_factor"
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
		Factor            string     `json:"factor"`
		ActiveUntil       *time.Time `json:"active_until"`
		UnavailableReason string     `json:"unavailable_reason,omitempty"`
	}{factor, active, stepUpUnavailableReason(session.User, factor)})
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
		msg := "No verification method is available for this account"
		if stepUpUnavailableReason(user, factor) == "sign_in_again" {
			msg = "This session started before two-factor authentication was turned on. Sign in again, then retry within 10 minutes"
		}
		writeError(w, http.StatusConflict, "step_up_unavailable", msg)
		return
	}
	var input struct {
		Code         secret.Secret `json:"code"`
		RecoveryCode secret.Secret `json:"recovery_code"`
		Password     secret.Secret `json:"password"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	failed := func() { s.logAuditEvent(models.ActionStepUpFailed, r, auditMeta(map[string]string{"factor": factor})) }
	switch factor {
	case "totp":
		if input.Code.IsZero() && input.RecoveryCode.IsZero() {
			writeError(w, http.StatusBadRequest, "validation_error", "code or recovery_code is required")
			return
		}
		verified, answered := s.checkSecondFactor(w, user, "user:"+user.ID, input.Code.Reveal(), input.RecoveryCode.Reveal())
		if !verified {
			failed()
			if !answered {
				writeError(w, http.StatusForbidden, "invalid_code", "Invalid verification code")
			}
			return
		}
	case "email":
		if input.Code.IsZero() {
			writeError(w, http.StatusBadRequest, "validation_error", "code is required")
			return
		}
		verified, err := s.store.VerifyStepUpCode(user.ID, session.ID, input.Code.Reveal())
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
		if input.Password.IsZero() {
			writeError(w, http.StatusBadRequest, "validation_error", "password is required")
			return
		}
		if s.rateLimiters != nil && s.rateLimiters.RecoveryCode != nil && !s.rateLimiters.RecoveryCode.allow("pw:user:"+user.ID) {
			failed()
			writeRateLimitResponse(w, s.rateLimiters.RecoveryCode.config)
			return
		}
		verified, err := s.store.ValidatePassword(user.Email, input.Password.Reveal())
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
