package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/config"

	"github.com/PerpetualSoftware/pad/internal/models"
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

				pgCmd, err := postgresClient("pg_dump", dbURL, pgArgs...)
				if err != nil {
					return err
				}
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
			inputInfo, err := os.Stat(inputFile)
			if err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("backup file not found: %s", inputFile)
				}
				return fmt.Errorf("stat backup file: %w", err)
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

				psqlCmd, err := postgresClient("psql", dbURL, psqlArgs...)
				if err != nil {
					return err
				}
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
			if dstInfo, err := os.Stat(dstPath); err == nil {
				if os.SameFile(inputInfo, dstInfo) {
					return fmt.Errorf("backup file %s is the SQLite database being restored; choose a different backup file", inputFile)
				}
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("stat database: %w", err)
			}

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
	var discardUnflushedEdits bool
	var forceLiveServer bool

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

			// BUG-3032, codex round 2 P1: REMOVE THE APPENDER rather than race it.
			//
			// Round 1's post-export bundle check narrowed the TOCTOU window from
			// "pre-pass → export" to "export → import". Round 2 was right that
			// narrowing is not closing: the source read and the destination write
			// are in two different databases, so no transaction can span them and
			// no amount of re-checking makes them atomic.
			//
			// What CAN be made atomic is nothing-can-append. The only thing that
			// appends to the op-log is a live server with a client attached, and
			// `pad db restore` already refuses for the same structural reason (a
			// running WAL checkpointer). docs/backup.md step 3 is "Stop the
			// server"; this enforces the step the docs already ask for, and with
			// the server down the window does not exist rather than being small.
			//
			// The probe is a heuristic, exactly as it is for restore: it asks
			// whether SOMETHING healthy answers on the configured host/port, not
			// whether that something is serving THIS database. Hence --force,
			// mirroring restore's, so a false positive is not a dead end. Under
			// --force the window is narrowed-not-closed again, and the two later
			// gates are what remain.
			if cfg, cfgErr := config.Load(); cfgErr == nil && cli.IsServerRunning(cfg) {
				if !forceLiveServer {
					return fmt.Errorf("the Pad server appears to be running at %s:%d — stop it first "+
						"('pad server stop') so nothing can append collaborative edits while this migration "+
						"reads the database, or re-run with --force to override. An edit landing mid-migration "+
						"is not carried (the bundle has no op-log) and the SQLite file holding it is abandoned "+
						"after this command", cfg.Host, cfg.Port)
				}
				fmt.Fprintf(os.Stderr, "WARNING: the Pad server appears to be running at %s:%d; migrating anyway "+
					"because --force was given. An edit made in a browser tab during this migration may not "+
					"reach PostgreSQL.\n", cfg.Host, cfg.Port)
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

			// BUG-3032: refuse the migration while any item's stored body is
			// BEHIND its live collaborative document.
			//
			// This gate exists here and nowhere else because this is the only
			// door that ABANDONS its source database. The bundle carries no
			// op-log — models.WorkspaceExport enumerates its sections and
			// item_yjs_updates is not among them — and ImportWorkspace writes
			// ItemExport.Content back as the destination's canonical content.
			// Everywhere else a stale body is merely served late and the real
			// text stays reachable: the two HTTP export/import doors leave the
			// source workspace in place, a cross-workspace copy leaves the
			// source item untouched, and `pad db backup` / `pad db restore`
			// don't come through here at all (VACUUM INTO / pg_dump copy the
			// whole database, op-log included). After this command the operator
			// switches to Postgres and the SQLite file stops being read, so the
			// same staleness is permanent loss.
			//
			// THE CHECK COVERS EVERY WORKSPACE BEFORE THE FIRST IMPORT, so the
			// refusal can truthfully say nothing has been migrated — the same
			// promise the NUL-suspect gate above makes. It is one indexed EXISTS
			// per item rather than a scan of pre-built bundles, which would mean
			// holding every workspace in memory to read one field.
			//
			// The remedy is the only one there is: a browser tab. No Go code in
			// this repo can decode a Yjs update payload (the dumb-relay design),
			// so nothing server-side can move op-log content into items.content
			// — and the two server-side paths that touch the op-log,
			// PruneAndApply and ForceRefreshRoom, write CALLER-supplied content
			// and prune, which would DISCARD exactly these edits. That is why
			// the escape hatch is named for what it does to the data.
			pendingByWorkspace := map[string][]store.PendingFlushItem{}
			pendingTotal := 0
			for _, ws := range workspaces {
				pending, err := srcStore.ListItemsPendingContentFlush(ws.ID)
				if err != nil {
					return fmt.Errorf("check for unflushed edits in %s: %w", ws.Slug, err)
				}
				if len(pending) > 0 {
					pendingByWorkspace[ws.Slug] = pending
					pendingTotal += len(pending)
				}
			}
			report, err := gateUnflushedEdits(workspaces, pendingByWorkspace, pendingTotal, discardUnflushedEdits)
			if report != "" {
				fmt.Fprint(os.Stderr, report)
			}
			if err != nil {
				return err
			}

			migrated := 0
			for _, ws := range workspaces {
				fmt.Fprintf(os.Stderr, "Migrating workspace: %s...\n", ws.Name)

				data, err := srcStore.ExportWorkspace(ws.Slug)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  ERROR exporting %s: %v (skipping)\n", ws.Slug, err)
					continue
				}

				// BUG-3032, codex round 1 P1: the pre-pass above closes the
				// all-or-nothing question, not the TOCTOU one. It reads the
				// database at one instant; `ExportWorkspace` reads it at
				// another, and a tab can append to the op-log in between — so a
				// bundle can carry `content_state` on an item the pre-pass saw
				// as current, and without this check the migration would import
				// that body and then abandon the only copy of the real text.
				//
				// The check is on the BUNDLE rather than on the database, which
				// is the point: the bundle is the artifact about to be written,
				// its marker was evaluated against the row it actually
				// serialised, and no window separates the two. That makes the
				// guarantee exact for the bytes that move, where the pre-pass
				// can only ever be exact for the moment it ran.
				//
				// It costs nothing — the marker is already in hand — and in the
				// intended procedure (docs/backup.md: stop the server, then
				// migrate) it can never fire, because nothing is appending. It
				// exists for the operator who did not stop the server, which
				// nothing enforces.
				if report, err := gateLateStale(ws.Slug, staleBundleItems(data), migrated); err != nil {
					fmt.Fprint(os.Stderr, report)
					return err
				}

				stats := fmt.Sprintf("%d collections, %d items, %d comments",
					len(data.Collections), len(data.Items), len(data.Comments))

				// Empty source: this is an operator-run copy of an existing
				// workspace, not a creation surface, and inventing "cli"
				// here would relabel every migrated workspace's origin.
				if _, err := dstStore.ImportWorkspace(data, "", "", ""); err != nil {
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
	cmd.Flags().BoolVar(&forceLiveServer, "force", false,
		"migrate even though a Pad server appears to be running (an edit made during the migration may be lost)")
	cmd.Flags().BoolVar(&discardUnflushedEdits, "discard-unflushed-edits", false,
		"migrate even though some items have edits only in the collaborative op-log, permanently losing them")

	return cmd
}

// gateLateStale refuses a migration when the BUNDLE about to be imported carries
// bodies the exporter marked as stale, and renders the operator-facing message
// (BUG-3032, codex round 1 P1).
//
// Separated from the command's RunE for the reason gateUnflushedEdits is: the
// wording is the whole product here, and a message reachable only by running two
// live databases is a message nobody checks until an operator hits it during a
// migration they cannot retry.
//
// `migratedSoFar` is what makes this message different from the pre-pass's. The
// pre-pass runs before the first import and can promise nothing has been
// migrated; this fires mid-loop, so earlier workspaces are already in PostgreSQL
// and claiming otherwise would be false. It says PARTIALLY populated instead,
// and only falls back to the pre-pass's promise when the count is genuinely 0.
func gateLateStale(wsSlug string, stale []store.PendingFlushItem, migratedSoFar int) (string, error) {
	if len(stale) == 0 {
		return "", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%d item(s) in %s became stale between the pre-flight check and the\n", len(stale), wsSlug)
	fmt.Fprint(&b, "export — an editor appended to the collaborative op-log while this command\n")
	fmt.Fprint(&b, "was running. Refusing to migrate a body whose real text this migration\n")
	fmt.Fprint(&b, "would then abandon:\n\n")
	for _, it := range stale {
		fmt.Fprintf(&b, "    %-12s %s\n", it.Ref, it.Title)
	}
	fmt.Fprint(&b, "\nStop the Pad server before migrating (docs/backup.md), then re-run.\n")
	if migratedSoFar > 0 {
		fmt.Fprintf(&b, "%d workspace(s) were already migrated before this refusal; the destination is "+
			"PARTIALLY populated.\n", migratedSoFar)
	} else {
		fmt.Fprint(&b, "Nothing has been migrated.\n")
	}
	return b.String(), fmt.Errorf("%s: %d item(s) became stale during the migration; refused", wsSlug, len(stale))
}

// staleBundleItems names the items in a built bundle whose bodies the exporter
// marked as behind their live collaborative documents (BUG-3032).
//
// It reads the bundle's OWN marker rather than re-querying, which is what makes
// it a TOCTOU fix rather than a second race: the marker was evaluated in the
// same SELECT that read the body, so bundle and marker cannot disagree, and
// there is no instant between them for an editor to slip into.
//
// Ref falls back to the slug when the bundle carries no collection prefix for
// the item, on the same reasoning as ListItemsPendingContentFlush: a refusal
// exists to tell an operator what to open, and a fabricated "PREFIX-0" sends
// them looking for something that does not exist.
func staleBundleItems(data *models.WorkspaceExport) []store.PendingFlushItem {
	if data == nil {
		return nil
	}
	prefix := map[string]string{}
	for _, c := range data.Collections {
		prefix[c.ID] = c.Prefix
	}
	var out []store.PendingFlushItem
	for _, it := range data.Items {
		if it.ContentState == "" {
			continue
		}
		ref := it.Slug
		if p := prefix[it.CollectionID]; p != "" && it.ItemNumber > 0 {
			ref = fmt.Sprintf("%s-%d", p, it.ItemNumber)
		}
		out = append(out, store.PendingFlushItem{Ref: ref, Title: it.Title})
	}
	return out
}

// gateUnflushedEdits decides whether a SQLite→PostgreSQL migration may proceed
// when some items' stored bodies are BEHIND their live collaborative documents
// (BUG-3032), and renders the operator-facing detail block either way.
//
// Separated from the command's RunE, and the reason is the NUL gate next door:
// its logic is reachable from a test without two live databases, which is why
// its refusal wording and its exit-status arithmetic both have coverage. A gate
// whose only entry point is a cobra RunE that opens two stores from the
// environment is a gate whose message nobody checks until an operator reads it
// during a migration they cannot retry.
//
// Returns (report, nil) to proceed — report non-empty only under the discard
// flag, carrying the WARNING banner so the loss lands on the terminal record
// rather than being implied by a flag name in shell history — and ("", err) to
// refuse, with the detail block inside the error so the caller cannot print a
// refusal without its reason. `workspaces` supplies the ORDER and the display
// names; a workspace with no pending items contributes nothing.
func gateUnflushedEdits(workspaces []models.Workspace, pending map[string][]store.PendingFlushItem, total int, discard bool) (string, error) {
	if total == 0 {
		return "", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%d item(s) have edits that exist only in the collaborative op-log,\n", total)
	fmt.Fprint(&b, "which this migration does NOT carry. Their bodies would arrive in PostgreSQL\n")
	fmt.Fprint(&b, "as they stand in SQLite now — missing those edits — and the SQLite database\n")
	fmt.Fprint(&b, "holding the real text is abandoned after this command.\n\n")
	for _, ws := range workspaces {
		items := pending[ws.Slug]
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  %s (%s):\n", ws.Name, ws.Slug)
		for _, it := range items {
			fmt.Fprintf(&b, "    %-12s %s\n", it.Ref, it.Title)
		}
	}
	// The only remedy there is. No Go code in this repo can decode a Yjs update
	// payload (the dumb-relay design), so nothing server-side can move op-log
	// content into items.content — and the two server-side paths that touch the
	// op-log, PruneAndApply and ForceRefreshRoom, write CALLER-supplied content
	// and prune, which would DISCARD exactly these edits.
	fmt.Fprint(&b, "\nOpen each of those items in the web UI so the tab flushes its pending edits\n")
	fmt.Fprint(&b, "into the database, then re-run this command. To migrate anyway and lose those\n")
	fmt.Fprint(&b, "edits, re-run with --discard-unflushed-edits.\n")

	if !discard {
		return "", fmt.Errorf("%s\n%d item(s) carry unflushed edits; nothing has been migrated", b.String(), total)
	}
	return "WARNING: --discard-unflushed-edits was given, so these edits will be LOST:\n" + b.String() + "\n", nil
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
	var suspectsElsewhere []store.NULSuspect
	for _, sus := range report.Suspects {
		if migrated[sus.Table] {
			migratedSuspects = append(migratedSuspects, sus)
			continue
		}
		suspectsElsewhere = append(suspectsElsewhere, sus)
	}

	// COUNTED, NOT PROBED, and not silently dropped either (codex round 11).
	// Whether one of these is actually fatal can only be answered by the
	// destination, and asking would put them back inside the fail-closed rule
	// this filter exists to keep them out of. So they are named, with the
	// command that examines them properly — the alternative is a comment
	// claiming they are reported while the code drops them, which is what the
	// first version of this filter did.
	if len(suspectsElsewhere) > 0 {
		// NAMED, not just counted (codex round 12). The rows are already in
		// hand; printing a bare number makes the operator run a second command
		// to learn something this one could have told them.
		fmt.Fprintf(os.Stderr,
			"  NOTE: %d value(s) mentioning a NUL escape are in tables this migration does not copy.\n"+
				"  They cannot block it and were not checked against the destination:\n",
			len(suspectsElsewhere))
		for _, sus := range suspectsElsewhere {
			fmt.Fprintf(os.Stderr, "    %s\n", sus)
		}
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
