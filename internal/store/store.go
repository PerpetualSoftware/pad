package store

import (
	"database/sql"
	"embed"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/xarmian/pad/internal/collections"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct {
	db *sql.DB
}

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

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	if err := s.backfillItemNumbers(); err != nil {
		return nil, fmt.Errorf("backfill item numbers: %w", err)
	}

	if err := s.backfillWorkspaceOwners(); err != nil {
		return nil, fmt.Errorf("backfill workspace owners: %w", err)
	}

	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
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
				return fmt.Errorf("exec migration statement: %w\nStatement: %.200s", err, stmt)
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
		err := s.db.QueryRow(query, scopeVal, slug, excludeID).Scan(&count)
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
// sequential item_number values to items that don't have one yet. It also
// ensures the unique index on (collection_id, item_number) exists.
func (s *Store) backfillItemNumbers() error {
	// 1. Backfill collection prefixes
	rows, err := s.db.Query("SELECT id, name FROM collections WHERE prefix = ''")
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
		if _, err := s.db.Exec("UPDATE collections SET prefix = ? WHERE id = ?", prefix, c.id); err != nil {
			return fmt.Errorf("update prefix for collection %s: %w", c.id, err)
		}
	}

	// 2. Backfill item numbers per collection
	collRows, err := s.db.Query("SELECT id FROM collections")
	if err != nil {
		return fmt.Errorf("query collections for item number backfill: %w", err)
	}
	defer collRows.Close()

	var collIDs []string
	for collRows.Next() {
		var id string
		if err := collRows.Scan(&id); err != nil {
			return err
		}
		collIDs = append(collIDs, id)
	}
	if err := collRows.Err(); err != nil {
		return err
	}

	for _, collID := range collIDs {
		itemRows, err := s.db.Query(
			"SELECT id FROM items WHERE collection_id = ? AND item_number IS NULL ORDER BY created_at ASC, id ASC",
			collID,
		)
		if err != nil {
			return fmt.Errorf("query items for backfill in collection %s: %w", collID, err)
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

		// Get current max
		var maxNum int
		if err := s.db.QueryRow("SELECT COALESCE(MAX(item_number), 0) FROM items WHERE collection_id = ?", collID).Scan(&maxNum); err != nil {
			return fmt.Errorf("get max item_number for collection %s: %w", collID, err)
		}

		for _, itemID := range itemIDs {
			maxNum++
			if _, err := s.db.Exec("UPDATE items SET item_number = ? WHERE id = ?", maxNum, itemID); err != nil {
				return fmt.Errorf("update item_number for item %s: %w", itemID, err)
			}
		}
	}

	// 3. Create unique index (safe now that all items have numbers)
	_, err = s.db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_items_collection_number ON items(collection_id, item_number)")
	if err != nil {
		return fmt.Errorf("create unique index: %w", err)
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
		err := s.db.QueryRow(query, scopeVal, slug).Scan(&count)
		if err != nil {
			return "", err
		}
		if count == 0 {
			return slug, nil
		}
		slug = fmt.Sprintf("%s-%d", baseSlug, i)
	}
}
