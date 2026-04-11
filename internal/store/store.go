package store

import (
	"database/sql"
	"embed"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver
	"github.com/xarmian/pad/internal/collections"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

//go:embed pgmigrations/*.sql
var pgMigrationsFS embed.FS

type Store struct {
	db      *sql.DB
	dialect Dialect
}

// D returns the store's dialect for building backend-specific SQL.
func (s *Store) D() Dialect { return s.dialect }

// DB returns the underlying *sql.DB (for use in migrations/testing).
func (s *Store) DB() *sql.DB { return s.db }

// New creates a Store backed by SQLite at the given path.
func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Enable WAL mode
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("enable WAL: %w", err)
	}

	// Enable foreign keys
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return nil, fmt.Errorf("enable foreign keys: %w", err)
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

// NewPostgres creates a Store backed by PostgreSQL.
// The connStr should be a PostgreSQL connection string (e.g. "postgres://user:pass@host/db").
func NewPostgres(connStr string) (*Store, error) {
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}

	// Verify connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	// Connection pool tuning for cloud deployment
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

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

func (s *Store) migrate() error {
	// Create migrations tracking table
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`)
	if err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	migrations := []string{
		"001_initial.sql",
		"002_version_diffs.sql",
		"003_custom_templates.sql",
		"004_progress_snapshots.sql",
		"005_collections.sql",
		"006_item_numbers.sql",
		"007_comments.sql",
		"008_tasks_phase_field.sql",
		"009_ideas_implemented_status.sql",
		"010_webhooks.sql",
		"011_api_tokens.sql",
		"012_users.sql",
		"013_workspace_invitations.sql",
		"014_platform_settings.sql",
		"015_password_resets.sql",
		"016_timeline.sql",
		"017_agent_roles.sql",
		"018_conventions_role_field.sql",
		"019_agent_roles_tools.sql",
		"020_role_sort_order.sql",
		"021_phase_to_links.sql",
		"022_audit_trail.sql",
		"023_parent_link_type.sql",
		"024_phases_to_plans.sql",
		"025_doc_type_plan.sql",
		"026_session_binding.sql",
		"027_totp.sql",
		"028_workspace_sort_order.sql",
		"029_username.sql",
		"030_workspace_owner.sql",
		"031_collection_access.sql",
		"032_permission_indexes.sql",
		"033_grants.sql",
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

	return nil
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

	migrations := []string{
		"001_initial.sql",
		"002_audit_trail.sql",
		"003_parent_link_type.sql",
		"004_doc_type_plan.sql",
		"005_phases_to_plans.sql",
		"006_session_binding.sql",
		"007_totp.sql",
		"008_workspace_sort_order.sql",
		"009_username.sql",
		"010_workspace_owner.sql",
		"011_collection_access.sql",
		"012_permission_indexes.sql",
		"013_grants.sql",
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
