package models

import "time"

// Item-level actions (existing)
var ValidActions = []string{
	"created", "updated", "archived", "restored", "moved", "read", "searched",
}

// Audit action constants for auth/admin events
const (
	ActionLogin                = "login"
	ActionLoginFailed          = "login_failed"
	ActionLogout               = "logout"
	ActionBootstrap            = "bootstrap"
	ActionRegister             = "register"
	ActionPasswordChanged      = "password_changed"
	ActionPasswordReset        = "password_reset"
	ActionTokenCreated         = "token_created"
	ActionTokenRevoked         = "token_revoked"
	ActionTokenRotated         = "token_rotated"
	ActionTOTPEnabled          = "totp_enabled"
	ActionTOTPDisabled         = "totp_disabled"
	ActionMemberInvited        = "member_invited"
	ActionMemberRemoved        = "member_removed"
	ActionRoleChanged          = "role_changed"
	ActionSettingsChanged      = "settings_changed"
	ActionOAuthLogin           = "oauth_login"
	ActionOAuthLoginFailed     = "oauth_login_failed"
	ActionPlanChanged          = "plan_changed"
	ActionPasswordResetByAdmin = "password_reset_by_admin"
	ActionUserDisabled         = "user_disabled"
	ActionUserEnabled          = "user_enabled"
	ActionAccountDeleted       = "account_deleted"
	// ActionSessionIPChanged is logged when a session presents a different
	// client IP than the one recorded at creation. We don't strict-check IP
	// by default (that breaks legitimate geo shifts — VPN toggle, mobile
	// roaming) but surface the change to the audit log for detection. In
	// deployments configured with PAD_IP_CHANGE_ENFORCE=strict the middleware
	// additionally rejects the request.
	ActionSessionIPChanged = "session_ip_changed"
	// ActionStripeEventUnmarked is logged when /admin/stripe-event-unmark
	// rolls back a row from stripe_processed_events (TASK-736). The
	// endpoint intentionally reopens Stripe retry windows, so a persisted
	// audit trail is required — a compromised cloud_secret could otherwise
	// spam unmarks invisible to the admin /audit-log UI.
	ActionStripeEventUnmarked = "stripe_event_unmarked"
	// ActionPaymentFailedEmailSent is logged when the sidecar triggers the
	// /admin/payment-failed endpoint and pad dispatches a failed-payment
	// notification to the user. Audit trail exists so operators can prove
	// a customer was notified before a dunning-related plan change.
	ActionPaymentFailedEmailSent = "payment_failed_email_sent"
)

type Activity struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	DocumentID  string    `json:"document_id,omitempty"`
	Action      string    `json:"action"`
	Actor       string    `json:"actor"`
	Source      string    `json:"source"`
	Metadata    string    `json:"metadata,omitempty"` // JSON
	UserID      string    `json:"user_id,omitempty"`
	IPAddress   string    `json:"ip_address,omitempty"`
	UserAgent   string    `json:"user_agent,omitempty"`
	CreatedAt   time.Time `json:"created_at"`

	// Enrichment fields — populated by handlers, not stored in DB
	ItemTitle      string `json:"item_title,omitempty"`
	ItemSlug       string `json:"item_slug,omitempty"`
	CollectionSlug string `json:"collection_slug,omitempty"`
	ActorName      string `json:"actor_name,omitempty"`
}

type ActivityListParams struct {
	Action string
	Actor  string
	Source string
	Limit  int
	Offset int
}

// AuditLogParams are query parameters for the audit log endpoint.
type AuditLogParams struct {
	Action      string
	Actor       string
	WorkspaceID string
	Days        int
	Limit       int
	Offset      int
}

// TimelineEntry represents a single entry in the unified item timeline.
// It wraps one of: a comment, an activity, or a version.
type TimelineEntry struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // "comment", "activity", "version"
	CreatedAt time.Time `json:"created_at"`
	Actor     string    `json:"actor"`
	ActorName string    `json:"actor_name,omitempty"`
	Source    string    `json:"source"`
	Comment   *Comment  `json:"comment,omitempty"`
	Activity  *Activity `json:"activity,omitempty"`
	Version   *Version  `json:"version,omitempty"`
}

// TimelineResponse is the paginated response from the timeline endpoint.
type TimelineResponse struct {
	Entries []TimelineEntry `json:"entries"`
	HasMore bool            `json:"has_more"`
}
