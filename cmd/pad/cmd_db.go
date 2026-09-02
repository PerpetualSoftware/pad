package main

import (
	"database/sql"
	"errors"
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

			// PREFLIGHT: refuse BEFORE moving anything (DOC-2823 S3).
			//
			// The reason this is a preflight and not an error mid-copy is the
			// shape of the failure it replaces. A legacy row carrying a NUL
			// reaches PostgreSQL's jsonb parser during ImportWorkspace and
			// fails there — at a point where earlier workspaces have already
			// been written, so the operator is left with a half-moved
			// database and an error naming a driver, not a cause. The scan is
			// read-only and cheap next to the copy it guards.
			//
			// It does NOT repair. Dave's day-54 ruling: a migration that
			// rewrites user content decides consent for the operator, so this
			// prints the exact command that asks for it.
			if err := preflightNULForMigration(srcStore, dstStore, fromPath); err != nil {
				return err
			}

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

// preflightNULForMigration refuses a SQLite-to-PostgreSQL migration whose
// source carries values PostgreSQL will not accept, listing them and naming the
// repair command.
//
// Nothing has been written to the destination when this runs — it sits above
// the workspace loop, which is the whole point: the failure it replaces
// happened partway through the copy.
//
// TWO CHECKS, because our predicate alone cannot answer the question the
// migration is actually asking (day-54 lead ruling on PR #1233).
//
// The first is the scan's violations: values every layer refuses. The second is
// the SUSPECT class — pre-filter matches the predicate did not refuse. Most of
// those are harmless doubled-backslash literals, but one shape in the set is
// fatal here and invisible to every layer of ours: a NUL in a value shadowed by
// a LITERAL duplicate key, which a map-model decode drops (textguard.KnownGaps,
// which DOC-2823 forbids closing in a single layer).
//
// Dropping that class silently was the defect: the scan already HELD those rows
// as candidates and threw them away, then promised the migration would go
// through. So each suspect is cast on the DESTINATION connection —
// `SELECT $1::jsonb`, side-effect-free, and the very cast an INSERT performs.
// The database that is about to refuse the value is the oracle, which is exact
// in both directions: no over-refusal on a literal, no miss on a shadowed one.
func preflightNULForMigration(src *store.Store, dst *store.Store, fromPath string) error {
	report, err := src.ScanNUL()
	if err != nil {
		return fmt.Errorf("NUL preflight: %w", err)
	}
	if !report.Applicable {
		return nil
	}

	// THE TABLE FILTER COMES FIRST, before the destination is asked anything
	// (codex round 10). The fail-closed rule refuses on a suspect that could
	// not be verified, and running it over suspects from tables the migration
	// never copies meant an unreadable row in `users` or `sessions` blocked a
	// copy that would not have touched it — the same over-refusal round 9
	// fixed for violations, reintroduced through the suspect path.
	//
	// Filtering here also stops the oracle making round trips about rows whose
	// answer cannot matter.
	migrated := store.MigratedTables()
	var migratedSuspects []store.NULSuspect
	var suspectsElsewhere int
	for _, sus := range report.Suspects {
		if migrated[sus.Table] {
			migratedSuspects = append(migratedSuspects, sus)
			continue
		}
		suspectsElsewhere++
	}

	// COUNTED, NOT PROBED, and not silently dropped either (codex round 11).
	// Whether one of these is actually fatal can only be answered by the
	// destination, and asking would put them back inside the fail-closed rule
	// this filter exists to keep them out of. So they are named, with the
	// command that examines them properly — the alternative is a comment
	// claiming they are reported while the code drops them, which is what the
	// first version of this filter did.
	if suspectsElsewhere > 0 {
		fmt.Fprintf(os.Stderr,
			"  NOTE: %d value(s) mentioning a NUL escape are in tables this migration does not copy;\n"+
				"  they cannot block it and were not checked. 'pad db scan-nul' lists them.\n",
			suspectsElsewhere)
	}

	// A nil destination means the oracle is unavailable. That never happens on
	// the real path — migrate-to-pg has connected to the target by the time
	// this runs — but it must be SAID rather than skipped, because silently
	// dropping the suspect class is the exact defect this check was added to
	// correct.
	var refusedSuspects []store.NULSuspect
	var otherFailures []suspectFailure
	if dst == nil {
		if len(migratedSuspects) > 0 {
			fmt.Fprintf(os.Stderr,
				"  NOTE: %d suspect value(s) could not be checked — no destination to ask.\n",
				len(migratedSuspects))
		}
	} else {
		var unverified []suspectFailure
		refusedSuspects, otherFailures, unverified = checkSuspectsAgainstDestination(src, dst, migratedSuspects)

		// FAIL CLOSED. A suspect the destination never rendered a verdict on —
		// a dropped connection, a timeout, a row that could not be read back —
		// is not a pass. Letting it through would be the preflight promising a
		// migration it did not check, which is the defect the suspect class was
		// added to correct, arriving by a different route (codex round 5).
		if len(unverified) > 0 {
			fmt.Fprintf(os.Stderr,
				"\nPreflight could not check %d suspect value(s) against the destination:\n\n",
				len(unverified))
			for _, f := range unverified {
				fmt.Fprintf(os.Stderr, "  %s\n    %v\n", f.suspect, f.err)
			}
			fmt.Fprintln(os.Stderr,
				"\nNothing has been migrated. These values may or may not be acceptable to the\n"+
					"destination; the check did not complete, so this refuses rather than guessing.\n"+
					"Re-run once the destination is reachable.")
			return fmt.Errorf("%d suspect value(s) could not be checked; nothing was migrated", len(unverified))
		}
	}

	// Cast failures for reasons OTHER than a NUL are reported and not refused
	// on. They mean the destination will reject that row too, but a NUL
	// preflight that silently grew into a general one would start refusing
	// migrations that have nothing to do with this bug. Naming them beats
	// discarding them, which is the mistake this whole check exists to correct.
	for _, f := range otherFailures {
		fmt.Fprintf(os.Stderr,
			"  NOTE: %s was rejected by the destination for a non-NUL reason, which this preflight does "+
				"not refuse on: %v\n", f.suspect, f.err)
	}

	// REFUSE only on rows the migration will actually copy. It reads six tables
	// (store.MigratedTables); a NUL in users, platform settings, sessions or the
	// oauth tables cannot break a copy that never touches them, and blocking on
	// one would demand the operator rewrite content unrelated to the migration
	// they asked for (codex round 9).
	//
	// The others are still REPORTED, below — as are the suspects from those
	// tables, counted above. They are real, `pad db scan-nul` lists them, and
	// staying silent about a broken row because this particular command does
	// not care about it would be the information-discarding this preflight
	// already had to be corrected for once.
	var blocking []store.NULViolation
	var elsewhere []store.NULViolation
	for _, v := range report.Violations {
		if migrated[v.Table] {
			blocking = append(blocking, v)
		} else {
			elsewhere = append(elsewhere, v)
		}
	}
	// refusedSuspects is already table-filtered: the oracle was only asked about
	// migrated ones.
	blockingSuspects := refusedSuspects

	if n := len(elsewhere); n > 0 {
		fmt.Fprintf(os.Stderr,
			"\nNOTE: %d value(s) carrying a NUL are in tables this migration does not copy\n"+
				"(users, platform settings, auth data). They do not block it, and\n"+
				"'%s' will repair them:\n\n", n, repairNULCommandHint)
		for _, v := range elsewhere {
			fmt.Fprintf(os.Stderr, "  %s\n", v)
		}
	}

	if len(blocking) == 0 && len(blockingSuspects) == 0 {
		return nil
	}

	total := len(blocking) + len(blockingSuspects)
	fmt.Fprintf(os.Stderr, "\nPreflight found %d stored value(s) in %s that PostgreSQL will not accept:\n\n",
		total, fromPath)
	for _, v := range blocking {
		fmt.Fprintf(os.Stderr, "  %s\n", v)
	}
	for _, sus := range blockingSuspects {
		// Named apart, because these were found by ASKING the destination
		// rather than by our own predicate — an operator comparing this list
		// against `pad db scan-nul`'s violations should be able to see why the
		// two differ.
		fmt.Fprintf(os.Stderr, "  %s (destination refused it; no layer of ours sees this one)\n", sus)
	}
	fmt.Fprintf(os.Stderr, "\nEach carries a NUL, which PostgreSQL refuses in a text or jsonb value —\n"+
		"SQLSTATE 22021 and 22P05. Migrating risks failing partway through the copy, after\n"+
		"some workspaces have already moved, so it is refused up front.\n\n"+
		"Nothing has been migrated. Repair them first:\n\n    %s\n\n"+
		"then re-run this command. To see the same list without migrating: pad db scan-nul\n",
		repairNULCommandHint)

	// `total`, not report.Total(). The first version returned the VIOLATION
	// count here while the listing above showed violations plus refused
	// suspects, so a preflight that refused one suspect and nothing else
	// announced "0 stored value(s) carry a NUL; nothing was migrated" — a
	// refusal whose own reason says there was nothing to refuse. Found by
	// running the command against a real Postgres, not by a test: the tests
	// asserted the message CONTAINED "nothing was migrated" and never read the
	// number.
	return fmt.Errorf("%d stored value(s) carry a NUL; nothing was migrated", total)
}

// suspectFailure pairs a suspect with the destination's complaint.
type suspectFailure struct {
	suspect store.NULSuspect
	err     error
}

// checkSuspectsAgainstDestination asks the target database about each suspect.
//
// Returns the ones it refused for a NUL reason (which the preflight refuses on)
// and the ones it refused for any other reason (which it reports).
// THREE outcomes, not two, and the third is the one codex round 5 found missing:
//
//   - refused    — the destination answered, with a NUL code. The preflight
//     refuses on these.
//   - other      — the destination answered, with some other complaint about
//     the value. Reported, not refused on: a NUL preflight that quietly grew
//     into a general one would block migrations unrelated to this bug.
//   - unverified — the destination did not answer, or the value could not be
//     read back. The caller refuses on these, because an unchecked suspect
//     treated as a pass is exactly what this whole check exists to stop.
func checkSuspectsAgainstDestination(
	src *store.Store, dst *store.Store, suspects []store.NULSuspect,
) (refused []store.NULSuspect, other []suspectFailure, unverified []suspectFailure) {
	for _, sus := range suspects {
		if sus.KeyIncomplete {
			unverified = append(unverified, suspectFailure{sus,
				fmt.Errorf("row has a NULL key column, so its value cannot be read back")})
			continue
		}
		value, rerr := src.ReadNULTargetValue(sus.Table, sus.Column, sus.Key)
		if rerr != nil {
			// Including "the row no longer exists". The scan and this check are
			// separate statements, so a row can legitimately vanish between
			// them — but a row that vanished is also a row whose value nobody
			// verified, and re-running the preflight costs nothing next to a
			// half-finished migration.
			unverified = append(unverified, suspectFailure{sus, rerr})
			continue
		}
		cerr := dst.CheckJSONBAcceptable(value)
		switch {
		case cerr == nil:
			// The common case: a harmless literal the destination accepts.
		case errors.Is(cerr, store.ErrNULDestinationRefused):
			refused = append(refused, sus)
		case errors.Is(cerr, store.ErrDestinationCheckUnavailable):
			unverified = append(unverified, suspectFailure{sus, cerr})
		default:
			other = append(other, suspectFailure{sus, cerr})
		}
	}
	return refused, other, unverified
}
