package models

import (
	"encoding/json"
	"time"
)

// Item-level actions (existing)
var ValidActions = []string{
	"created", "updated", "archived", "restored", "moved", "read", "searched",
}

// Audit action constants for auth/admin events
const (
	ActionLogin            = "login"
	ActionLoginFailed      = "login_failed"
	ActionLogout           = "logout"
	ActionBootstrap        = "bootstrap"
	ActionRegister         = "register"
	ActionPasswordChanged  = "password_changed"
	ActionPasswordReset    = "password_reset"
	ActionTokenCreated     = "token_created"
	ActionTokenRevoked     = "token_revoked"
	ActionTokenRotated     = "token_rotated"
	ActionTOTPEnabled      = "totp_enabled"
	ActionTOTPDisabled     = "totp_disabled"
	ActionMemberInvited    = "member_invited"
	ActionMemberRemoved    = "member_removed"
	ActionRoleChanged      = "role_changed"
	ActionSettingsChanged  = "settings_changed"
	ActionOAuthLogin       = "oauth_login"
	ActionOAuthLoginFailed = "oauth_login_failed"
	ActionPlanChanged      = "plan_changed"
	// ActionPlanOverridesChanged is logged when an admin updates a
	// user's plan_overrides JSON via the admin user-detail page.
	// Surfaces per-user storage / workspace / API-token quota
	// overrides in the audit feed so operators can correlate a
	// mysteriously-allowed upload with the override that enabled it.
	ActionPlanOverridesChanged = "plan_overrides_changed"
	ActionPasswordResetByAdmin = "password_reset_by_admin"
	ActionUserDisabled         = "user_disabled"
	ActionUserEnabled          = "user_enabled"
	ActionAccountDeleted       = "account_deleted"
	// ActionEmailVerified is logged when a user confirms their email address
	// via a verification link (POST /auth/verify-email). PLAN-1933 / TASK-1936.
	ActionEmailVerified = "email_verified"
	// ActionEmailVerifiedByAdmin is logged when an admin force-verifies a
	// user's email from the admin console (DR-7). PLAN-1933 / TASK-1936.
	ActionEmailVerifiedByAdmin = "email_verified_by_admin"
	// ActionSessionIPChanged is logged when a session presents a different
	// client IP than the one recorded at creation. We don't strict-check IP
	// by default (that breaks legitimate geo shifts — VPN toggle, mobile
	// roaming) but surface the change to the audit log for detection. In
	// deployments configured with PAD_IP_CHANGE_ENFORCE=strict the middleware
	// additionally rejects the request.
	ActionSessionIPChanged = "session_ip_changed"
	// ActionSessionUAChanged is logged when a session presents a different
	// User-Agent hash than the one recorded at creation. Like the IP signal
	// this is surfaced to the audit log for detection; in deployments
	// configured with PAD_IP_CHANGE_ENFORCE=strict the middleware additionally
	// revokes the session and rejects the request. The UA hash is stable for
	// the life of a real session (a browser doesn't rewrite its own UA mid-
	// session), so a mismatch is a stronger theft signal than an IP change —
	// which is precisely why UA enforcement carries fewer false positives than
	// IP enforcement. This audit row is only emitted in strict mode; log-only
	// mode keeps the historical slog-only behavior to avoid changing the audit
	// feed for existing self-host users.
	ActionSessionUAChanged = "session_ua_changed"
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
	ItemRef        string `json:"item_ref,omitempty"` // e.g. "BUG-1748" — computed from the referenced item
	CollectionSlug string `json:"collection_slug,omitempty"`
	ActorName      string `json:"actor_name,omitempty"`
}

type ActivityListParams struct {
	Action string
	Actor  string
	Source string
	// Since, when non-zero, restricts results to activity created on or
	// after this instant (a.created_at >= Since). Applied in the SQL query
	// so LIMIT counts post-filter rows. Used by `pad project activity
	// --since` and the pad_project.activity MCP action.
	Since  time.Time
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
// It wraps one of: a comment, an activity, a version, an implementation
// note, or a decision-log entry.
//
// Notes and decisions differ from the other three kinds in where they live:
// they are elements of the item's own fields blob, not rows in a table, so
// they arrive already-loaded on the item rather than through a cursor query
// (BUG-2301). The handler still runs them through the same (created_at, id)
// cursor predicate the SQL uses, so paging behaves identically for all five —
// over a STABLE dataset. The five sources are read at five instants with no
// shared snapshot, so a write landing mid-request can put one page slightly
// out of step with another (an `updated` activity present while the note that
// caused it is not, or the reverse). That predates this type and is not
// specific to the structured kinds — the three SQL sources were already read
// one after another. Nothing is lost: the blob is authoritative and the next
// fetch is consistent.
type TimelineEntry struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // "comment", "activity", "version", "note", "decision"
	CreatedAt time.Time `json:"created_at"`
	Actor     string    `json:"actor"`
	ActorName string    `json:"actor_name,omitempty"`
	// AgentName is set on "comment" entries only, and is DERIVED: it is a
	// copy of Comment.AgentName, surfaced at entry level to match the
	// actor_name idiom (ActorName is likewise a copy of Comment.Author for
	// comment entries), so a client reads entry-level attribution the same
	// way for every kind. The nested comment's value is the authoritative
	// one — it is what the store's join wrote. Activity entries deliberately
	// do NOT get it: their name already lives in Activity.Metadata, and a
	// second copy there would be a second source that can drift (TASK-2760,
	// lead ruling on the trail).
	AgentName string                  `json:"agent_name,omitempty"`
	Source    string                  `json:"source"`
	Comment   *Comment                `json:"comment,omitempty"`
	Activity  *Activity               `json:"activity,omitempty"`
	Version   *Version                `json:"version,omitempty"`
	Note      *ItemImplementationNote `json:"note,omitempty"`
	Decision  *ItemDecisionLogEntry   `json:"decision,omitempty"`
}

// TimelineResponse is the paginated response from the timeline endpoint.
type TimelineResponse struct {
	Entries []TimelineEntry `json:"entries"`
	HasMore bool            `json:"has_more"`
}

// AgentNameFromMetadata returns the agent display name stamped on an
// activity's metadata JSON (`handlers_documents.go::agentMeta` writes the
// `agent` key from the X-Pad-Agent header), or "" when the metadata is empty,
// unparseable, or carries no non-empty string under that key. It is the Go
// twin of the web client's agentNameOf (web/src/lib/utils/agentActor.ts) and
// applies the same contract: verbatim, no normalization, and a non-string
// value counts as absent rather than being rendered.
//
// Parsing happens here, in Go, rather than in SQL: the store targets both
// SQLite and Postgres, whose JSON accessors (json_extract vs ->>) differ, and
// the comment queries currently have no dialect fork to add one to.
func AgentNameFromMetadata(metadata string) string {
	if metadata == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(metadata), &m); err != nil {
		return ""
	}
	name, _ := m["agent"].(string)
	return name
}
