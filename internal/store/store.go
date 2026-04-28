package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

//go:embed pgmigrations/*.sql
var pgMigrationsFS embed.FS

type Store struct {
	db            *sql.DB
	dialect       Dialect
	encryptionKey []byte // 32-byte AES-256 key for encrypting sensitive fields (e.g., TOTP secrets)
}

// D returns the store's dialect for building backend-specific SQL.
func (s *Store) D() Dialect { return s.dialect }

// DB returns the underlying *sql.DB (for use in migrations/testing).
func (s *Store) DB() *sql.DB { return s.db }

// New creates a Store backed by SQLite at the given path.
//
// The DSN is configured for safe concurrent use under Go's connection pool:
//
//   - `_pragma=busy_timeout(30000)`: when a connection finds the database
//     locked, retry for up to 30 seconds before returning SQLITE_BUSY.
//     The pragma is applied per-connection by the driver, so every pool
//     member inherits it.
//
//     30s is much larger than the millisecond-scale p95 we observe under
//     normal load — the slack is there to absorb shared-runner jitter
//     under heavy CI contention. With the previous 5s value,
//     TestSQLiteConcurrentWritersNoBusy (25 writers × 5 ops, all
//     contending for BEGIN IMMEDIATE) intermittently failed on the
//     GitHub-hosted SQLite job: the cumulative wall-time of 125
//     serialized inserts on a slow runner can exceed 5s, leaving the
//     unluckiest writer to time out. See BUG-853. Genuine deadlocks
//     don't happen with WAL + BEGIN IMMEDIATE, so the only thing the
//     larger value costs is "how long we wait before declaring lock
//     contention pathological"; for Pad's workload, 30s is fine.
//
//   - `_pragma=foreign_keys(on)`: foreign-key enforcement is per-connection
//     in SQLite. Setting it via the DSN guarantees ALL pool members enforce
//     them, not just the one that received a `db.Exec("PRAGMA ...")` call.
//
//   - `_txlock=immediate`: makes every `db.Begin()` issue `BEGIN IMMEDIATE`
//     instead of the default `BEGIN DEFERRED`. With deferred mode, a
//     transaction starts holding only a SHARED lock and tries to upgrade
//     to a write lock on the first INSERT/UPDATE — and lock upgrades are
//     refused with SQLITE_BUSY *immediately* if another connection holds
//     the write lock, regardless of busy_timeout (SQLite refuses to wait
//     because that would risk deadlock between two connections both holding
//     SHARED locks). With IMMEDIATE, the write lock is acquired at BEGIN
//     time, and busy_timeout's wait-and-retry behaviour does apply, so
//     concurrent writers serialize cleanly instead of failing with
//     "database is locked". Reads are unaffected: single-statement SELECTs
//     don't open a transaction at the SQL layer, so they continue to run
//     concurrently against the WAL snapshot.
//
//     Tradeoff: IMMEDIATE widens the writer critical section. Some update
//     flows (e.g. items.UpdateItem, documents.UpdateDocument) now hold the
//     write lock during diff/version-throttle reads and slug-collision
//     checks, not just during the final INSERT/UPDATE. In practice these
//     reads are sub-millisecond and dominated by network/HTTP latency,
//     so concurrent writers wait briefly for cleanly serialized work
//     rather than failing fast. The pre-fix behaviour was "fail fast
//     with BUSY"; this is strictly better. If a future hot path produces
//     pathologically long write transactions (>100ms holding the lock),
//     the right move is to narrow that specific transaction — not to
//     revert this fix.
//
//     Rollout note for `_pragma=foreign_keys(on)`: FK enforcement was
//     previously per-connection, applied to only one pool member. If a
//     database was historically written through a different pool member
//     with FKs disabled, latent integrity violations may exist; the
//     `sql.Open` itself will not fail, but the next write touching a
//     stale relationship can now return an FK error. For Pad's data
//     model the realistic fallout is small (link tables already cascade),
//     but the integrity check `PRAGMA foreign_key_check` can surface
//     any pre-existing offenders.
//
//   - `journal_mode=WAL` is set via Exec below; WAL is a database-level
//     setting (stored in the file header), so it persists across
//     connections after the first one applies it.
func New(dbPath string) (*Store, error) {
	dsn := dbPath + "?_pragma=busy_timeout(30000)&_pragma=foreign_keys(on)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Enable WAL mode (database-level: persists across connections).
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("enable WAL: %w", err)
	}

	s := &Store{db: db, dialect: &sqliteDialect{}}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	if err := s.backfillItemNumbers(); err != nil {
		return nil, fmt.Errorf("backfill item numbers: %w", err)
	}

	if err := s.backfillWorkspaceOwners(); err != nil {
		return nil, fmt.Errorf("backfill workspace owners: %w", err)
	}

	if err := s.backfillUsernames(); err != nil {
		return nil, fmt.Errorf("backfill usernames: %w", err)
	}

	return s, nil
}

// openPostgresDB opens and configures a PostgreSQL connection pool.
func openPostgresDB(connStr string) (*sql.DB, error) {
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	return db, nil
}

// NewPostgres creates a Store backed by PostgreSQL.
// The connStr should be a PostgreSQL connection string (e.g. "postgres://user:pass@host/db").
func NewPostgres(connStr string) (*Store, error) {
	db, err := openPostgresDB(connStr)
	if err != nil {
		return nil, err
	}

	s := &Store{db: db, dialect: &postgresDialect{}}
	if err := s.migratePostgres(); err != nil {
		return nil, fmt.Errorf("migrate postgres: %w", err)
	}

	if err := s.backfillItemNumbers(); err != nil {
		return nil, fmt.Errorf("backfill item numbers: %w", err)
	}

	if err := s.backfillWorkspaceOwners(); err != nil {
		return nil, fmt.Errorf("backfill workspace owners: %w", err)
	}

	if err := s.backfillUsernames(); err != nil {
		return nil, fmt.Errorf("backfill usernames: %w", err)
	}

	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Ping verifies the database connection is alive.
func (s *Store) Ping() error {
	return s.db.Ping()
}

// readMigrationNames returns sorted .sql filenames from an embedded FS directory.
func readMigrationNames(fsys embed.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read migration dir %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (s *Store) migrate() error {
	// Create migrations tracking table
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`)
	if err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	migrations, err := readMigrationNames(migrationsFS, "migrations")
	if err != nil {
		return err
	}

	for _, name := range migrations {
		// Check if already applied
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version = ?", name).Scan(&count); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if count > 0 {
			continue
		}

		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		if err := execMulti(s.db, string(data)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}

		// Record migration
		_, err = s.db.Exec("INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)", name, now())
		if err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
	}

	// Defensive: log a warning for any FTS trigger that should exist but
	// doesn't. Catches the BUG-822 class of regression — a table-rebuild
	// migration that recorded as applied but silently failed to recreate
	// its triggers, leaving search broken until someone notices.
	s.validateFTSInvariants()

	return nil
}

// expectedFTSTriggers is the canonical list of FTS5 triggers that ship with
// the SQLite migrations. Each entry is (trigger name, table the trigger is
// attached to). The migrations file at the right of each row defines them:
//
//   - items_fts_*    — internal/store/migrations/001_initial.sql
//   - comments_fts_* — internal/store/migrations/007_comments.sql
//   - documents_*    — internal/store/migrations/001_initial.sql (restored by 046 after BUG-822 drift)
//
// If a future migration intentionally renames or removes any of these,
// update this list in the same commit.
var expectedFTSTriggers = []struct {
	name  string
	table string
}{
	{"items_fts_insert", "items"},
	{"items_fts_update", "items"},
	{"items_fts_delete", "items"},
	{"comments_fts_insert", "comments"},
	{"comments_fts_update", "comments"},
	{"comments_fts_delete", "comments"},
	{"documents_ai", "documents"},
	{"documents_au", "documents"},
	{"documents_ad", "documents"},
}

// validateFTSInvariants checks that every trigger in expectedFTSTriggers
// exists. Missing triggers are logged as structured warnings; this function
// is intentionally non-fatal and non-repairing.
//
// Non-fatal: a missing trigger doesn't prevent server startup — the
// operator may have legitimately removed one and just not updated this
// list yet, and we'd rather warn loudly than refuse to boot.
//
// Non-repairing: auto-creating triggers here would mask future legitimate
// removals and obscure the source of truth (the migrations directory).
// The recovery path is a targeted migration like 046_restore_documents_fts_triggers.sql.
//
// SQLite-only: Postgres uses a different trigger model and has its own
// search_vector update functions in pgmigrations.
func (s *Store) validateFTSInvariants() {
	if s.dialect.Driver() != DriverSQLite {
		return
	}
	for _, t := range expectedFTSTriggers {
		var name string
		err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='trigger' AND tbl_name=? AND name=?`,
			t.table, t.name,
		).Scan(&name)
		if err == sql.ErrNoRows {
			slog.Warn(
				"FTS trigger missing — search may silently miss new rows; consider a restoration migration like 046_restore_documents_fts_triggers.sql",
				"trigger", t.name,
				"table", t.table,
			)
			continue
		}
		if err != nil {
			slog.Warn(
				"FTS trigger check errored",
				"trigger", t.name,
				"table", t.table,
				"err", err,
			)
		}
	}
}

// migratePostgres applies PostgreSQL migrations.
// PostgreSQL supports multi-statement execution natively, so we don't need execMulti.
func (s *Store) migratePostgres() error {
	// Create migrations tracking table
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`)
	if err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	migrations, err := readMigrationNames(pgMigrationsFS, "pgmigrations")
	if err != nil {
		return err
	}

	for _, name := range migrations {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version = $1", name).Scan(&count); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if count > 0 {
			continue
		}

		data, err := pgMigrationsFS.ReadFile("pgmigrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		if _, err := s.db.Exec(string(data)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}

		_, err = s.db.Exec("INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)", name, now())
		if err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
	}

	return nil
}

// execMulti executes multiple SQL statements by iteratively using
// database/sql's Exec which processes one statement at a time,
// then advancing past it using the driver's awareness of statement boundaries.
// This handles triggers, FTS5, and other complex SQL correctly.
func execMulti(db *sql.DB, sqlText string) error {
	for {
		sqlText = strings.TrimSpace(sqlText)
		if sqlText == "" {
			return nil
		}

		// Skip comment-only lines at the start
		if strings.HasPrefix(sqlText, "--") {
			idx := strings.Index(sqlText, "\n")
			if idx < 0 {
				return nil
			}
			sqlText = sqlText[idx+1:]
			continue
		}

		// Find the next complete statement by tracking BEGIN/END blocks
		end := findStatementEnd(sqlText)
		if end < 0 {
			// No more complete statements
			return nil
		}

		stmt := strings.TrimSpace(sqlText[:end+1])
		if stmt != "" && stmt != ";" {
			if _, err := db.Exec(stmt); err != nil {
				// Tolerate "duplicate column name" errors from ALTER TABLE ADD COLUMN.
				// This makes migrations idempotent when partially applied (e.g. server
				// crashed after adding a column but before recording the migration).
				upper := strings.ToUpper(strings.TrimSpace(stmt))
				isDupCol := strings.Contains(err.Error(), "duplicate column name")
				isAddCol := strings.HasPrefix(upper, "ALTER TABLE") && strings.Contains(upper, "ADD COLUMN")
				if !(isDupCol && isAddCol) {
					return fmt.Errorf("exec migration statement: %w\nStatement: %.200s", err, stmt)
				}
			}
		}
		sqlText = sqlText[end+1:]
	}
}

// findStatementEnd finds the index of the semicolon that ends the next
// complete SQL statement, correctly handling BEGIN...END blocks.
func findStatementEnd(sql string) int {
	depth := 0
	i := 0
	for i < len(sql) {
		// Skip string literals
		if sql[i] == '\'' {
			i++
			for i < len(sql) {
				if sql[i] == '\'' {
					if i+1 < len(sql) && sql[i+1] == '\'' {
						i += 2 // escaped quote
						continue
					}
					break
				}
				i++
			}
			i++
			continue
		}

		// Skip line comments
		if i+1 < len(sql) && sql[i] == '-' && sql[i+1] == '-' {
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			continue
		}

		// Check for BEGIN (case-insensitive)
		if i+5 <= len(sql) && strings.EqualFold(sql[i:i+5], "BEGIN") {
			// Make sure it's a word boundary
			if (i == 0 || !isAlpha(sql[i-1])) && (i+5 >= len(sql) || !isAlpha(sql[i+5])) {
				depth++
				i += 5
				continue
			}
		}

		// Check for END (case-insensitive)
		if depth > 0 && i+3 <= len(sql) && strings.EqualFold(sql[i:i+3], "END") {
			if (i == 0 || !isAlpha(sql[i-1])) && (i+3 >= len(sql) || !isAlpha(sql[i+3])) {
				depth--
				i += 3
				continue
			}
		}

		// Semicolon outside of any BEGIN...END block = statement end
		if sql[i] == ';' && depth == 0 {
			return i
		}

		i++
	}
	return -1
}

// uniqueSlugExcluding generates a unique slug, excluding a specific document ID
// from the collision check. Used during title renames.
func (s *Store) uniqueSlugExcluding(table, scopeCol, scopeVal, baseSlug, excludeID string) (string, error) {
	slug := baseSlug
	for i := 2; ; i++ {
		var count int
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = ? AND slug = ? AND id != ?", table, scopeCol)
		err := s.db.QueryRow(s.q(query), scopeVal, slug, excludeID).Scan(&count)
		if err != nil {
			return "", err
		}
		if count == 0 {
			return slug, nil
		}
		slug = fmt.Sprintf("%s-%d", baseSlug, i)
	}
}

func isAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

// isValidFieldKey checks that a field name contains only safe characters
// (alphanumeric, underscore, hyphen). This prevents SQL injection when
// field keys from user input are interpolated into JSON path expressions.
func isValidFieldKey(key string) bool {
	if key == "" {
		return false
	}
	for _, c := range key {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// q rebinds a query to the store's dialect (converts "?" to "$1", "$2", etc. for PostgreSQL).
func (s *Store) q(query string) string {
	return s.dialect.Rebind(query)
}

func newID() string {
	return uuid.New().String()
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func parseTimePtr(s *string) *time.Time {
	if s == nil {
		return nil
	}
	t := parseTime(*s)
	return &t
}

// slugify converts a string to a URL-friendly slug.
func slugify(s string) string {
	s = strings.ToLower(s)
	var result []byte
	prevHyphen := false
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			result = append(result, byte(c))
			prevHyphen = false
		} else if c == '\'' {
			// Strip apostrophes without inserting a hyphen
			continue
		} else if !prevHyphen && len(result) > 0 {
			result = append(result, '-')
			prevHyphen = true
		}
	}
	// Trim trailing hyphen
	if len(result) > 0 && result[len(result)-1] == '-' {
		result = result[:len(result)-1]
	}
	return string(result)
}

// backfillItemNumbers assigns prefixes to collections that lack them and
// sequential item_number values to items that don't have one yet.
//
// On first run after the workspace-global numbering migration, this function
// detects the old per-collection unique index and performs a one-time
// renumbering of all items so that item_number is unique per workspace
// (not per collection). This allows items to keep their number when moved
// between collections (e.g. IDEA-42 → BUG-42).
func (s *Store) backfillItemNumbers() error {
	// 1. Backfill collection prefixes
	rows, err := s.db.Query(s.q("SELECT id, name FROM collections WHERE prefix = ''"))
	if err != nil {
		return fmt.Errorf("query collections for prefix backfill: %w", err)
	}
	defer rows.Close()

	type collInfo struct {
		id, name string
	}
	var colls []collInfo
	for rows.Next() {
		var c collInfo
		if err := rows.Scan(&c.id, &c.name); err != nil {
			return err
		}
		colls = append(colls, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, c := range colls {
		prefix := collections.DerivePrefix(c.name)
		if prefix == "" {
			prefix = "ITEM"
		}
		if _, err := s.db.Exec(s.q("UPDATE collections SET prefix = ? WHERE id = ?"), prefix, c.id); err != nil {
			return fmt.Errorf("update prefix for collection %s: %w", c.id, err)
		}
	}

	// 2. Migrate from per-collection to per-workspace numbering (one-time)
	if err := s.migrateToWorkspaceNumbering(); err != nil {
		return fmt.Errorf("migrate to workspace numbering: %w", err)
	}

	// 3. Backfill NULL item_numbers for any new items (per workspace)
	if err := s.backfillNullItemNumbers(); err != nil {
		return fmt.Errorf("backfill null item numbers: %w", err)
	}

	// 4. Ensure the workspace-level unique index exists
	_, err = s.db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_items_workspace_number ON items(workspace_id, item_number)")
	if err != nil {
		return fmt.Errorf("create workspace number index: %w", err)
	}

	return nil
}

// indexExists checks whether the named index exists in the database.
func (s *Store) indexExists(name string) bool {
	var query string
	switch s.dialect.Driver() {
	case DriverPostgres:
		query = "SELECT 1 FROM pg_indexes WHERE indexname = $1"
	default: // SQLite
		query = "SELECT 1 FROM sqlite_master WHERE type='index' AND name=?"
	}
	var one int
	err := s.db.QueryRow(query, name).Scan(&one)
	return err == nil
}

// migrateToWorkspaceNumbering performs a one-time migration from per-collection
// item numbering to per-workspace item numbering. It detects the old index
// (collection_id, item_number) and, if present, renumbers all items within each
// workspace using a single sequential counter ordered by created_at.
//
// The entire operation runs in a single transaction — if anything fails the
// database is left unchanged and the migration will be retried on next startup.
func (s *Store) migrateToWorkspaceNumbering() error {
	// Nothing to migrate if the old index doesn't exist
	if !s.indexExists("idx_items_collection_number") {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Drop the old per-collection unique index
	if _, err := tx.Exec("DROP INDEX IF EXISTS idx_items_collection_number"); err != nil {
		return fmt.Errorf("drop old index: %w", err)
	}

	// Get all workspace IDs
	wsRows, err := tx.Query(s.q("SELECT id FROM workspaces"))
	if err != nil {
		return fmt.Errorf("query workspaces: %w", err)
	}
	var wsIDs []string
	for wsRows.Next() {
		var id string
		if err := wsRows.Scan(&id); err != nil {
			wsRows.Close()
			return err
		}
		wsIDs = append(wsIDs, id)
	}
	wsRows.Close()
	if err := wsRows.Err(); err != nil {
		return err
	}

	// Renumber items per workspace: assign 1, 2, 3… ordered by created_at, id
	for _, wsID := range wsIDs {
		itemRows, err := tx.Query(s.q(
			"SELECT id FROM items WHERE workspace_id = ? ORDER BY created_at ASC, id ASC"),
			wsID,
		)
		if err != nil {
			return fmt.Errorf("query items for workspace %s: %w", wsID, err)
		}

		var itemIDs []string
		for itemRows.Next() {
			var id string
			if err := itemRows.Scan(&id); err != nil {
				itemRows.Close()
				return err
			}
			itemIDs = append(itemIDs, id)
		}
		itemRows.Close()
		if err := itemRows.Err(); err != nil {
			return err
		}

		// Assign sequential numbers across the entire workspace
		for i, itemID := range itemIDs {
			num := i + 1
			if _, err := tx.Exec(s.q("UPDATE items SET item_number = ? WHERE id = ?"), num, itemID); err != nil {
				return fmt.Errorf("renumber item %s to %d: %w", itemID, num, err)
			}
		}
	}

	// Create the new workspace-level unique index inside the transaction
	if _, err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_items_workspace_number ON items(workspace_id, item_number)"); err != nil {
		return fmt.Errorf("create workspace number index: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}

	return nil
}

// backfillNullItemNumbers assigns the next workspace-global item_number to any
// items that have a NULL item_number (e.g. from interrupted inserts or pre-
// migration data).
func (s *Store) backfillNullItemNumbers() error {
	// Get workspaces that have items with NULL item_number
	wsRows, err := s.db.Query(s.q(
		"SELECT DISTINCT workspace_id FROM items WHERE item_number IS NULL"))
	if err != nil {
		return fmt.Errorf("query workspaces with null item numbers: %w", err)
	}
	var wsIDs []string
	for wsRows.Next() {
		var id string
		if err := wsRows.Scan(&id); err != nil {
			wsRows.Close()
			return err
		}
		wsIDs = append(wsIDs, id)
	}
	wsRows.Close()
	if err := wsRows.Err(); err != nil {
		return err
	}

	for _, wsID := range wsIDs {
		itemRows, err := s.db.Query(
			s.q("SELECT id FROM items WHERE workspace_id = ? AND item_number IS NULL ORDER BY created_at ASC, id ASC"),
			wsID,
		)
		if err != nil {
			return fmt.Errorf("query null-numbered items for workspace %s: %w", wsID, err)
		}

		var itemIDs []string
		for itemRows.Next() {
			var id string
			if err := itemRows.Scan(&id); err != nil {
				itemRows.Close()
				return err
			}
			itemIDs = append(itemIDs, id)
		}
		itemRows.Close()
		if len(itemIDs) == 0 {
			continue
		}

		// Get current max in this workspace
		var maxNum int
		if err := s.db.QueryRow(s.q("SELECT COALESCE(MAX(item_number), 0) FROM items WHERE workspace_id = ?"), wsID).Scan(&maxNum); err != nil {
			return fmt.Errorf("get max item_number for workspace %s: %w", wsID, err)
		}

		for _, itemID := range itemIDs {
			maxNum++
			if _, err := s.db.Exec(s.q("UPDATE items SET item_number = ? WHERE id = ?"), maxNum, itemID); err != nil {
				return fmt.Errorf("update item_number for item %s: %w", itemID, err)
			}
		}
	}

	return nil
}

// uniqueSlug generates a unique slug within a scope by appending -2, -3, etc.
func (s *Store) uniqueSlug(table, scopeCol, scopeVal, baseSlug string) (string, error) {
	slug := baseSlug
	for i := 2; ; i++ {
		var count int
		// Check all rows including soft-deleted to respect the DB UNIQUE constraint
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = ? AND slug = ?", table, scopeCol)
		err := s.db.QueryRow(s.q(query), scopeVal, slug).Scan(&count)
		if err != nil {
			return "", err
		}
		if count == 0 {
			return slug, nil
		}
		slug = fmt.Sprintf("%s-%d", baseSlug, i)
	}
}
