package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/notify"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Push device registration and notification settings (TASK-3938, PLAN-3908
// phase 2). The device routes are session-only: a device token is bound to the
// session that registered it and dies with it. The settings routes are
// account-level and open to any authenticated person, a PAT included.

const (
	pushTokenMaxBytes      = 4096
	pushAppVersionMaxRunes = 64
)

// pushConnectionIDPattern is what the app's connection ids look like (UUIDs).
var pushConnectionIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// SetPushGateway sets the push gateway URL (PAD_PUSH_GATEWAY_URL). Empty means
// this server offers no push. Called once at startup, before the server
// accepts connections.
func (s *Server) SetPushGateway(url string) {
	s.pushGatewayURL = url
}

// SetPushGatewayClient sets the client the push dispatcher sends through
// (TASK-3939). Called once at startup, before the server accepts connections,
// and before StartPushDispatch (cmd/pad configurePush).
func (s *Server) SetPushGatewayClient(g *notify.Gateway) {
	s.pushGateway = g
}

// pushAvailable reports whether this server can send pushes.
func (s *Server) pushAvailable() bool {
	return s.pushGatewayURL != ""
}

// requirePushSession admits only a session credential, a cookie or a padsess_
// bearer, and returns that session. Everything else is refused 403
// session_required: an API token, an OAuth bearer (also when a cookie rides
// along, which requestSessionInfo alone would fall through to), an app
// principal. No credential is a 401. A workspace-pinned API token never
// reaches here: the auth middleware's BUG-3982 gate refuses it first.
func (s *Server) requirePushSession(w http.ResponseWriter, r *http.Request) *store.SessionInfo {
	if appContextFrom(r) != nil {
		writeError(w, http.StatusForbidden, "session_required", "Push devices are registered by a signed-in session")
		return nil
	}
	switch authKind(r) {
	case authKindSessionBearer, authKindSessionCookie:
	case "":
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return nil
	default:
		writeError(w, http.StatusForbidden, "session_required", "Push devices are registered by a signed-in session, not a token")
		return nil
	}
	info := s.requestSessionInfo(r)
	if info == nil || info.ID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return nil
	}
	return info
}

func writePushUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "push_unavailable", "Push notifications are not available on this server")
}

// strictPushBody decodes a request body into a T: the transport rules every
// decodeJSON caller gets (NUL, a repeated member, trailing bytes), then a
// second pass that refuses a member T does not declare.
func strictPushBody[T any](r *http.Request) (T, error) {
	var out T
	raw, err := readBodyForDecode(r, defaultJSONBodyLimit)
	if err != nil {
		return out, err
	}
	if err := decodeJSONBytes(raw, &out); err != nil {
		return out, err
	}
	var strict T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&strict); err != nil {
		return out, err
	}
	return strict, nil
}

type pushDeviceBody struct {
	Platform string `json:"platform"`
	Token    string `json:"token"`
	// Environment is accepted for client compatibility and ignored: every
	// token is an FCM token, and FCM routes APNs itself.
	Environment json.RawMessage `json:"environment"`
	AppVersion  string          `json:"app_version"`
}

type notificationSettingsBody struct {
	PushEnabled          *bool `json:"push_enabled"`
	NotifyAssigned       *bool `json:"notify_assigned"`
	NotifyComments       *bool `json:"notify_comments"`
	NotifyReplies        *bool `json:"notify_replies"`
	NotifyReminders      *bool `json:"notify_reminders"`
	NotifyMentions       *bool `json:"notify_mentions"`
	IncludeAgentActivity *bool `json:"include_agent_activity"`
}

// handlePutPushDevice answers PUT /api/v1/me/push-devices/{connection_id}.
func (s *Server) handlePutPushDevice(w http.ResponseWriter, r *http.Request) {
	info := s.requirePushSession(w, r)
	if info == nil {
		return
	}
	if !s.pushAvailable() {
		writePushUnavailable(w)
		return
	}
	connectionID := chi.URLParam(r, "connection_id")
	if !pushConnectionIDPattern.MatchString(connectionID) {
		writeError(w, http.StatusBadRequest, "validation_error", "connection_id must be 1 to 64 letters, digits or hyphens")
		return
	}
	in, err := strictPushBody[pushDeviceBody](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}
	if in.Platform != "ios" && in.Platform != "android" {
		writeError(w, http.StatusBadRequest, "validation_error", "platform must be ios or android")
		return
	}
	if in.Token == "" || len(in.Token) > pushTokenMaxBytes || !bindableText(in.Token) {
		writeError(w, http.StatusBadRequest, "validation_error", "token must be a non-empty string of at most 4096 bytes")
		return
	}
	if utf8.RuneCountInString(in.AppVersion) > pushAppVersionMaxRunes || !bindableText(in.AppVersion) {
		writeError(w, http.StatusBadRequest, "validation_error", "app_version must be at most 64 characters")
		return
	}
	dev, err := s.store.RegisterPushDevice(info.User.ID, info.ID, connectionID, in.Platform, in.Token, in.AppVersion)
	if errors.Is(err, store.ErrPushSessionGone) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Your session has ended. Sign in again.")
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":            dev.ID,
		"connection_id": dev.ConnectionID,
		"platform":      dev.Platform,
		"app_version":   dev.AppVersion,
		"updated_at":    dev.UpdatedAt,
	})
}

// handleDeletePushDevice answers DELETE /api/v1/me/push-devices/{connection_id}.
// Idempotent: a device that is not registered is a success too.
func (s *Server) handleDeletePushDevice(w http.ResponseWriter, r *http.Request) {
	info := s.requirePushSession(w, r)
	if info == nil {
		return
	}
	if !s.pushAvailable() {
		writePushUnavailable(w)
		return
	}
	connectionID := chi.URLParam(r, "connection_id")
	if !pushConnectionIDPattern.MatchString(connectionID) {
		writeError(w, http.StatusBadRequest, "validation_error", "connection_id must be 1 to 64 letters, digits or hyphens")
		return
	}
	if err := s.store.DeletePushDevice(info.User.ID, info.ID, connectionID); err != nil {
		writeInternalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// notificationSettingsUser admits the caller for the account-level settings
// routes: any authenticated person, a PAT included. An app principal is
// refused. A PAT pinned to one workspace never reaches here: these routes are
// not in pinnedTokenExtraRoute, so the auth middleware's BUG-3982 gate refuses
// it first, and that gate is the one owner of the refusal.
func (s *Server) notificationSettingsUser(w http.ResponseWriter, r *http.Request) *models.User {
	if appContextFrom(r) != nil {
		writeError(w, http.StatusForbidden, "forbidden", "Notification settings belong to a person, not an app")
		return nil
	}
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return nil
	}
	if user.IsApp() {
		writeError(w, http.StatusForbidden, "forbidden", "Notification settings belong to a person, not an app")
		return nil
	}
	return user
}

// handleGetNotificationSettings answers GET /api/v1/me/notification-settings:
// the caller's settings, or the defaults when they never saved any.
func (s *Server) handleGetNotificationSettings(w http.ResponseWriter, r *http.Request) {
	user := s.notificationSettingsUser(w, r)
	if user == nil {
		return
	}
	settings, err := s.store.GetNotificationSettings(user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

// handlePutNotificationSettings answers PUT /api/v1/me/notification-settings:
// a full replace. All seven booleans are required, and an unknown member is
// refused, so a client that is behind or ahead of the server fails loudly
// rather than silently resetting a choice.
func (s *Server) handlePutNotificationSettings(w http.ResponseWriter, r *http.Request) {
	user := s.notificationSettingsUser(w, r)
	if user == nil {
		return
	}
	in, err := strictPushBody[notificationSettingsBody](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}
	if in.PushEnabled == nil || in.NotifyAssigned == nil || in.NotifyComments == nil || in.NotifyReplies == nil ||
		in.NotifyReminders == nil || in.NotifyMentions == nil || in.IncludeAgentActivity == nil {
		writeError(w, http.StatusBadRequest, "validation_error",
			"all seven settings are required: push_enabled, notify_assigned, notify_comments, notify_replies, notify_reminders, notify_mentions, include_agent_activity")
		return
	}
	settings := models.NotificationSettings{
		PushEnabled:          *in.PushEnabled,
		NotifyAssigned:       *in.NotifyAssigned,
		NotifyComments:       *in.NotifyComments,
		NotifyReplies:        *in.NotifyReplies,
		NotifyReminders:      *in.NotifyReminders,
		NotifyMentions:       *in.NotifyMentions,
		IncludeAgentActivity: *in.IncludeAgentActivity,
	}
	if err := s.store.PutNotificationSettings(user.ID, settings); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}
