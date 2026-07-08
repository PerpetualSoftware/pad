package main

import (
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/config"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// pgDbnameFromURL extracts just the database name from a PostgreSQL DSN for
// display purposes. Handles both the URI form (postgres://.../dbname) and the
// libpq keyword=value form ("host=... dbname=foo ..."). Returns "unknown" when
// the dbname can't be determined — this is display-only, not used to build
// the actual connection.
func pgDbnameFromURL(raw string) string {
	// URI form: postgres://user:pass@host/dbname?opts
	if strings.HasPrefix(raw, "postgres://") || strings.HasPrefix(raw, "postgresql://") {
		if u, err := url.Parse(raw); err == nil {
			if name := strings.TrimPrefix(u.Path, "/"); name != "" {
				return name
			}
		}
	}
	// libpq keyword=value form: "host=... dbname=foo ..."
	for _, tok := range strings.Fields(raw) {
		if strings.HasPrefix(tok, "dbname=") {
			return strings.TrimPrefix(tok, "dbname=")
		}
	}
	return "unknown"
}

// resolveSQLiteDBPath returns the SQLite database path using the SAME
// precedence the server uses (PAD_DB_PATH > PAD_DATA_DIR/pad.db > ~/.pad/pad.db),
// via the shared config loader rather than a hardcoded HOME path. This keeps
// the backup/restore/migrate commands in sync with wherever the server
// actually stores its database — notably PAD_DATA_DIR=/data inside the Docker
// image, and non-HOME layouts on Windows.
func resolveSQLiteDBPath() (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", fmt.Errorf("load config: %w", err)
	}
	return cfg.DBPath, nil
}

// backupSQLite writes an online-safe, self-contained copy of the SQLite
// database at srcPath to outPath using `VACUUM INTO`. Unlike an io.Copy of the
// pad.db/-wal/-shm trio, VACUUM INTO produces a single fully-checkpointed file
// and is safe to run while the server is actively writing: SQLite reads a
// consistent snapshot through the engine instead of us copying live pages out
// from under an in-flight WAL checkpoint.
func backupSQLite(srcPath, outPath string) error {
	// VACUUM INTO refuses to write to an existing file; surface a clear error
	// rather than SQLite's terse "output file already exists".
	if _, err := os.Stat(outPath); err == nil {
		return fmt.Errorf("output file already exists: %s (remove it or choose another -o path)", outPath)
	}

	// busy_timeout lets the read wait out a transient write lock instead of
	// failing immediately with SQLITE_BUSY. No _txlock=immediate — VACUUM INTO
	// only reads the source database.
	dsn := srcPath + "?_pragma=busy_timeout(30000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	// Resolve to an absolute path so the target doesn't depend on the SQLite
	// engine's notion of the current directory.
	absOut, err := filepath.Abs(outPath)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}

	if _, err := db.Exec("VACUUM INTO ?", absOut); err != nil {
		return fmt.Errorf("vacuum into %s: %w", outPath, err)
	}
	return nil
}

func dbBackupCmd() *cobra.Command {
	var output string
	var cronMode bool

	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up the database",
		Long: `Creates a backup of the Pad database.

For PostgreSQL (PAD_DB_DRIVER=postgres): creates a SQL dump using pg_dump.
For SQLite (default): writes an online-safe single-file backup via VACUUM INTO —
safe to run while the server is live. The database path is resolved the same way
the server resolves it (PAD_DB_PATH > PAD_DATA_DIR/pad.db > ~/.pad/pad.db).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			dbDriver := os.Getenv("PAD_DB_DRIVER")
			dbURL := os.Getenv("PAD_DATABASE_URL")

			if dbDriver == "postgres" || dbURL != "" {
				// PostgreSQL backup via pg_dump
				if dbURL == "" {
					return fmt.Errorf("PAD_DATABASE_URL is required when PAD_DB_DRIVER=postgres")
				}

				if output == "" {
					output = fmt.Sprintf("pad-backup-%s.sql", time.Now().Format("20060102-150405"))
				}

				pgArgs := []string{
					"--format", "plain",
					"--clean",
					"--if-exists",
					"--file", output,
				}

				pgCmd := exec.Command("pg_dump", pgArgs...)
				pgCmd.Env = append(os.Environ(), "PGDATABASE="+dbURL)
				pgCmd.Stdout = os.Stdout
				pgCmd.Stderr = os.Stderr

				dbname := pgDbnameFromURL(dbURL)
				if !cronMode {
					fmt.Fprintf(os.Stderr, "Backing up PostgreSQL database %s to %s...\n", dbname, output)
				}

				if err := pgCmd.Run(); err != nil {
					if cronMode {
						slog.Error("backup failed", "error", err, "output", output)
					}
					return fmt.Errorf("pg_dump failed: %w", err)
				}

				if info, err := os.Stat(output); err == nil {
					sizeMB := float64(info.Size()) / 1024 / 1024
					if cronMode {
						slog.Info("backup completed", "output", output, "size_mb", fmt.Sprintf("%.1f", sizeMB))
					} else {
						fmt.Fprintf(os.Stderr, "Backup complete: %s (%.1f MB)\n", output, sizeMB)
					}
				}

				return nil
			}

			// SQLite backup via VACUUM INTO — online-safe single file.
			srcPath, err := resolveSQLiteDBPath()
			if err != nil {
				return err
			}
			if _, err := os.Stat(srcPath); os.IsNotExist(err) {
				return fmt.Errorf("SQLite database not found: %s", srcPath)
			}

			if output == "" {
				output = fmt.Sprintf("pad-backup-%s.db", time.Now().Format("20060102-150405"))
			}

			if !cronMode {
				fmt.Fprintf(os.Stderr, "Backing up SQLite database %s to %s...\n", srcPath, output)
			}

			if err := backupSQLite(srcPath, output); err != nil {
				if cronMode {
					slog.Error("backup failed", "error", err, "output", output)
				}
				return err
			}

			if info, err := os.Stat(output); err == nil {
				sizeMB := float64(info.Size()) / 1024 / 1024
				if cronMode {
					slog.Info("backup completed", "output", output, "size_mb", fmt.Sprintf("%.1f", sizeMB))
				} else {
					fmt.Fprintf(os.Stderr, "Backup complete: %s (%.1f MB)\n", output, sizeMB)
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "output file path (default: pad-backup-YYYYMMDD-HHMMSS.db or .sql)")
	cmd.Flags().BoolVar(&cronMode, "cron", false, "cron mode: structured log output, no interactive messages")

	return cmd
}

func dbRestoreCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "restore <file>",
		Short: "Restore a database from a backup",
		Long: `Restores a Pad database from a backup created by 'pad db backup'.

For PostgreSQL: restores from a SQL dump using psql. Requires PAD_DATABASE_URL.
For SQLite (default): copies the backup file over the live database, whose path
is resolved the same way the server resolves it (PAD_DB_PATH > PAD_DATA_DIR/pad.db
> ~/.pad/pad.db). Stop the server first — restore refuses to run while it detects
a live server (a running WAL checkpoint could clobber the restored file); use
--force to override.

WARNING: This will overwrite the current database contents.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inputFile := args[0]
			if _, err := os.Stat(inputFile); os.IsNotExist(err) {
				return fmt.Errorf("backup file not found: %s", inputFile)
			}

			dbDriver := os.Getenv("PAD_DB_DRIVER")
			dbURL := os.Getenv("PAD_DATABASE_URL")

			if dbDriver == "postgres" || dbURL != "" {
				// PostgreSQL restore via psql
				if dbURL == "" {
					return fmt.Errorf("PAD_DATABASE_URL is required when PAD_DB_DRIVER=postgres")
				}

				dbname := pgDbnameFromURL(dbURL)
				if !force {
					fmt.Fprintf(os.Stderr, "WARNING: This will overwrite the PostgreSQL database '%s' with data from %s.\n", dbname, inputFile)
					fmt.Fprintf(os.Stderr, "Run with --force to skip this confirmation, or press Ctrl+C to abort.\n")
					fmt.Fprintf(os.Stderr, "Continue? [y/N] ")
					var confirm string
					fmt.Scanln(&confirm)
					if confirm != "y" && confirm != "Y" {
						fmt.Fprintln(os.Stderr, "Aborted.")
						return nil
					}
				}

				psqlArgs := []string{
					"--file", inputFile,
					"--single-transaction",
				}

				psqlCmd := exec.Command("psql", psqlArgs...)
				psqlCmd.Env = append(os.Environ(), "PGDATABASE="+dbURL)
				psqlCmd.Stdout = os.Stdout
				psqlCmd.Stderr = os.Stderr

				fmt.Fprintf(os.Stderr, "Restoring database %s from %s...\n", dbname, inputFile)

				if err := psqlCmd.Run(); err != nil {
					return fmt.Errorf("psql restore failed: %w", err)
				}

				fmt.Fprintln(os.Stderr, "Restore complete.")
				return nil
			}

			// SQLite restore via file copy
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			dstPath := cfg.DBPath

			// Refuse to overwrite the database out from under a running server:
			// the server holds it open and its background WAL checkpointer could
			// write pages back over the freshly restored file, corrupting it.
			// Require the server to be stopped (or an explicit --force override).
			if cli.IsServerRunning(cfg) {
				if !force {
					return fmt.Errorf("the Pad server appears to be running at %s:%d — stop it first ('pad server stop') so it can't overwrite the restored database, or re-run with --force to override", cfg.Host, cfg.Port)
				}
				fmt.Fprintln(os.Stderr, "WARNING: the Pad server appears to be running; restoring anyway because --force was given. Stop and restart the server around the restore to avoid corruption.")
			}

			if !force {
				fmt.Fprintf(os.Stderr, "WARNING: This will overwrite the SQLite database at %s with data from %s.\n", dstPath, inputFile)
				fmt.Fprintf(os.Stderr, "Run with --force to skip this confirmation, or press Ctrl+C to abort.\n")
				fmt.Fprintf(os.Stderr, "Continue? [y/N] ")
				var confirm string
				fmt.Scanln(&confirm)
				if confirm != "y" && confirm != "Y" {
					fmt.Fprintln(os.Stderr, "Aborted.")
					return nil
				}
			}

			fmt.Fprintf(os.Stderr, "Restoring SQLite database %s from %s...\n", dstPath, inputFile)

			src, err := os.Open(inputFile)
			if err != nil {
				return fmt.Errorf("open backup file: %w", err)
			}
			defer src.Close()

			dst, err := os.Create(dstPath)
			if err != nil {
				return fmt.Errorf("open database for writing: %w", err)
			}
			defer dst.Close()

			if _, err := io.Copy(dst, src); err != nil {
				return fmt.Errorf("copy backup: %w", err)
			}

			// Also restore WAL and SHM files if they exist alongside the backup
			for _, suffix := range []string{"-wal", "-shm"} {
				walPath := inputFile + suffix
				if _, err := os.Stat(walPath); err == nil {
					walSrc, err := os.Open(walPath)
					if err != nil {
						return fmt.Errorf("open %s: %w", suffix, err)
					}
					walDst, err := os.Create(dstPath + suffix)
					if err != nil {
						walSrc.Close()
						return fmt.Errorf("create %s: %w", suffix, err)
					}
					_, copyErr := io.Copy(walDst, walSrc)
					walSrc.Close()
					walDst.Close()
					if copyErr != nil {
						return fmt.Errorf("copy %s: %w", suffix, copyErr)
					}
				} else {
					// No WAL/SHM in backup (the VACUUM INTO path produces none):
					// remove any stale sidecar at the target. A leftover -wal/-shm
					// would let SQLite replay old WAL state over the freshly
					// restored main DB on next open, so a remove failure is fatal
					// rather than a silent success.
					if err := os.Remove(dstPath + suffix); err != nil && !os.IsNotExist(err) {
						return fmt.Errorf("remove stale %s: %w", suffix, err)
					}
				}
			}

			fmt.Fprintln(os.Stderr, "Restore complete. Restart the Pad server to pick up the restored database.")
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "skip confirmation prompt")

	return cmd
}

func dbMigrateToPgCmd() *cobra.Command {
	var fromPath string
	var toURL string

	cmd := &cobra.Command{
		Use:   "migrate-to-pg",
		Short: "Migrate data from SQLite to PostgreSQL",
		Long: `One-time migration from a SQLite database to PostgreSQL.
Uses application-level export/import to transfer all workspace data.

This reads each workspace from the SQLite database and imports it into
the PostgreSQL database. Users, platform settings, and auth data are
NOT migrated — only workspace content (collections, items, comments,
links, versions).

Steps:
  1. Set up a fresh PostgreSQL database
  2. Run 'pad server start' with PAD_DB_DRIVER=postgres once to create the schema
  3. Stop the server
  4. Run this command to migrate workspace data`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if fromPath == "" {
				resolved, err := resolveSQLiteDBPath()
				if err != nil {
					return err
				}
				fromPath = resolved
			}
			if _, err := os.Stat(fromPath); os.IsNotExist(err) {
				return fmt.Errorf("SQLite database not found: %s", fromPath)
			}

			if toURL == "" {
				toURL = os.Getenv("PAD_DATABASE_URL")
			}
			if toURL == "" {
				return fmt.Errorf("target PostgreSQL URL required: use --to or set PAD_DATABASE_URL")
			}

			// Open source SQLite
			fmt.Fprintf(os.Stderr, "Opening SQLite database: %s\n", fromPath)
			srcStore, err := store.New(fromPath)
			if err != nil {
				return fmt.Errorf("open SQLite: %w", err)
			}
			defer srcStore.Close()

			// Open target PostgreSQL
			fmt.Fprintf(os.Stderr, "Connecting to PostgreSQL: %s\n", maskPassword(toURL))
			dstStore, err := store.NewPostgres(toURL)
			if err != nil {
				return fmt.Errorf("open PostgreSQL: %w", err)
			}
			defer dstStore.Close()

			// List workspaces from source
			workspaces, err := srcStore.ListWorkspaces()
			if err != nil {
				return fmt.Errorf("list workspaces: %w", err)
			}

			if len(workspaces) == 0 {
				fmt.Fprintln(os.Stderr, "No workspaces found in SQLite database.")
				return nil
			}

			fmt.Fprintf(os.Stderr, "Found %d workspace(s) to migrate:\n", len(workspaces))
			for _, ws := range workspaces {
				fmt.Fprintf(os.Stderr, "  - %s (%s)\n", ws.Name, ws.Slug)
			}
			fmt.Fprintln(os.Stderr)

			migrated := 0
			for _, ws := range workspaces {
				fmt.Fprintf(os.Stderr, "Migrating workspace: %s...\n", ws.Name)

				data, err := srcStore.ExportWorkspace(ws.Slug)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  ERROR exporting %s: %v (skipping)\n", ws.Slug, err)
					continue
				}

				stats := fmt.Sprintf("%d collections, %d items, %d comments",
					len(data.Collections), len(data.Items), len(data.Comments))

				if _, err := dstStore.ImportWorkspace(data, "", ""); err != nil {
					fmt.Fprintf(os.Stderr, "  ERROR importing %s: %v (skipping)\n", ws.Slug, err)
					continue
				}

				fmt.Fprintf(os.Stderr, "  OK: %s\n", stats)
				migrated++
			}

			fmt.Fprintf(os.Stderr, "\nMigration complete: %d/%d workspace(s) migrated.\n", migrated, len(workspaces))
			if migrated < len(workspaces) {
				fmt.Fprintln(os.Stderr, "Some workspaces failed — check the errors above.")
				return fmt.Errorf("%d workspace(s) failed to migrate", len(workspaces)-migrated)
			}

			fmt.Fprintln(os.Stderr, "\nNext steps:")
			fmt.Fprintln(os.Stderr, "  1. Set PAD_DB_DRIVER=postgres and PAD_DATABASE_URL in your environment")
			fmt.Fprintln(os.Stderr, "  2. Start the server: pad server start")
			fmt.Fprintln(os.Stderr, "  3. Run 'pad auth setup' to create an admin account on the new database")
			fmt.Fprintln(os.Stderr, "  4. Verify your data in the web UI")

			return nil
		},
	}

	cmd.Flags().StringVar(&fromPath, "from", "", "SQLite database path (default: server-resolved — PAD_DB_PATH > PAD_DATA_DIR/pad.db > ~/.pad/pad.db)")
	cmd.Flags().StringVar(&toURL, "to", "", "PostgreSQL connection URL (default: PAD_DATABASE_URL)")

	return cmd
}

// maskPassword replaces the password in a PostgreSQL URL for safe display.
func maskPassword(pgURL string) string {
	u, err := url.Parse(pgURL)
	if err != nil {
		return "***"
	}
	if _, hasPW := u.User.Password(); hasPW {
		u.User = url.UserPassword(u.User.Username(), "***")
	}
	return u.String()
}

// --- audit-log ---
