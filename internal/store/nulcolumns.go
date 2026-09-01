package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
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
	// items.slug IS protected (codex round 2), and the reasoning that excluded
	// it was true of one write path and false of another — the lesson this
	// cluster keeps re-teaching. The API path derives the slug through slugify,
	// whose [a-z0-9-] output cannot carry a NUL. ImportWorkspace has its OWN
	// INSERT and writes the BUNDLE's slug verbatim: importCoercedSlug returns
	// it unchanged whenever it is inside the length bound, so a crafted bundle
	// puts any bytes it likes in this column.
	{"items", "slug", classText},

	// Attribution columns. The handlers let a request body's value win over the
	// server's own, so these carry caller text.
	{"items", "created_by", classText},
	{"items", "last_modified_by", classText},
	{"items", "source", classText},

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
	{"item_versions", "created_by", classText},
	{"item_versions", "source", classText},
	{"item_links", "created_by", classText},
	// link_type is normalized on the ordinary create path and written VERBATIM
	// by ImportWorkspace — the second-write-path shape again, and the third
	// column in this unit found that way (codex round 3).
	{"item_links", "link_type", classText},
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
	// view_type is `viewType := input.ViewType` with "list" only as a fallback,
	// on create AND update — caller text, not an enum the store validates
	// (codex round 3).
	{"views", "view_type", classText},

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
	{"custom_templates", "icon", classText},
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
	// These are caller-supplied, and were missed because items.slug's
	// derived-only reasoning was read as covering slugs generally.
	// CreateWorkspace and
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

	// ATTRIBUTION COLUMNS, swept as a CLASS (codex round 6, CONVE-18).
	//
	// Rounds 2, 3 and 6 each named one or two of these, which is the signal
	// that the reviewer was sampling a population rather than finding
	// instances. Enumerating every created_by / last_modified_by / source /
	// actor / author / *_by column in the schema found SIXTEEN unprotected,
	// against the eight round 6 listed.
	//
	// Some are server-set today (granted_by, invited_by, uploaded_by are user
	// ids). They go in anyway, on Ruling 2's posture for the second ring: an
	// instr trigger is near-free, and litigating each one is how the last three
	// rounds were spent.
	{"activities", "source", classText},
	{"attachments", "uploaded_by", classText},
	{"collection_grants", "granted_by", classText},
	{"comment_reactions", "actor", classText},
	{"comments", "created_by", classText},
	{"comments", "source", classText},
	{"documents", "created_by", classText},
	{"documents", "last_modified_by", classText},
	{"documents", "source", classText},
	{"item_grants", "granted_by", classText},
	{"item_workspace_moves", "created_by", classText},
	{"share_links", "created_by", classText},
	{"versions", "created_by", classText},
	{"versions", "source", classText},
	{"workspace_invitations", "invited_by", classText},
	{"workspaces", "source", classText},

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
	_, err := s.ensureNULTriggersReporting()
	return err
}

// ensureNULTriggersReporting is ensureNULTriggers, reporting whether it
// actually RESTORED anything.
//
// The bool exists for tests and is not decoration: the no-op case was asserted
// first on the trigger COUNT and then on sqlite_master ROWIDs, and BOTH are
// satisfied by a full drop-and-recreate — SQLite reuses the rowids when the
// drops and creates happen in one transaction in the same order. Two wrong
// observables in a row is the point at which the honest fix is to make the
// thing itself observable (codex round 5).
func (s *Store) ensureNULTriggersReporting() (restored bool, err error) {
	if s.dialect.Driver() != DriverSQLite {
		return false, nil
	}

	// NOTHING TO RESTORE BEFORE THE MIGRATION THAT CREATES THEM.
	//
	// This runs after every migration (codex round 4, to narrow the window in
	// which a rebuild has dropped the triggers), which means it also runs
	// during a FRESH install while the early migrations are still creating the
	// tables. Attempting the trigger SQL then fails on "no such table" — caught
	// immediately by the test suite when the per-migration call was added.
	applied, err := s.nulTriggerMigrationApplied()
	if err != nil {
		return false, err
	}
	if !applied {
		return false, nil
	}

	// name -> the exact CREATE statement the list renders, so the check can
	// compare DEFINITIONS.
	want := renderedNULTriggers()

	// The SET, not the count (codex round 2). A database with the right NUMBER
	// of triggers but a missing one and an extra one read as healthy, and
	// CREATE TRIGGER IF NOT EXISTS would then never repair the missing one.
	//
	// Matched with GLOB rather than LIKE because LIKE's `_` is a single-
	// character wildcard, so 'pad_nul_%' also matches names this code never
	// generates — a loose pattern in a health check is a health check that can
	// be satisfied by the wrong thing.
	data, err := migrationsFS.ReadFile("migrations/" + nulTriggerMigration)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", nulTriggerMigration, err)
	}

	// TRANSACTION FIRST, THEN INSPECT (codex rounds 2 and 3).
	//
	// Round 2 put the CREATE statements in a transaction, which closed the
	// window between them. Round 3 found the window BEFORE them: checking
	// whether triggers were missing outside the transaction let a raw writer
	// commit an invalid row between the check and the lock. The store's DSN
	// carries _txlock=immediate, so Begin takes the write lock up front and the
	// inspection below sees the state the restore will act on.
	//
	// The restore is still all-or-nothing: SQLite's DDL is transactional, so
	// there is never a moment where some tables are protected and others are
	// not.
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin trigger restore: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	have, err := nulTriggersIn(tx)
	if err != nil {
		return false, err
	}
	missing := false
	// An EXTRA pad_nul_ trigger is unhealthy too (codex round 6). A stray one
	// left by a partial restore or a manual edit can ABORT legitimate writes,
	// and a check that only asks "is everything I expect present" reports that
	// database as fine. The drop-then-recreate below removes them, so detecting
	// them is all that was missing.
	for name := range have {
		if _, expected := want[name]; !expected {
			missing = true
			break
		}
	}
	for name, stmt := range want {
		// DEFINITION, not just presence (codex round 3). A same-name trigger
		// with a stale or no-op body satisfies CREATE TRIGGER IF NOT EXISTS
		// forever, so a name check can never repair it.
		if have[name] != stmt {
			missing = true
			break
		}
	}
	if !missing {
		return false, nil
	}

	// Drop first, so a stale DEFINITION is actually replaced rather than
	// skipped by IF NOT EXISTS.
	for name := range have {
		if _, derr := tx.Exec("DROP TRIGGER IF EXISTS " + name); derr != nil {
			return false, fmt.Errorf("drop stale NUL trigger %s: %w", name, derr)
		}
	}
	if err := execMulti(tx, string(data)); err != nil {
		return false, fmt.Errorf("restore NUL triggers: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit trigger restore: %w", err)
	}

	// THE RESIDUAL, stated because it is real and this unit cannot close it.
	//
	// The DROP (inside a rebuild migration's own transaction) and this recreate
	// are in DIFFERENT transactions, so a concurrent raw writer — an old binary
	// on the same file, which is the population Layer B exists for — can commit
	// a violating row in between. Restoring the triggers does not remove it.
	//
	// Running after EVERY migration narrows the window to one statement's worth
	// of time rather than the whole remaining chain, which is as far as
	// enforcement can go from here. Making an existing violating row go away is
	// S3's repair sweep, and it is needed regardless: rows written before S2
	// shipped are the same problem arriving by a different route.
	slog.Warn("NUL invariant triggers were missing and have been restored — a table rebuild most likely "+
		"dropped them; rows written while they were absent are NOT retroactively checked",
		"had", len(have), "want", len(want))
	return true, nil
}

// nulTriggersIn reads the NUL triggers present, name -> stored SQL.
//
// GLOB rather than LIKE: LIKE's `_` is a single-character wildcard, so
// 'pad_nul_%' also matches names this code never generates, and a loose pattern
// in a health check is a check that can be satisfied by the wrong thing.
func nulTriggersIn(q Queryer) (map[string]string, error) {
	rows, err := q.Query(
		`SELECT name, sql FROM sqlite_master WHERE type = 'trigger' AND name GLOB 'pad_nul_*'`,
	)
	if err != nil {
		return nil, fmt.Errorf("list NUL triggers: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n string
		var sqlText sql.NullString
		if err := rows.Scan(&n, &sqlText); err != nil {
			return nil, err
		}
		out[n] = normalizeTriggerSQL(sqlText.String)
	}
	return out, rows.Err()
}

// nulTriggerMigration is the generated file, named once so the generator, the
// re-assertion and the pin test all refer to the same artifact.
const nulTriggerMigration = "084_nul_invariant_triggers.sql"

// renderedNULTriggers returns the exact CREATE statement each trigger should
// have, keyed by name.
//
// It parses the SAME rendered migration text the file is generated from, so
// there is still one definition of what a trigger is. Comparing DEFINITIONS
// rather than names is what lets the restoration replace a stale trigger — a
// same-name no-op body satisfies CREATE TRIGGER IF NOT EXISTS forever, so a
// name check can never repair one (codex round 3).
func renderedNULTriggers() map[string]string {
	out := map[string]string{}
	for _, stmt := range strings.Split(renderNULTriggerMigration(), ";\n\n") {
		stmt = strings.TrimSpace(stmt)
		i := strings.Index(stmt, "CREATE TRIGGER IF NOT EXISTS ")
		if i < 0 {
			continue
		}
		rest := stmt[i+len("CREATE TRIGGER IF NOT EXISTS "):]
		j := strings.IndexAny(rest, " \n")
		if j < 0 {
			continue
		}
		out[rest[:j]] = normalizeTriggerSQL(stmt[i:])
	}
	return out
}

// normalizeTriggerSQL makes two spellings of the same trigger comparable.
//
// SQLite stores the statement text as written, minus the trailing semicolon,
// and its whitespace survives verbatim — so the comparison collapses runs of
// whitespace rather than requiring byte equality. Collapsing is safe here
// because none of the generated triggers contain a string literal in which
// whitespace is significant beyond a single space.
func normalizeTriggerSQL(s string) string {
	// IF NOT EXISTS is dropped, because SQLite does NOT store it: the generated
	// statement says CREATE TRIGGER IF NOT EXISTS and sqlite_master holds
	// CREATE TRIGGER. Comparing the two verbatim made every trigger look
	// changed, so every startup dropped and recreated all 226 of them under an
	// immediate write lock (codex round 5).
	//
	// The no-op test did not catch it because it asserted the trigger COUNT was
	// unchanged, which is true of a full drop-and-recreate — the assertion was
	// on the wrong observable.
	out := strings.Join(strings.Fields(s), " ")
	return strings.Replace(out, "CREATE TRIGGER IF NOT EXISTS ", "CREATE TRIGGER ", 1)
}

// nulTriggerMigrationApplied reports whether the migration that creates the
// triggers has run.
//
// A QUERY ERROR IS AN ERROR (codex round 5). The first version folded it in
// with "not applied" behind a nolint:nilerr, so a migrated database with a
// transient read failure would start successfully with the invariant
// unenforced — the one outcome this whole layer exists to prevent. Only the
// schema_migrations table being ABSENT means "earlier than that migration".
func (s *Store) nulTriggerMigrationApplied() (bool, error) {
	var exists int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("check schema_migrations: %w", err)
	}
	if exists == 0 {
		return false, nil
	}
	var applied int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, nulTriggerMigration,
	).Scan(&applied); err != nil {
		return false, fmt.Errorf("check %s applied: %w", nulTriggerMigration, err)
	}
	return applied > 0, nil
}

// renderNULTriggerMigration is the single definition of the migration's text.
//
// It lives in PRODUCTION code, not the generator test, because it has three
// production-relevant readers: the generator writes it, the pin test compares
// the committed file against it byte for byte, and the startup restoration
// compares the LIVE triggers against it (codex round 3). One definition, three
// consumers — the same shape the column list has, for the same reason.
func renderNULTriggerMigration() string {
	cols := NULProtectedColumns()
	sort.Slice(cols, func(i, j int) bool {
		if cols[i].Table != cols[j].Table {
			return cols[i].Table < cols[j].Table
		}
		return cols[i].Column < cols[j].Column
	})

	var b strings.Builder
	b.WriteString(nulTriggerMigrationHeader)
	for _, c := range cols {
		for _, ev := range []struct{ suffix, on string }{
			{"ins", "INSERT"},
			{"upd", "UPDATE OF " + c.Column},
		} {
			name := fmt.Sprintf("pad_nul_%s_%s_%s", c.Table, c.Column, ev.suffix)
			cond := fmt.Sprintf("instr(NEW.%s, char(0)) > 0", c.Column)
			if c.Class == classJSON {
				cond += fmt.Sprintf(`
			OR (json_valid(NEW.%s) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.%s)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))`, c.Column, c.Column)
			}
			fmt.Fprintf(&b, `CREATE TRIGGER IF NOT EXISTS %s
BEFORE %s ON %s
FOR EACH ROW WHEN NEW.%s IS NOT NULL AND (
			%s
)
BEGIN
	SELECT RAISE(ABORT, '%s: %s.%s must not contain a NUL');
END;

`, name, ev.on, c.Table, c.Column, cond, nulTriggerMarker, c.Table, c.Column)
		}
	}
	return b.String()
}
