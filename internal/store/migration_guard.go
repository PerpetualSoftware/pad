package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite"
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

// snapshotBeforeMigrate writes a consistent snapshot of the live SQLite
// database to <db>.pre-<VERSION>-m<NNN> (see snapshotPathFor) before any
// pending migrations run, so a botched upgrade can be rolled back by copying
// the snapshot back over the DB. targetMigration is the migration this run is
// migrating TO: the highest embedded migration filename.
//
// SQLite-only and best-effort: any failure (backup, I/O, missing file) is
// logged as a warning and migration proceeds. Refusing to boot because a
// backup file couldn't be written (read-only volume, full disk) would be
// worse than proceeding — the operator following the upgrade docs keeps their
// own backup regardless.
func (s *Store) snapshotBeforeMigrate(targetMigration string) {
	if s.dialect.Driver() != DriverSQLite {
		return // Postgres: snapshots are the operator's pg_dump/PITR job.
	}
	if s.dbPath == "" || strings.HasPrefix(s.dbPath, ":memory:") || strings.Contains(s.dbPath, "mode=memory") {
		return // in-memory DB: nothing on disk to snapshot.
	}
	if _, err := os.Stat(s.dbPath); err != nil {
		return // no file yet (fresh DB) — nothing to snapshot.
	}

	dst := s.snapshotPath(targetMigration)

	// Preserve the FIRST snapshot taken for this key. Migrations apply one
	// file at a time, so a failed migration can leave some already committed;
	// the next boot of the same binary still sees pending migrations and
	// would call us again. Overwriting here would replace the true
	// pre-upgrade snapshot with a half-migrated DB — destroying the rollback
	// point. If a snapshot for this key already exists, keep it.
	//
	// The key is the binary version AND the target migration, not the
	// version alone: a dev build's version is always "dev", so a
	// version-only key was written once and then preserved forever, and
	// every later dev-build upgrade ran against that stale snapshot. A build
	// that ships a new migration therefore gets a new key and a fresh
	// snapshot, while a retry of the same upgrade keeps the same key. Once
	// the run that wrote this key's snapshot completes successfully,
	// pruneOldSnapshots keeps it plus the newest earlier one and removes
	// older snapshots of this shape.
	if _, err := os.Stat(dst); err == nil {
		slog.Info("pre-migration snapshot already exists; preserving it", "snapshot", dst)
		return
	}

	// Take the snapshot via SQLite's online backup API so committed WAL
	// frames are included consistently even while another connection holds
	// a WAL read transaction, and implicit rowids are preserved.
	if err := backupSQLiteFile(s.dbPath, dst); err != nil {
		slog.Warn("pre-migration snapshot failed; continuing with migration (make sure you have your own backup)", "dest", dst, "error", err)
		return
	}
	slog.Info("wrote pre-migration database snapshot", "snapshot", dst)
}

// pruneSnapshotsAfterMigrate is called at the end of a migrate run that
// completed successfully and in which a snapshot was due. It applies the same
// guards as snapshotBeforeMigrate (SQLite, a real file path) and then keeps
// this run's snapshot plus the newest earlier one; see pruneOldSnapshots.
func (s *Store) pruneSnapshotsAfterMigrate(targetMigration string) {
	if s.dialect.Driver() != DriverSQLite {
		return // Postgres: no snapshot files.
	}
	if s.dbPath == "" || strings.HasPrefix(s.dbPath, ":memory:") || strings.Contains(s.dbPath, "mode=memory") {
		return
	}
	pruneOldSnapshots(s.dbPath, s.snapshotPath(targetMigration))
}

// pruneOldSnapshots removes pre-migration snapshots of dbPath older than the
// newest one other than keep, so each upgrade does not leave another full
// copy of the database behind. keep is this run's snapshot; it and the single
// newest other snapshot (by modification time, ties broken by name
// descending) survive. The snapshot is renamed into place from a fresh temp
// file, so its mtime is its creation time.
//
// If keep is not on disk (the backup failed), nothing is removed: the older
// snapshots are then the only rollback point.
//
// Only regular files whose name is exactly what snapshotPathFor produces for
// this database are candidates (see snapshotNamePattern). A name the builder
// did not produce (a hand-made copy, an older version-only snapshot, a
// .tmp, a symlink, a directory) is left for a person to deal with. For a
// removed snapshot, its -wal and -shm siblings are removed too, if they
// exist as regular files.
//
// Best-effort: every failure is a warning and the sweep continues.
func pruneOldSnapshots(dbPath, keep string) {
	if _, err := os.Stat(keep); err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("pre-migration snapshot prune: cannot stat the current snapshot; removing nothing", "snapshot", keep, "error", err)
		}
		return
	}

	dir := filepath.Dir(dbPath)
	keepName := filepath.Base(keep)
	pattern := snapshotNamePattern(filepath.Base(dbPath))

	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Warn("pre-migration snapshot prune: cannot list the database directory", "dir", dir, "error", err)
		return
	}

	type candidate struct {
		path  string
		name  string
		mtime time.Time
	}
	var candidates []candidate
	for _, e := range entries {
		name := e.Name()
		if name == keepName || !pattern.MatchString(name) {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			slog.Warn("pre-migration snapshot prune: cannot stat a snapshot; leaving it", "file", path, "error", err)
			continue
		}
		if !info.Mode().IsRegular() {
			continue // symlink, directory, device: not something we wrote.
		}
		candidates = append(candidates, candidate{path: path, name: name, mtime: info.ModTime()})
	}
	if len(candidates) <= 1 {
		return
	}

	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].mtime.Equal(candidates[j].mtime) {
			return candidates[i].mtime.After(candidates[j].mtime)
		}
		return candidates[i].name > candidates[j].name
	})

	for _, c := range candidates[1:] {
		if err := os.Remove(c.path); err != nil {
			slog.Warn("pre-migration snapshot prune: cannot remove an old snapshot", "file", c.path, "error", err)
			continue
		}
		slog.Info("removed old pre-migration snapshot", "file", c.path)
		for _, suffix := range []string{"-wal", "-shm"} {
			sib := c.path + suffix
			info, err := os.Lstat(sib)
			if err != nil {
				if !os.IsNotExist(err) {
					slog.Warn("pre-migration snapshot prune: cannot stat a snapshot sidecar; leaving it", "file", sib, "error", err)
				}
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if err := os.Remove(sib); err != nil {
				slog.Warn("pre-migration snapshot prune: cannot remove a snapshot sidecar", "file", sib, "error", err)
				continue
			}
			slog.Info("removed old pre-migration snapshot", "file", sib)
		}
	}
}

// snapshotNamePattern matches exactly the file names snapshotPathFor builds
// for a database file named base: <base>.pre-<version>-m<digits>, where
// <version> is "dev", "unknown", or a sanitized semver-like build version
// (optional "v", then <digits>.<digits>, then any of [A-Za-z0-9._-]) and the
// key is the numeric migration prefix. Anything else is not ours to remove.
func snapshotNamePattern(base string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(base) +
		`\.pre-(?:dev|unknown|v?[0-9]+\.[0-9]+[A-Za-z0-9._-]*)-m[0-9]+$`)
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

// sqliteFileURI builds an escaped SQLite file: URI for path so filesystem
// characters like '?' or '#' are not interpreted as URI options. Windows drive
// paths get a leading slash in file URIs.
func sqliteFileURI(path, rawQuery string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path = filepath.ToSlash(abs)
	if filepath.VolumeName(abs) != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: rawQuery}).String(), nil
}

// backupSQLiteFile writes a consistent, self-contained copy of srcPath to dst
// using SQLite's online backup API (which incorporates committed WAL frames
// even while readers hold WAL marks and preserves implicit rowids, unlike
// VACUUM INTO). It backs up into a sibling temp file, chmods and fsyncs it,
// then renames it into place so a failure never leaves a partial file at dst
// or tmp.
func backupSQLiteFile(srcPath, dst string) (retErr error) {
	tmp := dst + ".tmp"
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(tmp + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	defer func() {
		if retErr != nil {
			for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
				_ = os.Remove(tmp + suffix)
			}
		}
	}()

	srcURI, err := sqliteFileURI(srcPath, "mode=ro&_pragma=busy_timeout(30000)")
	if err != nil {
		return err
	}
	dstURI, err := sqliteFileURI(tmp, "")
	if err != nil {
		return err
	}

	if err := runSQLiteOnlineBackup(srcURI, dstURI); err != nil {
		return err
	}

	f, err := os.OpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := errors.Join(f.Chmod(0o600), f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func runSQLiteOnlineBackup(srcURI, dstURI string) (retErr error) {
	db, err := sql.Open("sqlite", srcURI)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, db.Close()) }()

	conn, err := db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, conn.Close()) }()

	return conn.Raw(func(raw any) error {
		b, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("sqlite connection %T does not support online backup", raw)
		}
		backup, err := b.NewBackup(dstURI)
		if err != nil {
			return err
		}
		more, stepErr := backup.Step(-1)
		if more && stepErr == nil {
			stepErr = errors.New("SQLite backup did not finish")
		}
		// Finalize even on failure to roll back the private destination, and
		// close it explicitly: Backup.Finish discards its Close error.
		dstConn, finishErr := backup.Commit()
		if dstConn != nil {
			finishErr = errors.Join(finishErr, dstConn.Close())
		}
		return errors.Join(stepErr, finishErr)
	})
}

// snapshotPath is snapshotPathFor for this store's database file.
func (s *Store) snapshotPath(targetMigration string) string {
	return snapshotPathFor(s.dbPath, targetMigration)
}

// snapshotPathFor is the one place the pre-migration snapshot filename is
// built: <dbPath>.pre-<sanitized BinaryVersion>-m<target key>, e.g.
// pad.db.pre-0.21.0-m183 or pad.db.pre-dev-m183.
func snapshotPathFor(dbPath, targetMigration string) string {
	return dbPath + ".pre-" + sanitizeVersion(BinaryVersion) + "-m" + snapshotTargetKey(targetMigration)
}

// snapshotTargetKey reduces a migration filename to the part of the snapshot
// key that names it: the numeric prefix before the first "_" ("183" from
// "183_foo.sql"), or, for a name with no "_", the name without its ".sql"
// suffix. The result gets the same character sanitisation as the version.
func snapshotTargetKey(name string) string {
	if i := strings.Index(name, "_"); i >= 0 {
		name = name[:i]
	} else {
		name = strings.TrimSuffix(name, ".sql")
	}
	return sanitizeVersion(name)
}
