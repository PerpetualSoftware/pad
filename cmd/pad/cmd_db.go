package main

import (
	"database/sql"
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
			// What CAN be removed is the appender. `item_yjs_updates` has exactly
			// ONE non-test writer in this codebase — `internal/collab/room.go`'s
			// readLoop, reached only through the collab WebSocket — so an append
			// requires a live HTTP server with a client attached. (Nothing lowers
			// `content_flushed_op_log_id` either, so a row cannot become stale by
			// the watermark regressing; and a prune only ever makes a row less
			// stale.) `pad db restore` already refuses on the same structural
			// ground, for the WAL rather than the op-log, and docs/backup.md step
			// 3 is already "Stop the server" — this enforces the step the docs
			// ask for.
			//
			// THE CLAIM IS DELIBERATELY NOT "the window cannot exist". The probe
			// asks whether something healthy answers at the CONFIGURED host and
			// port; it cannot see a second server on another port against this
			// same SQLite file, and it does not verify that what answered is
			// serving this database at all. So: no reachable server at the
			// configured address, which is the honest scope. That imprecision cuts
			// both ways, which is why --force exists (mirroring restore's) and why
			// the pre-pass and the post-export bundle check both stay — under
			// --force, or against a server this probe cannot see, they are the
			// remaining protection and the window is narrowed rather than gone.
			//
			// The residual was tracked as BUG-3072 and is now closed from the
			// OTHER END rather than here: this probe still asks about an
			// ADDRESS when the real question is about a FILE, and it still
			// cannot see a second server on another port or any other caller of
			// the exported Store.AppendYjsUpdate. What changed is that a
			// successful migration MARKS the source (MarkMigratedTx below), so
			// such a writer is refused by the database itself instead of
			// succeeding into a file nothing will read again. This probe
			// remains as the cheap EARLY refusal — it fails before any work is
			// done, and its message tells an operator to do the one thing that
			// makes the whole question moot.
			cfg, cfgErr := config.Load()
			if cfgErr != nil {
				// FAIL CLOSED (codex round 3 P1). The previous version warned and
				// proceeded, on the reasoning that an unrelated config problem
				// should not block a migration. That gets the direction backwards
				// for this particular gate: its entire job is to establish that
				// nothing can append, and a config it cannot read is a gate that
				// cannot establish anything. Disclosure is not safety — a warning
				// on a terminal nobody is reading still ends with the migration
				// running against a live server.
				//
				// --force is the escape and already exists, so this is not a dead
				// end for an operator with a broken config who knows the server is
				// down.
				if !forceLiveServer {
					return fmt.Errorf("could not load config to check for a running Pad server (%w) — "+
						"stop the server and re-run with --force if you know it is down. An edit made while "+
						"this migration reads the database is not carried (the bundle has no op-log) and the "+
						"SQLite file holding it is abandoned afterwards", cfgErr)
				}
				fmt.Fprintf(os.Stderr, "WARNING: could not load config (%v), so the running-server check was "+
					"SKIPPED; proceeding because --force was given.\n", cfgErr)
			} else if cli.IsServerRunning(cfg) {
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
			if err := preflightNULForMigration(srcStore, fromPath); err != nil {
				return err
			}

			// ONE SNAPSHOT ACROSS THE WHOLE MIGRATION (BUG-3072).
			//
			// Every read below runs on this transaction: the workspace list,
			// the unflushed-edits gate, and every workspace's export. It is
			// opened BEFORE the gate and released only after the last import,
			// so the whole command is one instant.
			//
			// WHAT THE TRANSACTION BUYS is that nothing can commit underneath
			// it. SQLite has one write lock and `_txlock=immediate` takes it
			// here, on Begin — measured on BUG-3072: another handle's write
			// stays blocked for as long as this is held and lands only after
			// the commit. That is what makes the bundle internally consistent,
			// and it is a real defect closed rather than a tidy-up:
			// ExportWorkspace used to issue six pooled queries with nothing
			// enclosing them, so a concurrent writer could land between any two
			// SECTIONS and produce a bundle that disagrees with itself — an
			// item naming a collection captured before it existed, a link whose
			// endpoint arrived after the items section was read. It is also why
			// the gate below can no longer measure a different instant than the
			// bundles it is gating.
			//
			// The *Q read variants are used throughout for a narrower reason,
			// stated where they are defined: while ONE transaction spans the
			// whole run the pool would answer identically, so threading the
			// executor is what stops that from being load-bearing. Split this
			// transaction and pooled reads diverge silently; transaction-scoped
			// ones do not.
			//
			// THE SNAPSHOT IS NOT WHAT STOPS A CONCURRENT APPEND FROM BEING
			// LOST. Measured on BUG-3072: busy_timeout belongs to the
			// APPENDER's connection, so holding this lock DEFERS that write
			// rather than refusing it, and the deferred write then lands in the
			// file this command is about to abandon. What refuses it is the
			// marker installed below, published by this transaction's COMMIT.
			//
			// A rollback anywhere before that commit leaves the source
			// completely untouched and the command re-runnable.
			snapshot, err := srcStore.BeginSnapshot()
			if err != nil {
				return fmt.Errorf("open migration snapshot on %s: %w", fromPath, err)
			}
			committed := false
			defer func() {
				if !committed {
					_ = snapshot.Rollback()
				}
			}()

			// List workspaces from source
			workspaces, err := srcStore.ListWorkspacesQ(snapshot)
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
			// promise the NUL preflight above makes. It is one indexed EXISTS
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
				pending, err := srcStore.ListItemsPendingContentFlushQ(snapshot, ws.ID)
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

				// A FAILURE HERE IS FATAL, where it used to `continue`
				// (BUG-3072).
				//
				// Skipping was defensible while the command's only effect was
				// on the DESTINATION: a workspace that failed to copy simply
				// was not there, the exit status said so, and the source was
				// untouched. It stops being defensible the moment success
				// MARKS the source as abandoned. A partial run must leave the
				// source unmarked and fully usable, so there is exactly one
				// outcome in which the marker goes on: every workspace
				// migrated. Anything else rolls the snapshot back.
				data, err := srcStore.ExportWorkspaceQ(snapshot, ws.Slug)
				if err != nil {
					return fmt.Errorf("export %s: %w (nothing has been marked; the SQLite database is unchanged)", ws.Slug, err)
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
				if report, err := gateLateStale(ws.Slug, staleBundleItems(data), migrated, discardUnflushedEdits); err != nil {
					fmt.Fprint(os.Stderr, report)
					return err
				}

				stats := fmt.Sprintf("%d collections, %d items, %d comments",
					len(data.Collections), len(data.Items), len(data.Comments))

				// Empty source: this is an operator-run copy of an existing
				// workspace, not a creation surface, and inventing "cli"
				// here would relabel every migrated workspace's origin.
				// Fatal for the same reason the export is (BUG-3072). The
				// DESTINATION is left partially populated by a mid-loop
				// failure — that is pre-existing, there is no transaction
				// spanning two databases, and this unit does not change it.
				// What this unit does guarantee is the half it can: the
				// SOURCE is unmarked, so the operator still has a working
				// database to re-run from.
				if _, err := dstStore.ImportWorkspace(data, "", "", ""); err != nil {
					return fmt.Errorf("import %s: %w (nothing has been marked; the SQLite database is unchanged, "+
						"but PostgreSQL is now PARTIALLY populated — drop and recreate it before re-running)", ws.Slug, err)
				}

				fmt.Fprintf(os.Stderr, "  OK: %s\n", stats)
				migrated++
			}

			// Every workspace landed, so this is the one path that marks the
			// source (BUG-3072). The two arms above return on any failure, so
			// `migrated < len(workspaces)` is now unreachable here — the check
			// that used to live at this spot said "some workspaces failed",
			// which can no longer be true of a run that reaches this line.
			//
			// The order is: mark INSIDE the snapshot, then commit. The commit
			// publishes the refusal triggers and releases the write lock in
			// one step, which is the only ordering in which an appender that
			// has been waiting out its busy_timeout cannot slip between the
			// lock going away and the refusal existing. Measured on BUG-3072:
			// that append then returns SQLITE_CONSTRAINT_TRIGGER naming the
			// migration, where before it returned nil.
			//
			// maskPassword, not toURL: this string is WRITTEN INTO the
			// database and rendered in every refusal message thereafter, so a
			// password in the connection URL would be persisted in the clear
			// and printed to anyone who trips a trigger.
			if err := srcStore.MarkMigratedTx(snapshot, maskPassword(toURL)); err != nil {
				return fmt.Errorf("mark %s as migrated: %w (nothing has been marked; the SQLite database is unchanged, "+
					"but PostgreSQL is now fully populated)", fromPath, err)
			}
			if err := snapshot.Commit(); err != nil {
				// A FAILED COMMIT HAS AN OUTCOME THIS CODE DOES NOT KNOW, and
				// every other failure message in this command can honestly
				// promise the source is unchanged because a rollback undoes
				// the work. This one cannot: the transaction is finished
				// either way, and the deferred Rollback cannot undo a commit
				// that did land. Asserting "unchanged" here would be the exact
				// failure this whole unit exists to remove — telling an
				// operator something about a one-shot irreversible command
				// that nobody measured.
				//
				// So it is MEASURED. The marker is the observable, and reading
				// it on the pool — a different connection from the dead
				// transaction — distinguishes the three real outcomes. Only
				// the third is genuinely unknown, and it says so rather than
				// picking the comfortable answer.
				committed = true // the tx is finished; the deferred rollback has nothing to do
				remedy, checkErr := srcStore.MigratedRemedy()
				switch {
				case checkErr != nil:
					return fmt.Errorf("commit migration snapshot on %s: %w — and re-reading the marker "+
						"afterwards ALSO failed (%v), so whether the commit landed is UNKNOWN. PostgreSQL "+
						"is fully populated. Do NOT re-run until you check: `pad server start` against %s "+
						"either works (the commit did not land) or refuses naming PostgreSQL (it did)",
						fromPath, err, checkErr, fromPath)
				case remedy != "":
					// The commit landed despite the error. Everything the
					// operator asked for has happened, so reporting failure
					// would send them to re-run a migration that is complete.
					fmt.Fprintf(os.Stderr, "WARNING: committing the migration snapshot on %s reported an "+
						"error (%v), but the marker is present, so the commit DID land. The migration is "+
						"COMPLETE — do not re-run it.\n", fromPath, err)
				default:
					return fmt.Errorf("commit migration snapshot on %s: %w (verified: the marker is absent, "+
						"so the SQLite database is unchanged — but PostgreSQL is now fully populated, so "+
						"drop and recreate it before re-running)", fromPath, err)
				}
			} else {
				committed = true
			}

			fmt.Fprintf(os.Stderr, "\nMigration complete: %d/%d workspace(s) migrated.\n", migrated, len(workspaces))
			fmt.Fprintf(os.Stderr, "%s is now marked as migrated and will refuse writes and refuse to be opened.\n", fromPath)

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
		"migrate even though a Pad server appears to be running, OR the config needed to check for one "+
			"could not be read (an edit made during the migration may be lost)")
	cmd.Flags().BoolVar(&discardUnflushedEdits, "discard-unflushed-edits", false,
		"migrate even though some items have edits only in the collaborative op-log, permanently losing them")

	return cmd
}

// gateLateStale is the invariant check on a built bundle: no item the bundle
// carries may be marked as behind its live collaborative document (BUG-3032,
// reshaped by BUG-3072 and BUG-3077).
//
// WHAT IT USED TO BE, AND WHY THAT CHANGED. It was written as a TOCTOU gate:
// the pre-pass read the database at one instant, ExportWorkspace read it at
// another, and an editor could append between the two. BUG-3072 put the
// pre-pass and every export inside ONE snapshot transaction, so those two
// reads are now the same instant by construction and no editor can slip
// between them. The race it was built for cannot occur any more.
//
// SO ITS MESSAGE HAD TO CHANGE, not just its plumbing. Telling an operator
// that "an editor appended while this command was running" is now telling them
// something that cannot have happened, and sending them to stop a server that
// is not the problem. If this fires with discard=false, the pre-pass passed
// these items and the bundle then read them stale INSIDE one snapshot — which
// is a defect in this command, not an operator error, and the message says so
// and asks for it to be filed.
//
// `discard` is BUG-3077, and without it the gate made
// `--discard-unflushed-edits` unreachable: the pre-pass returns (warning, nil)
// under that flag, nothing flushes those items in between, so the bundle still
// carries the marker on exactly them and this refused what the operator had
// just been shown and had just approved. The flag worked only when there was
// nothing to discard — and the unflushed-edits refusal names it as the only
// way out, so an operator whose edits were in a tab that is now gone was
// pointed at a door that was nailed shut.
//
// Separated from the command's RunE for the reason gateUnflushedEdits is: the
// wording is the whole product here, and a message reachable only by running
// two live databases is a message nobody checks until an operator hits it
// during a migration they cannot retry.
//
// `migratedSoFar` is what makes this message different from the pre-pass's.
// The pre-pass runs before the first import and can promise nothing has been
// migrated; this fires mid-loop, so earlier workspaces are already in
// PostgreSQL and claiming otherwise would be false. It says PARTIALLY
// populated instead — still true of the Postgres side, which BUG-3072's
// source-side snapshot does not span — and only falls back to the pre-pass's
// promise when the count is genuinely 0.
func gateLateStale(wsSlug string, stale []store.PendingFlushItem, migratedSoFar int, discard bool) (string, error) {
	if len(stale) == 0 {
		return "", nil
	}
	// The operator was shown these items by the pre-pass and chose to lose
	// them. Re-refusing here would be the command changing its mind about a
	// decision it already took the operator's answer on.
	if discard {
		return "", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nINTERNAL: %d item(s) in %s passed the pre-flight check and then read as\n", len(stale), wsSlug)
	fmt.Fprint(&b, "stale in the exported bundle. Both reads run inside one snapshot transaction,\n")
	fmt.Fprint(&b, "so this cannot be an editor appending mid-migration — it is a defect in\n")
	fmt.Fprint(&b, "migrate-to-pg. Refusing rather than migrating a body whose real text this\n")
	fmt.Fprint(&b, "migration would then abandon:\n\n")
	for _, it := range stale {
		fmt.Fprintf(&b, "    %-12s %s\n", it.Ref, it.Title)
	}
	fmt.Fprint(&b, "\nPlease report this, with the refs above (BUG-3072 / BUG-3077 for context).\n")
	if migratedSoFar > 0 {
		fmt.Fprintf(&b, "%d workspace(s) were already migrated before this refusal; the destination is "+
			"PARTIALLY populated.\n", migratedSoFar)
	} else {
		fmt.Fprint(&b, "Nothing has been migrated.\n")
	}
	fmt.Fprintf(&b, "The SQLite database at the source is UNCHANGED and has NOT been marked as migrated.\n")
	return b.String(), fmt.Errorf("%s: %d item(s) read as stale inside the migration snapshot; refused", wsSlug, len(stale))
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
// ONE CHECK: the scan's violations. That means values every layer refuses for
// a NUL, and, since BUG-3222 (c31fb06c), values that are not valid UTF-8,
// which PostgreSQL refuses too. It used to be two. A NUL in a value shadowed by a LITERAL duplicate key was invisible to
// every layer until BUG-2812, so pre-filter matches the predicate cleared were
// kept as a SUSPECT class and cast on the destination. The token walk reports
// that shape as a violation, and the suspects left were doubled-backslash
// literals the destination accepts, so the class and its destination cast were
// deleted (BUG-3220). ScanNUL judges STORED BYTES, so the same holds for a value
// any earlier release wrote; the PostgreSQL 12 and 17 matrix behind that is on
// BUG-3220's trail.
func preflightNULForMigration(src *store.Store, fromPath string) error {
	report, err := src.ScanNUL()
	if err != nil {
		return fmt.Errorf("NUL preflight: %w", err)
	}
	if !report.Applicable {
		return nil
	}

	// REFUSE only on rows the migration will actually copy. It reads the
	// tables store.MigratedTables names; a NUL in users, platform settings,
	// sessions or the oauth tables cannot break a copy that never touches them,
	// and blocking on one would demand the operator rewrite content unrelated
	// to the migration they asked for (codex round 9).
	//
	// The others are still REPORTED, below. They are real, `pad db scan-nul`
	// lists them, and staying silent about a broken row because this
	// particular command does not care about it would be the
	// information-discarding this preflight already had to be corrected for
	// once.
	migrated := store.MigratedTables()
	var blocking []store.NULViolation
	var elsewhere []store.NULViolation
	for _, v := range report.Violations {
		if migrated[v.Table] {
			blocking = append(blocking, v)
		} else {
			elsewhere = append(elsewhere, v)
		}
	}

	if n := len(elsewhere); n > 0 {
		fmt.Fprintf(os.Stderr,
			"\nNOTE: %d value(s) carrying a NUL or invalid UTF-8 are in tables this migration does not copy\n"+
				"(users, platform settings, auth data). They do not block it, and\n"+
				"'%s' will repair them:\n\n", n, repairNULCommandHint)
		for _, v := range elsewhere {
			fmt.Fprintf(os.Stderr, "  %s\n", v)
		}
	}

	if len(blocking) == 0 {
		return nil
	}

	fmt.Fprintf(os.Stderr, "\nPreflight found %d stored value(s) in %s that PostgreSQL will not accept:\n\n",
		len(blocking), fromPath)
	for _, v := range blocking {
		fmt.Fprintf(os.Stderr, "  %s\n", v)
	}
	fmt.Fprintf(os.Stderr, "\nEach carries a NUL or invalid UTF-8. PostgreSQL refuses a NUL in text (SQLSTATE 22021),\n"+
		"a NUL escape in jsonb (22P05) and invalid UTF-8 (22021). Migrating risks failing partway through the copy, after\n"+
		"some workspaces have already moved, so it is refused up front.\n\n"+
		"Nothing has been migrated. Repair them first:\n\n    %s\n\n"+
		"then re-run this command. To see the same list without migrating: pad db scan-nul\n",
		repairNULCommandHint)

	// The count of BLOCKING rows, the same number the listing above printed.
	// An earlier version returned a different total than it listed, and a
	// refusal whose own reason disagrees with its list is worse than either.
	return fmt.Errorf("%d stored value(s) carry a NUL or invalid UTF-8; nothing was migrated", len(blocking))
}
