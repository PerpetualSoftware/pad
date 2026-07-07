package store

import (
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
)

// appliedMigrations returns the set of migration version strings recorded
// in schema_migrations (the full filename, e.g. "073_workspace_deleted_at_idx.sql").
// The caller must have already ensured the table exists.
func appliedMigrations(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query("SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}
	return applied, nil
}

// hasPending reports whether any embedded migration has not yet been applied.
func hasPending(applied map[string]bool, embedded []string) bool {
	for _, name := range embedded {
		if !applied[name] {
			return true
		}
	}
	return false
}

// guardSchemaAhead refuses to start when the database contains migrations
// this binary doesn't ship — i.e. the DB is AHEAD of the binary, the
// tell-tale of a downgrade (brew/docker rollback to an older pad against a
// newer schema). Running old code on a newer schema silently corrupts data:
// missing columns, dropped-then-recreated tables, changed constraints.
//
// Detection: migration filenames are zero-padded (NNN_name.sql), so they
// sort lexically in the same order they were authored. Any applied version
// that sorts AFTER the highest embedded migration is one the binary has
// never heard of, so the DB is ahead. This is cheap and needs no separate
// version column.
//
// Escape hatch: set AllowSchemaAhead (start --force / PAD_ALLOW_SCHEMA_AHEAD=1)
// to downgrade the refusal to a warning for the operator who has knowingly
// downgraded and accepts the risk.
func guardSchemaAhead(applied map[string]bool, embedded []string) error {
	if len(embedded) == 0 || len(applied) == 0 {
		return nil
	}
	// embedded is sorted ascending by readMigrationNames.
	highestEmbedded := embedded[len(embedded)-1]

	var ahead []string
	for v := range applied {
		if v > highestEmbedded {
			ahead = append(ahead, v)
		}
	}
	if len(ahead) == 0 {
		return nil
	}
	sort.Strings(ahead)

	if AllowSchemaAhead {
		slog.Warn(
			"database schema is AHEAD of this binary — proceeding anyway because the schema-ahead guard was overridden; old code running against a newer schema can corrupt data",
			"unknown_migrations", strings.Join(ahead, ","),
			"highest_known", highestEmbedded,
		)
		return nil
	}

	return fmt.Errorf(
		"database schema is newer than this pad binary: the database has %d migration(s) this binary doesn't ship (%s), the newest known to this binary is %s. "+
			"This almost always means the binary was DOWNGRADED (e.g. brew/docker rollback) — running old code against a newer schema can corrupt data. "+
			"Upgrade pad back to a build that includes those migrations, or, if you have intentionally downgraded and accept the risk, re-run with `pad start --force` (or PAD_ALLOW_SCHEMA_AHEAD=1). "+
			"See the \"Upgrading Pad\" section of the README",
		len(ahead), strings.Join(ahead, ", "), highestEmbedded,
	)
}

// snapshotBeforeMigrate copies the live SQLite database file to
// <db>.pre-<VERSION> before any pending migrations run, so a botched
// upgrade can be rolled back by copying the snapshot back over the DB.
//
// SQLite-only and best-effort: any failure (checkpoint, copy, missing
// file) is logged as a warning and migration proceeds. Refusing to boot
// because a backup file couldn't be written (read-only volume, full disk)
// would be worse than proceeding — the operator following the upgrade docs
// keeps their own backup regardless.
func (s *Store) snapshotBeforeMigrate() {
	if s.dialect.Driver() != DriverSQLite {
		return // Postgres: snapshots are the operator's pg_dump/PITR job.
	}
	if s.dbPath == "" || strings.HasPrefix(s.dbPath, ":memory:") || strings.Contains(s.dbPath, "mode=memory") {
		return // in-memory DB: nothing on disk to copy.
	}
	if _, err := os.Stat(s.dbPath); err != nil {
		return // no file yet (fresh DB) — nothing to snapshot.
	}

	dst := s.dbPath + ".pre-" + sanitizeVersion(BinaryVersion)

	// Preserve the FIRST snapshot taken for this binary version. Migrations
	// apply one file at a time, so a failed migration can leave some already
	// committed; the next boot still sees pending migrations and would call
	// us again. Overwriting here would replace the true pre-upgrade snapshot
	// with a half-migrated DB — destroying the rollback point. If a snapshot
	// for this version already exists, keep it.
	if _, err := os.Stat(dst); err == nil {
		slog.Info("pre-migration snapshot already exists; preserving it", "path", dst)
		return
	}

	// Flush the WAL into the main DB file so the copy is a complete,
	// self-contained snapshot (no dependence on -wal/-shm sidecars).
	// wal_checkpoint returns a (busy, log, checkpointed) row rather than an
	// error when it can't fully checkpoint; surface that so an incomplete
	// snapshot isn't taken silently. At startup the DB has a single
	// connection and no live readers, so busy should always be 0.
	var busy, logFrames, ckpt sql.NullInt64
	if err := s.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &ckpt); err != nil {
		slog.Warn("pre-migration snapshot: WAL checkpoint failed; snapshot may be incomplete", "error", err)
	} else if busy.Int64 != 0 {
		slog.Warn("pre-migration snapshot: WAL checkpoint reported busy; snapshot may miss un-checkpointed WAL frames", "log_frames", logFrames.Int64)
	}

	if err := copyFileContents(s.dbPath, dst); err != nil {
		slog.Warn("pre-migration snapshot failed; continuing with migration (make sure you have your own backup)", "dest", dst, "error", err)
		return
	}
	slog.Info("wrote pre-migration database snapshot", "path", dst)
}

// sanitizeVersion makes a build-version string safe as a filename suffix.
// Build versions can carry spaces/parens (e.g. "1.2.3 (abc1234)"); collapse
// anything outside [A-Za-z0-9._-] to a hyphen so the snapshot path is clean.
func sanitizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// copyFileContents copies src to dst atomically: it writes to a sibling
// temp file, fsyncs it, then renames it into place. A failure therefore
// never leaves a partial file at dst that a later existence check would
// mistake for a complete snapshot.
func copyFileContents(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// Clean up the temp file on any error path.
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()

	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err = out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
