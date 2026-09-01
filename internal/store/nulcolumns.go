package store

// The enforcement population for the NUL invariant (DOC-2823 S2, Layer B).
//
// ONE LIST, THREE CONSUMERS. The trigger migration is GENERATED from this list,
// the guard test compares this list against the live schema, and a reader
// looking for "which columns are protected" has exactly one place to look.
// TASK-2825 delivered the census and named this shape: the migration and the
// test must both derive from the shared list, or they drift and the drift is
// invisible.
//
// The census is on TASK-2825's trail with a file:line citation per column. What
// is reproduced here is the CLASSIFICATION, because that is what the triggers
// need; the reasoning stays there rather than being copied and going stale.

// nulColumnClass says which check a column gets.
type nulColumnClass int

const (
	// classText gets the raw-NUL check only. No JSON parser will ever read the
	// value, so an escape in it is six ordinary characters.
	classText nulColumnClass = iota

	// classJSON gets the raw-NUL check AND the decoded-escape check, because
	// Postgres's jsonb parser reads these and refuses an escape that decodes
	// to a NUL (SQLSTATE 22P05). A SQLite instance that stores one cannot
	// later migrate.
	classJSON
)

// nulColumn is one protected column.
type nulColumn struct {
	Table  string
	Column string
	Class  nulColumnClass
}

// nulColumns is the 86-column population from TASK-2825 (45 JSON / 36
// user-text / 5 derived) plus the second ring Ruling 2 admitted wholesale.
//
// DERIVED columns are classed as text: status_transitions and item_wiki_links
// carry values extracted from stored item content, which is the BUG-2814
// re-emit in miniature — they need the raw check and no JSON parser reads them.
//
// The SECOND RING is included per the lead's Ruling 2: instr triggers are
// near-free, so caller-influenced text columns go in wholesale rather than
// being litigated one at a time. Ruling 2 attached a condition — a
// header-derived value (user agent, IP) must not surface as a 500 or a broken
// login when the trigger refuses — which is why S2 also maps the trigger's
// error, and why those columns are marked in the comments below.
var nulColumns = []nulColumn{
	// items
	{"items", "fields", classJSON},
	{"items", "tags", classJSON},
	{"items", "title", classText},
	{"items", "content", classText},
	// items.slug is deliberately absent: slugify emits only [a-z0-9-], so a
	// NUL cannot survive into it.

	// collections
	{"collections", "schema", classJSON},
	{"collections", "settings", classJSON},
	{"collections", "traits", classJSON},
	{"collections", "name", classText},
	{"collections", "description", classText},
	{"collections", "icon", classText},
	{"collections", "prefix", classText},

	// workspaces
	{"workspaces", "settings", classJSON},
	{"workspaces", "name", classText},
	{"workspaces", "description", classText},

	// documents
	{"documents", "tags", classJSON},
	{"documents", "title", classText},
	{"documents", "content", classText},
	{"documents", "doc_type", classText}, // second ring

	// versions
	{"item_versions", "content", classText},
	{"item_versions", "change_summary", classText},
	{"versions", "content", classText},
	{"versions", "change_summary", classText},

	// comments
	{"comments", "body", classText},
	{"comments", "author", classText},

	// activities
	{"activities", "metadata", classJSON},
	{"activities", "actor", classText},
	{"activities", "user_agent", classText}, // second ring, header-derived
	{"activities", "ip_address", classText}, // second ring, header-derived

	// event outbox
	{"event_outbox", "payload", classJSON},

	// derived-from-stored-content columns
	{"status_transitions", "from_status", classText},
	{"status_transitions", "to_status", classText},
	{"item_wiki_links", "target_title", classText},
	{"item_wiki_links", "display_text", classText},
	{"item_wiki_links", "target_ref", classText},

	// views
	{"views", "config", classJSON},
	{"views", "name", classText},

	// agent roles
	{"agent_roles", "tools", classJSON},
	{"agent_roles", "name", classText},
	{"agent_roles", "description", classText},
	{"agent_roles", "icon", classText},

	// templates
	{"custom_templates", "content", classText},
	{"custom_templates", "name", classText},
	{"custom_templates", "description", classText},
	{"custom_templates", "doc_type", classText}, // second ring

	// webhooks
	{"webhooks", "events", classJSON},
	{"webhooks", "url", classText},
	{"webhooks", "secret", classText}, // second ring, caller-supplied

	// api tokens
	{"api_tokens", "scopes", classJSON},
	{"api_tokens", "name", classText},

	// users
	{"users", "plan_overrides", classJSON},
	{"users", "oauth_providers", classJSON},
	{"users", "name", classText},
	{"users", "email", classText},
	{"users", "username", classText},
	{"users", "avatar_url", classText},
	// users.recovery_codes is deliberately absent: newline-joined bcrypt
	// hashes of server-generated codes. It LOOKS like JSON and is not.

	// oauth
	{"oauth_clients", "redirect_uris", classJSON},
	{"oauth_clients", "grant_types", classJSON},
	{"oauth_clients", "response_types", classJSON},
	{"oauth_clients", "scopes", classJSON},
	{"oauth_clients", "name", classText},
	{"oauth_connections", "name", classText},

	// report layouts, snapshots, watches
	{"user_report_layouts", "config", classJSON},
	{"progress_snapshots", "phase_data", classJSON},
	{"watches", "predicate", classText},

	// platform + sharing + invitations
	{"platform_settings", "value", classText},
	{"share_links", "restrict_to_email", classText},
	{"workspace_invitations", "email", classText},

	// second ring, remaining
	{"attachments", "filename", classText},
	{"attachments", "mime_type", classText},
	{"sessions", "device_info", classText},
	{"sessions", "ip_address", classText}, // header-derived
	{"email_optouts", "email", classText},
	{"mcp_audit_log", "request_id", classText},
}

// oauthRequestTables share one writer and one column set, so they are expanded
// rather than written out four times — the census counted them the same way
// (19 of the 45 JSON columns come from this extension).
var oauthRequestTables = []string{
	"oauth_authorization_codes",
	"oauth_access_tokens",
	"oauth_refresh_tokens",
	"oauth_pkce_requests",
}

var oauthRequestColumns = []string{
	"request_form",
	"session_data",
	"scopes",
	"granted_scopes",
	"audience",
	"granted_audience",
}

// NULProtectedColumns returns the full population, oauth expansion included.
//
// Exported so the guard test and the migration generator consume the SAME
// value rather than two readings of the same table.
func NULProtectedColumns() []nulColumn {
	out := make([]nulColumn, 0, len(nulColumns)+len(oauthRequestTables)*len(oauthRequestColumns))
	out = append(out, nulColumns...)
	for _, t := range oauthRequestTables {
		for _, c := range oauthRequestColumns {
			out = append(out, nulColumn{t, c, classJSON})
		}
	}
	return out
}

// nulExcluded records columns the census WALKED and deliberately left out, with
// the reason.
//
// It exists so the guard test can tell "a column nobody has looked at" from "a
// column someone looked at and excluded" — without it, the test's only options
// on an unlisted column are to fail on every known exclusion or to ignore the
// class entirely.
var nulExcluded = map[string]string{
	"activities.action":                   "fixed enum, models.ValidActions",
	"event_outbox.last_error":             "Go error string, server-composed",
	"mcp_audit_log.tool_name":             "server enum, mcp_audit.go",
	"mcp_audit_log.error_kind":            "server enum, mcp_audit.go",
	"users.recovery_codes":                "newline-joined bcrypt hashes of server-generated codes; looks like JSON, is not",
	"workspace_members.collection_access": "validated enum all/selected",
	"items.slug":                          "slugify emits only [a-z0-9-]; a NUL cannot survive into it",
}
