package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/config"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// The operator-facing half of DOC-2823 S3 (BUG-2810): find the legacy rows the
// two enforcement layers cannot retroactively fix, and repair them on an
// explicit instruction.
//
// TWO SIBLING COMMANDS RATHER THAN `repair --nul`, which is how DOC-2823 first
// spelled it. A `repair` verb that errors when given no flag is a worse shape
// than two honest siblings, and there is no second kind of repair for it to
// share a namespace with today. `scan-nul` also matches `migrate-to-pg`, the
// existing hyphenated compound in this group.
//
// `scan-nul` IS the dry run, so `repair-nul` grows no --dry-run of its own.

// repairNULCommandHint is the exact command the migrate-to-pg preflight prints
// and this command's own help names. It is the STORE's constant rather than a
// second spelling of the same words: the server quotes that same string in the
// import's strict refusal, and a remedy naming a command that has been renamed
// is worse than no remedy at all.
//
// TestRepairNULHintNamesARealCommand pins it against the cobra command tree, so
// a rename that misses one of the three call sites fails a test rather than an
// operator.
const repairNULCommandHint = store.RepairNULCommand

func dbScanNULCmd() *cobra.Command {
	var fromPath string

	cmd := &cobra.Command{
		Use:   "scan-nul",
		Short: "Report stored values carrying a NUL (read-only)",
		Long: `Counts and locates every stored value that violates Pad's NUL invariant:
a real NUL byte in any protected column, or a JSON escape in a JSON column
that a JSON parser would decode to one.

Such rows can only have been written by a binary older than the enforcement
that now refuses them. They are not cosmetic: their workspace exports fine and
re-imports with a 400, and 'pad db migrate-to-pg' fails partway through the
copy against PostgreSQL's jsonb parser.

The scan itself only reads. Opening the database does apply any pending schema
migrations, exactly as starting the server does — pass --from to inspect a
backup file instead of the live database.

Nothing is repaired. Run '` + repairNULCommandHint + `' for that.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openSQLiteForNULTools(&fromPath)
			if err != nil {
				return err
			}
			if s == nil {
				return nil // Postgres: reported and nothing to do.
			}
			defer s.Close()

			report, err := s.ScanNUL()
			if err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			printNULScanReport(report, fromPath)
			return nil
		},
	}

	cmd.Flags().StringVar(&fromPath, "from", "", "SQLite database path (default: server-resolved — PAD_DB_PATH > PAD_DATA_DIR/pad.db > ~/.pad/pad.db)")
	return cmd
}

func dbRepairNULCmd() *cobra.Command {
	var fromPath string
	var force bool

	cmd := &cobra.Command{
		Use:   "repair-nul",
		Short: "Replace stored NULs with U+FFFD (rewrites user content)",
		Long: `Rewrites every stored value 'pad db scan-nul' reports, replacing each NUL
with U+FFFD (the Unicode replacement character) and leaving the rest of the
value byte for byte as it was.

THIS CHANGES USER CONTENT. It is a separate command, and never part of a
migration, for that reason: a migration that rewrote stored text would decide
on the operator's behalf. Every value it changes is listed.

Run 'pad db scan-nul' first to see what would change — that is the dry run, so
this command has no --dry-run of its own. Running it twice is safe: the second
pass finds nothing to do.

A row whose PRIMARY KEY carries the NUL is reported and left alone, because
repairing it would change the row's identity and could collide with another
row.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Refuse while the server is up, on the same reasoning as
			// 'pad db restore': the report is a claim about a database, and a
			// database somebody else is concurrently writing makes it a claim
			// about a moment that has passed.
			if fromPath == "" {
				cfg, err := config.Load()
				if err != nil {
					return fmt.Errorf("load config: %w", err)
				}
				if cli.IsServerRunning(cfg) && !force {
					return fmt.Errorf("the Pad server appears to be running at %s:%d — stop it first ('pad server stop') so the rows cannot change under the repair, or re-run with --force to override", cfg.Host, cfg.Port)
				}
			}

			s, err := openSQLiteForNULTools(&fromPath)
			if err != nil {
				return err
			}
			if s == nil {
				return nil
			}
			defer s.Close()

			// Show the operator what is about to change BEFORE asking. A
			// confirmation prompt for an unnamed set of rows is not consent.
			scan, err := s.ScanNUL()
			if err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			printNULScanReport(scan, fromPath)
			if scan.Total() == 0 {
				return nil
			}

			if !force {
				fmt.Fprintf(os.Stderr, "\nThis will rewrite %d value(s) above, replacing each NUL with U+FFFD.\n", scan.Total())
				fmt.Fprintf(os.Stderr, "Run with --force to skip this confirmation, or press Ctrl+C to abort.\n")
				fmt.Fprintf(os.Stderr, "Continue? [y/N] ")
				var confirm string
				fmt.Scanln(&confirm)
				if confirm != "y" && confirm != "Y" {
					fmt.Fprintln(os.Stderr, "Aborted. Nothing was changed.")
					return nil
				}
			}

			report, err := s.RepairNUL()
			if err != nil {
				return fmt.Errorf("repair: %w", err)
			}

			fmt.Fprintf(os.Stderr, "\nRepaired %d value(s).\n", len(report.Repaired))
			for _, v := range report.Repaired {
				fmt.Fprintf(os.Stderr, "  %s\n", v)
			}
			if len(report.Skipped) > 0 {
				fmt.Fprintf(os.Stderr, "\nSkipped %d value(s):\n", len(report.Skipped))
				for _, sk := range report.Skipped {
					fmt.Fprintf(os.Stderr, "  %s\n    %s\n", sk.Violation, sk.Reason)
				}
			}
			if len(report.Failed) > 0 {
				fmt.Fprintf(os.Stderr, "\nFailed on %d value(s):\n", len(report.Failed))
				for _, f := range report.Failed {
					fmt.Fprintf(os.Stderr, "  %s\n    %v\n", f.Violation, f.Err)
				}
				return fmt.Errorf("%d value(s) could not be repaired", len(report.Failed))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&fromPath, "from", "", "SQLite database path (default: server-resolved)")
	cmd.Flags().BoolVar(&force, "force", false, "skip the confirmation prompt and the running-server check")
	return cmd
}

// openSQLiteForNULTools resolves the database both commands work on, and
// returns (nil, nil) when the deployment is PostgreSQL — where the state these
// commands exist for cannot be stored at all.
//
// It writes the resolved path back through fromPath so the caller can report
// which database it looked at. A report that does not name its subject is the
// kind an operator can act on against the wrong instance.
func openSQLiteForNULTools(fromPath *string) (*store.Store, error) {
	if *fromPath == "" {
		if os.Getenv("PAD_DB_DRIVER") == "postgres" || os.Getenv("PAD_DATABASE_URL") != "" {
			fmt.Fprintln(os.Stderr,
				"This deployment is PostgreSQL, which refuses these values natively (SQLSTATE 22021 for a\n"+
					"NUL in text, 22P05 for the escape reaching jsonb), so no stored row can carry one.\n"+
					"Nothing to scan or repair.")
			return nil, nil
		}
		resolved, err := resolveSQLiteDBPath()
		if err != nil {
			return nil, err
		}
		*fromPath = resolved
	}
	if _, err := os.Stat(*fromPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("SQLite database not found: %s", *fromPath)
	}
	s, err := store.New(*fromPath)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	return s, nil
}

// printNULScanReport renders a scan for a human, on stderr so a caller piping
// stdout is unaffected.
func printNULScanReport(report *store.NULScanReport, dbPath string) {
	if !report.Applicable {
		fmt.Fprintf(os.Stderr, "Not applicable: %s.\n", report.Reason)
		return
	}

	fmt.Fprintf(os.Stderr, "Scanned %d protected column(s) in %s.\n", report.ColumnsScanned, dbPath)
	if len(report.ColumnsAbsent) > 0 {
		// Not an error — an older schema legitimately lacks later columns —
		// but a census that quietly skipped part of its population is not one.
		fmt.Fprintf(os.Stderr, "  (%d listed column(s) absent from this schema: %v)\n",
			len(report.ColumnsAbsent), report.ColumnsAbsent)
	}

	if report.Total() == 0 {
		fmt.Fprintln(os.Stderr, "No values carrying a NUL were found.")
		return
	}

	fmt.Fprintf(os.Stderr, "\nFound %d value(s) carrying a NUL:\n\n", report.Total())

	byColumn := report.ByColumn()
	for _, key := range sortedCountKeys(byColumn) {
		fmt.Fprintf(os.Stderr, "  %-44s %d\n", key, byColumn[key])
	}

	byWorkspace := report.ByWorkspace()
	fmt.Fprintln(os.Stderr, "\nBy workspace:")
	for _, id := range sortedCountKeys(byWorkspace) {
		label := id
		if label == "" {
			// Sixteen of the protected tables carry no workspace_id — users,
			// sessions, the oauth tables, platform settings. Saying so beats
			// an empty column.
			label = "(instance-wide tables, no workspace)"
		}
		fmt.Fprintf(os.Stderr, "  %-44s %d\n", label, byWorkspace[id])
	}

	fmt.Fprintln(os.Stderr, "\nRows:")
	for _, v := range report.Violations {
		fmt.Fprintf(os.Stderr, "  %s\n", v)
	}
}

func sortedCountKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
