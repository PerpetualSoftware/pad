package store

import (
	"fmt"
	"log/slog"
)

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
	// tools is FREE TEXT, not JSON — migration 019 says so in as many words
	// ("free-text notes about preferred tools/models", e.g. "Claude Code +
	// Sonnet 4.6"). TASK-2825's census classed it J and this list inherited
	// that; codex round 1 caught it. Classing it JSON would refuse a user's
	// note that happens to be valid JSON carrying an escape.
	{"agent_roles", "tools", classText},
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

	// CALLER-SUPPLIED SLUGS (codex round 1).
	//
	// items.slug is excluded because it is DERIVED — ItemCreate has no Slug
	// field, so slugify's [a-z0-9-] output is the only thing that reaches it.
	// That reasoning does NOT transfer to these: CreateWorkspace and
	// CreateCollection both start with `slug := input.Slug` and only fall back
	// to slugify when the caller supplied none. The census's exclusion note was
	// right about items and was read as covering slugs generally.
	{"workspaces", "slug", classText},
	{"collections", "slug", classText},
	{"views", "slug", classText},
	{"agent_roles", "slug", classText},

	// Other caller-influenced columns the census did not reach.
	{"comment_reactions", "emoji", classText},
	{"oauth_clients", "logo_url", classText},

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

// Only session_data is JSON. The other five are NOT, and the census's
// "shared-writer extension" classed all six alike (codex round 1).
//
// Measured in internal/oauth/storage.go: RequestForm is
// `req.GetRequestForm().Encode()` — url-encoded form data; Scopes,
// GrantedScopes, Audience and GrantedAudience are `strings.Join(..., " ")`.
// Only SessionData is `string(sessionBytes)` from a marshal.
//
// oauth_clients.scopes and friends ARE json, via jsonStringList — same word,
// different tables, which is how the extension went wrong.
var oauthRequestJSONColumns = []string{
	"session_data",
}

var oauthRequestTextColumns = []string{
	"request_form",
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
	out := make([]nulColumn, 0, len(nulColumns)+len(oauthRequestTables)*(len(oauthRequestJSONColumns)+len(oauthRequestTextColumns)))
	out = append(out, nulColumns...)
	for _, t := range oauthRequestTables {
		for _, c := range oauthRequestJSONColumns {
			out = append(out, nulColumn{t, c, classJSON})
		}
		for _, c := range oauthRequestTextColumns {
			out = append(out, nulColumn{t, c, classText})
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
	"items.slug":                          "DERIVED: ItemCreate has no Slug field, so slugify's [a-z0-9-] output is the only thing that reaches it. NOTE this reasoning is specific to items — workspaces, collections, views and agent_roles all accept a caller-supplied slug and ARE protected.",
	"item_yjs_updates.update_data":        "BINARY (BLOB/BYTEA), the only such column in either schema. Raw Yjs updates legitimately contain NUL bytes; Layer A exempts it for the same reason and TestBinaryColumnCensus pins that. Surfaced here when the census's type filter was widened to include BLOB affinity, which is correct — the decision to exclude it is a judgement, not an oversight.",
}

// ensureNULTriggers re-applies the Layer B trigger migration if any of its
// triggers are missing.
//
// It runs after every migration pass, and exists because a table rebuild drops
// the table's triggers while migration 084 stays recorded as applied — so
// without this, the first rebuild after S2 would remove protection from that
// table forever, with nothing to see.
//
// It re-runs the migration FILE rather than regenerating SQL here, so there is
// still exactly one definition of what the triggers are. Every statement is
// CREATE TRIGGER IF NOT EXISTS, so re-running it is a no-op when nothing is
// missing — which is the common case, checked with one query first.
func (s *Store) ensureNULTriggers() error {
	if s.dialect.Driver() != DriverSQLite {
		return nil
	}

	var have int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'pad_nul_%'`,
	).Scan(&have); err != nil {
		return fmt.Errorf("count NUL triggers: %w", err)
	}
	want := len(NULProtectedColumns()) * 2 // one BEFORE INSERT + one BEFORE UPDATE each
	if have == want {
		return nil
	}

	data, err := migrationsFS.ReadFile("migrations/" + nulTriggerMigration)
	if err != nil {
		return fmt.Errorf("read %s: %w", nulTriggerMigration, err)
	}
	if err := execMulti(s.db, string(data)); err != nil {
		return fmt.Errorf("restore NUL triggers: %w", err)
	}
	slog.Warn("NUL invariant triggers were missing and have been restored — a table rebuild most likely "+
		"dropped them; the rows written while they were absent are NOT checked",
		"had", have, "want", want)
	return nil
}

// nulTriggerMigration is the generated file, named once so the generator, the
// re-assertion and the pin test all refer to the same artifact.
const nulTriggerMigration = "084_nul_invariant_triggers.sql"
