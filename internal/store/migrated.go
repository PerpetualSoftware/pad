package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Marking a SQLite database as migrated to PostgreSQL (BUG-3072).
//
// WHAT THIS IS FOR. `pad db migrate-to-pg` is the only door in the codebase
// that ABANDONS its source database: afterwards the operator points the server
// at PostgreSQL and the SQLite file stops being read. BUG-3032 made the
// migration refuse while a server answers at the CONFIGURED address, which is
// a proxy for the real question — it cannot see a second server on another
// port, or any other caller of the exported Store.AppendYjsUpdate.
//
// THE MEASUREMENT THIS RESTS ON (BUG-3072's trail, day 66). Holding a write
// transaction across the migration does NOT make a concurrent append fail. The
// store's DSN carries busy_timeout(30000) and that timeout belongs to the
// APPENDER's connection, not ours, so the lock DEFERS the append rather than
// rejecting it: measured at 3.04s-then-err=nil for a lock released after 3s.
// The deferred write then lands in the file that is about to be abandoned. The
// guarantee would have been weakest in the fast case, which is most cases.
//
// SO THE REFUSAL IS STRUCTURAL, NOT TEMPORAL. After the import succeeds, and
// inside the same transaction, the migration installs BEFORE INSERT / UPDATE /
// DELETE triggers that RAISE(ABORT) on the tables carrying user state, and
// writes a marker row that makes store.New refuse to open the file at all. The
// COMMIT publishes both atomically and releases the write lock — so the append
// that waited out its busy_timeout executes against a database that refuses
// it, on any handle, in any process, on any port, from any binary.
//
// THAT THE DEFERRED APPEND SEES THE NEW TRIGGER IS MEASURED, NOT ASSUMED
// (day 67). The append is a pooled autocommit Exec (yjs_updates.go), already
// prepared and blocked mid-step when the DDL commits, so the mechanism is
// SQLite's SQLITE_SCHEMA auto-reprepare. Measured: 3.050524463s then
// `constraint failed: <marker> (1811)` — SQLITE_CONSTRAINT_TRIGGER — against a
// control leg on the same harness that returns err=nil at 3.042738533s when no
// trigger is installed. Both legs are in migrated_deferred_append_test.go;
// without the control the green proves nothing about the trigger.
//
// WHERE THE REFUSAL SURFACES. `internal/collab/room.go` is the only non-test
// appender, and on an append error it logs and CONTINUES broadcasting so the
// live mesh stays consistent. So the honest claim is that the edit is REFUSED
// AND LOGGED — not that the user is told. Telling the tab is a collab protocol
// change and is deliberately not part of this.
//
// SQLITE ONLY. PostgreSQL is the migration's DESTINATION; there is no shape in
// which a Postgres database is the abandoned side of this command.

// migratedMarkerTable holds one row, written by the migration inside its own
// transaction.
//
// ONE ARTIFACT, NOT TWO. `PRAGMA user_version` is unused in this codebase and
// would serve as the flag, but the remedy TEXT has to be stored somewhere
// regardless — and a flag in one place with its message in another is two
// artifacts that must agree, which is the drift surface nulcolumns.go's
// "ONE LIST, THREE CONSUMERS" header exists to warn about. A table carries
// both, is readable before migrate() runs, and is visible to an operator
// running `sqlite3 pad.db .tables` — which a pragma is not.
const migratedMarkerTable = "pad_migrated_marker"

// migratedTriggerPrefix namespaces the refusal triggers.
//
// DELIBERATELY NOT `pad_nul_`. nulcolumns.go's ensureNULTriggersReporting
// enumerates sqlite_master with GLOB 'pad_nul_*' and drop-recreates what it
// finds, and treats an unexpected `pad_nul_` trigger as an unhealthy extra. A
// separate prefix keeps these two sets outside each other's population in both
// directions — verified against that GLOB, not assumed.
const migratedTriggerPrefix = "pad_migrated_"

// MigratedMarker is the string every refusal carries, so an operator who greps
// a log for it finds the trigger that produced it and vice versa. Same role as
// nulTriggerMarker.
const MigratedMarker = "PAD_MIGRATED_TO_POSTGRES"

// migratedRefusalTables is the population the refusal triggers cover.
//
// ONE LIST, TWO CONSUMERS: the renderer below and the guard test that compares
// it against the live schema. The INSTRUMENT that generated it is stated so a
// reviewer can attack the instrument rather than the rows (CONVE-35): the
// live schema's table list read from sqlite_master on a fresh store.New — 68
// tables — partitioned by whether models.WorkspaceExport carries the table's
// contents. The live schema rather than the migration text, because the latter
// also contains `*_new` rebuild temporaries that are not tables in the end
// state.
//
// The four groups deliberately left OUT, with reasons, so a reader can tell a
// table nobody considered from one that was considered and excluded:
//
//   - DERIVED-FROM-CONTENT (status_transitions, item_wiki_links,
//     item_relation_links, activities, event_outbox): written as a consequence
//     of a protected write, which aborts first. A trigger here would only
//     produce a second error for one act.
//   - FTS SHADOW (items_fts*, comments_fts*, documents_fts*, 15 tables): FTS5
//     internals, reached only behind a base-table write that has already
//     aborted.
//   - AUTH / PLATFORM / BOOKKEEPING (users, sessions, api_tokens, oauth_*,
//     password_reset_tokens, email_verification_tokens, email_optouts,
//     cli_auth_sessions, platform_settings, stripe_processed_events,
//     mcp_audit_log, share_link_views, member_collection_access,
//     schema_migrations): the command's own help says these are NOT migrated
//     and the operator is told to run `pad auth setup` against the new
//     database. Refusing them would claim a loss the migration never promised
//     to prevent.
//   - The marker table itself, which the migration writes.
//
// Group 3 below — workspace user state the BUNDLE does not carry — is in on a
// judgement the lead ruled: the ruling is refuse-rather-than-lose, and a star
// written to an abandoned file is exactly the loss the marker exists to
// prevent. The posture is nulcolumns.go's Ruling 2: a RAISE(ABORT) trigger
// with no value check is near-free, and litigating a table at a time is how
// that unit spent three rounds.
var migratedRefusalTables = []string{
	// The named victim. The op-log is the durability layer for content that
	// has not been flushed to items.content, and it is the one BUG-3032 could
	// detect but not save.
	"item_yjs_updates",

	// Carried by models.WorkspaceExport: these rows now live in PostgreSQL, so
	// a write here edits a copy that is gone.
	"workspaces",
	"collections",
	"items",
	"comments",
	"item_links",
	"item_versions",
	"versions",
	"item_reminders",

	// Workspace-scoped user state the bundle does NOT carry. item_stars and
	// watches are the sharpest: their loss in a migration is already silent
	// today.
	"documents",
	"attachments",
	"item_stars",
	"watches",
	"comment_reactions",
	"views",
	"agent_roles",
	"custom_templates",
	"webhooks",
	"share_links",
	"progress_snapshots",
	"user_report_layouts",
	"item_grants",
	"collection_grants",
	"workspace_members",
	"workspace_invitations",
	"item_collection_moves",
	"item_workspace_moves",
}

// MigratedRefusalTables returns the population, sorted, for the guard test and
// the renderer to consume the same value.
func MigratedRefusalTables() []string {
	out := append([]string(nil), migratedRefusalTables...)
	sort.Strings(out)
	return out
}

// migratedRemedy composes the operator-facing refusal text.
//
// `destination` names where the data went. The CALLER decides what to put
// there and is responsible for not leaking a password — cmd/pad passes
// maskPassword(toURL), which is known at install time. When the caller has no
// destination to name it passes "", and the text falls back to the command
// that prints one rather than inventing a DSN.
func migratedRemedy(destination string) string {
	where := "the PostgreSQL database it was migrated to"
	if destination != "" {
		where = destination
	}
	return fmt.Sprintf(
		"%s: this SQLite database was migrated to PostgreSQL and is no longer the live copy — "+
			"writes to it would be lost. Use %s instead (see `pad server info`); "+
			"the migration cannot be re-run against this file",
		MigratedMarker, where)
}

// renderMigratedTriggers returns the CREATE TRIGGER statements, one per table
// per event, in a stable order.
//
// THREE EVENTS, NOT ONE. An INSERT trigger alone leaves UPDATE and DELETE
// reachable, and losing an edit to an existing row — or a deletion — into an
// abandoned file is the same defect as losing an insert. SQLite requires a
// separate trigger per event.
func renderMigratedTriggers(destination string) []string {
	// Single-quoted SQL string literal: double any quote in the message rather
	// than trusting the caller's destination to be quote-free.
	msg := strings.ReplaceAll(migratedRemedy(destination), "'", "''")

	var out []string
	for _, table := range MigratedRefusalTables() {
		for _, ev := range []struct{ suffix, on string }{
			{"ins", "INSERT"},
			{"upd", "UPDATE"},
			{"del", "DELETE"},
		} {
			out = append(out, fmt.Sprintf(
				`CREATE TRIGGER IF NOT EXISTS %s%s_%s
BEFORE %s ON %s
FOR EACH ROW
BEGIN
	SELECT RAISE(ABORT, '%s');
END`, migratedTriggerPrefix, table, ev.suffix, ev.on, table, msg))
		}
	}
	return out
}

// MarkMigratedTx installs the refusal triggers and writes the marker row.
//
// IT MUST RUN INSIDE THE MIGRATION'S OWN TRANSACTION, after the last import
// has succeeded and before the commit. That is not a style preference: the
// commit is what publishes the triggers and releases the write lock in one
// step, which is the only ordering in which a deferred appender cannot slip
// between "the lock is gone" and "the refusal exists". A rollback anywhere
// earlier leaves the source completely untouched and re-runnable.
//
// SQLite only; on any other dialect this is a no-op, because PostgreSQL is
// this command's destination and never its abandoned source.
func (s *Store) MarkMigratedTx(tx *sql.Tx, destination string) error {
	if s.dialect.Driver() != DriverSQLite {
		return nil
	}

	if _, err := tx.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (
			marked_at   TEXT NOT NULL,
			destination TEXT NOT NULL,
			remedy      TEXT NOT NULL
		)`, migratedMarkerTable)); err != nil {
		return fmt.Errorf("create migrated marker table: %w", err)
	}

	if _, err := tx.Exec(fmt.Sprintf(
		`INSERT INTO %s (marked_at, destination, remedy) VALUES (?, ?, ?)`, migratedMarkerTable),
		time.Now().UTC().Format(time.RFC3339), destination, migratedRemedy(destination),
	); err != nil {
		return fmt.Errorf("write migrated marker: %w", err)
	}

	for _, stmt := range renderMigratedTriggers(destination) {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("install migrated refusal trigger: %w", err)
		}
	}
	return nil
}

// migratedRemedyIfMarked reports the stored remedy when this database has been
// migrated, and "" when it has not.
//
// IT IS CALLED BEFORE migrate() AND BEFORE THE BACKFILLS, which is the whole
// reason it reads sqlite_master first rather than just selecting from the
// table: on an unmarked database the table does not exist, and a failed select
// is not a usable answer. Checking after migrate() would be worse than
// useless — migrate() and the three backfills in New (collections.prefix,
// workspaces.owner_id, users.username) WRITE, so they would either trip these
// triggers and report a migration failure instead of the remedy, or mutate a
// file this function is about to declare refused.
//
// A QUERY ERROR IS AN ERROR, not "unmarked" — the nulTriggerMigrationApplied
// posture. Folding a transient read failure into "no marker" would open a
// migrated file with its triggers un-consulted, which is the one outcome this
// exists to prevent.
func migratedRemedyIfMarked(q Queryer) (string, error) {
	var present int
	if err := q.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, migratedMarkerTable,
	).Scan(&present); err != nil {
		return "", fmt.Errorf("check migrated marker: %w", err)
	}
	if present == 0 {
		return "", nil
	}

	var remedy string
	err := q.QueryRow(fmt.Sprintf(
		`SELECT remedy FROM %s ORDER BY marked_at DESC LIMIT 1`, migratedMarkerTable)).Scan(&remedy)
	if err == sql.ErrNoRows {
		// The table exists with no row. Nothing in this package produces that
		// state — the table and its row are written in one transaction — so it
		// means someone made the table by hand or emptied it. Treat the table's
		// EXISTENCE as the signal and refuse anyway: a marker that can be
		// disarmed with a DELETE is not a marker.
		return migratedRemedy(""), nil
	}
	if err != nil {
		return "", fmt.Errorf("read migrated marker: %w", err)
	}
	if remedy == "" {
		return migratedRemedy(""), nil
	}
	return remedy, nil
}
