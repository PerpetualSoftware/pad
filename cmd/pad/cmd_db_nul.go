package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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
			proceed, err := resolveNULToolsTarget(&fromPath)
			if err != nil {
				return err
			}
			if !proceed {
				return nil // Postgres: reported and nothing to do.
			}
			s, err := openNULToolsStore(fromPath)
			if err != nil {
				return err
			}
			defer s.Close()

			report, err := s.ScanNUL()
			if err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			printNULScanReport(os.Stdout, report, fromPath)
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
			proceed, err := resolveNULToolsTarget(&fromPath)
			if err != nil {
				return err
			}
			if !proceed {
				return nil
			}

			// Refuse while the server is up, on the same reasoning as
			// 'pad db restore': the report is a claim about a database, and a
			// database somebody else is concurrently writing makes it a claim
			// about a moment that has passed.
			//
			// The check is on the RESOLVED PATH, not on whether --from was
			// given. Skipping it whenever --from was passed made the guard
			// opt-out by accident: `--from` pointing at the live database —
			// which is exactly what an operator copying the path out of
			// `pad db scan-nul`'s output would type — repaired underneath a
			// running server with no warning. A --from that names an unrelated
			// backup is still unguarded, and should be: nothing is writing it.
			if !force {
				if err := refuseIfServerOwns(fromPath); err != nil {
					return err
				}
			}

			s, err := openNULToolsStore(fromPath)
			if err != nil {
				return err
			}
			defer s.Close()

			// Show the operator what is about to change BEFORE asking. A
			// confirmation prompt for an unnamed set of rows is not consent.
			scan, err := s.ScanNUL()
			if err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			printNULScanReport(os.Stdout, scan, fromPath)
			if scan.Total() == 0 && len(scan.Suspects) == 0 {
				return nil
			}

			if !force {
				fmt.Fprintf(os.Stderr, "\nThis will rewrite the %d value(s) above, replacing each NUL with "+
					"U+FFFD, and inspect %d suspect value(s) — rewriting only those that hide a NUL behind "+
					"a repeated key.\n", scan.Total(), len(scan.Suspects))
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

			// Only printed when there is something to say. A bare
			// "Repaired 0 value(s)." above a suspect section that DID repair
			// something reads as a contradiction.
			if n := len(report.Repaired); n > 0 {
				fmt.Fprintf(os.Stdout, "\nRepaired %d value(s).\n", n)
				for _, v := range report.Repaired {
					fmt.Fprintf(os.Stdout, "  %s\n", v)
				}
			}
			if n := len(report.SuspectsRepaired); n > 0 {
				fmt.Fprintf(os.Stdout, "\nRepaired %d value(s) from the suspect list — "+
					"a NUL hidden behind a repeated JSON key:\n", n)
				for _, sus := range report.SuspectsRepaired {
					fmt.Fprintf(os.Stdout, "  %s\n", sus)
				}
			}
			if n := len(report.SuspectsClean); n > 0 {
				fmt.Fprintf(os.Stdout, "\n%d suspect value(s) needed nothing — they mention the escape "+
					"without using it.\n", n)
			}
			if n := len(report.SuspectsSkipped); n > 0 {
				fmt.Fprintf(os.Stdout, "\n%d suspect value(s) could not be addressed (NULL key column).\n", n)
			}
			if len(report.Skipped) > 0 {
				fmt.Fprintf(os.Stdout, "\nSkipped %d value(s):\n", len(report.Skipped))
				for _, sk := range report.Skipped {
					fmt.Fprintf(os.Stdout, "  %s\n    %s\n", sk.Violation, sk.Reason)
				}
			}
			if n := len(report.SuspectsFailed); n > 0 {
				fmt.Fprintf(os.Stdout, "\nFailed on %d suspect value(s):\n", n)
				for _, f := range report.SuspectsFailed {
					fmt.Fprintf(os.Stdout, "  %s\n    %v\n", f.Suspect, f.Err)
				}
			}
			if len(report.Failed) > 0 {
				fmt.Fprintf(os.Stdout, "\nFailed on %d value(s):\n", len(report.Failed))
				for _, f := range report.Failed {
					fmt.Fprintf(os.Stdout, "  %s\n    %v\n", f.Violation, f.Err)
				}
			}
			return nulRepairExitError(report)
		},
	}

	cmd.Flags().StringVar(&fromPath, "from", "", "SQLite database path (default: server-resolved)")
	cmd.Flags().BoolVar(&force, "force", false, "skip the confirmation prompt and the running-server check")
	return cmd
}

// resolveNULToolsTarget works out which database file the two commands act on,
// WITHOUT opening it, and reports whether there is anything to do.
//
// Resolution is separated from opening because opening is not free of
// consequence: store.New runs any pending schema migrations, exactly as
// starting the server does. The repair must be able to REFUSE — because the
// server is running and owns this file — before that happens, and an earlier
// version opened first and checked second, which made the refusal arrive after
// the thing it was protecting against.
//
// It writes the resolved path back through fromPath so the caller can report
// which database it looked at. A report that does not name its subject is the
// kind an operator can act on against the wrong instance.
func resolveNULToolsTarget(fromPath *string) (proceed bool, err error) {
	if *fromPath == "" {
		// PAD_DB_DRIVER ALONE decides, and PAD_DATABASE_URL deliberately does
		// not (codex round 9). cmd_server.go opens PostgreSQL only when
		// PAD_DB_DRIVER=postgres; PAD_DATABASE_URL is also migrate-to-pg's
		// TARGET, and its default at that. Treating the URL as proof of a
		// PostgreSQL deployment broke the exact flow this unit prescribes: the
		// preflight refuses, tells the operator to run `pad db repair-nul`,
		// and — with the target URL still exported in their shell — that
		// command announced there was nothing to repair and exited 0.
		if os.Getenv("PAD_DB_DRIVER") == "postgres" {
			fmt.Fprintln(os.Stderr,
				"This deployment is PostgreSQL, which refuses these values natively (SQLSTATE 22021 for a\n"+
					"NUL in text, 22P05 for the escape reaching jsonb), so no stored row can carry one.\n"+
					"Nothing to scan or repair.")
			return false, nil
		}
		resolved, rerr := resolveSQLiteDBPath()
		if rerr != nil {
			return false, rerr
		}
		*fromPath = resolved
	}
	if _, serr := os.Stat(*fromPath); os.IsNotExist(serr) {
		return false, fmt.Errorf("SQLite database not found: %s", *fromPath)
	}
	return true, nil
}

// openNULToolsStore opens the resolved database. Opening applies any pending
// schema migrations, which is why the caller does its refusing first.
func openNULToolsStore(path string) (*store.Store, error) {
	s, err := store.New(path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	return s, nil
}

// printNULScanReport renders a scan for a human.
//
// ON STDOUT, unlike the rest of this command group. `pad db backup` and
// `pad db restore` keep their progress on stderr because stdout may carry the
// backup itself; these two commands emit no data at all, and their REPORT is
// the whole point — an operator piping `pad db scan-nul > affected.txt` should
// get the list, not an empty file. Only the confirmation prompt stays on
// stderr, where a prompt belongs.
func printNULScanReport(w io.Writer, report *store.NULScanReport, dbPath string) {
	if !report.Applicable {
		fmt.Fprintf(w, "Not applicable: %s.\n", report.Reason)
		return
	}

	fmt.Fprintf(w, "Scanned %d protected column(s) in %s.\n", report.ColumnsScanned, dbPath)
	if len(report.ColumnsAbsent) > 0 {
		// Not an error — an older schema legitimately lacks later columns —
		// but a census that quietly skipped part of its population is not one.
		fmt.Fprintf(w, "  (%d listed column(s) absent from this schema: %v)\n",
			len(report.ColumnsAbsent), report.ColumnsAbsent)
	}

	if report.Total() == 0 {
		fmt.Fprintln(w, "No values carrying a NUL were found.")
		printNULSuspects(w, report)
		return
	}

	fmt.Fprintf(w, "\nFound %d value(s) carrying a NUL:\n\n", report.Total())

	byColumn := report.ByColumn()
	for _, key := range sortedCountKeys(byColumn) {
		fmt.Fprintf(w, "  %-44s %d\n", key, byColumn[key])
	}

	byWorkspace := report.ByWorkspace()
	fmt.Fprintln(w, "\nBy workspace:")
	for _, id := range sortedCountKeys(byWorkspace) {
		label := id
		if label == "" {
			// Sixteen of the protected tables carry no workspace_id — users,
			// sessions, the oauth tables, platform settings. Saying so beats
			// an empty column.
			label = "(instance-wide tables, no workspace)"
		}
		fmt.Fprintf(w, "  %-44s %d\n", label, byWorkspace[id])
	}

	fmt.Fprintln(w, "\nRows:")
	for _, v := range report.Violations {
		fmt.Fprintf(w, "  %s\n", v)
	}

	printNULSuspects(w, report)
}

// printNULSuspects renders the suspect class under its own heading.
//
// SEPARATE FROM THE VIOLATIONS, deliberately. These are not values Pad refuses
// — most are ordinary text that merely contains the escape's leading characters
// — so listing them among the violations would tell an operator their database
// is more broken than it is. But one shape in the set is fatal to a PostgreSQL
// migration and invisible to every check Pad makes, and the honest thing is to
// say so rather than to drop the whole class silently (day-54 lead ruling on
// PR #1233).
//
// The heading names the resolution rather than leaving the operator to guess:
// migrate-to-pg decides these by asking the destination, and repair-nul fixes
// the fatal shape without touching the harmless ones.
func printNULSuspects(w io.Writer, report *store.NULScanReport) {
	if len(report.Suspects) == 0 {
		return
	}
	fmt.Fprintf(w, "\nAlso found %d value(s) that MENTION a NUL escape without carrying one:\n\n",
		len(report.Suspects))
	for _, sus := range report.Suspects {
		fmt.Fprintf(w, "  %s\n", sus)
	}
	fmt.Fprintln(w, "\n  These are almost always harmless — text that writes about the escape rather than")
	fmt.Fprintln(w, "  using it. They are listed because ONE shape in this set is not: a NUL hidden behind")
	fmt.Fprintln(w, "  a repeated JSON key, which PostgreSQL refuses and no check here can see.")
	fmt.Fprintln(w, "  'pad db migrate-to-pg' resolves each one by asking the destination database, and")
	fmt.Fprintln(w, "  '"+repairNULCommandHint+"' fixes the fatal shape while leaving the rest alone.")
}

func sortedCountKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// refuseIfServerOwns errors when dbPath is the database a running server is
// serving.
//
// Paths are compared after EvalSymlinks and Abs, because "the same file"
// reached by two spellings is the case the comparison exists for — a symlinked
// data directory, or a relative --from typed from a different working
// directory. A path that cannot be resolved falls back to its cleaned absolute
// form rather than being treated as different, so the guard errs toward
// refusing.
func refuseIfServerOwns(dbPath string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !cli.IsServerRunning(cfg) {
		return nil
	}
	if !sameFilePath(dbPath, cfg.DBPath) {
		return nil
	}
	return fmt.Errorf("the Pad server appears to be running at %s:%d and is serving %s — stop it first "+
		"('pad server stop') so the rows cannot change under the repair, or re-run with --force to override",
		cfg.Host, cfg.Port, cfg.DBPath)
}

// sameFilePath reports whether two paths name the same file.
func sameFilePath(a, b string) bool {
	return resolvePathForCompare(a) == resolvePathForCompare(b)
}

func resolvePathForCompare(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}

// nulRepairExitError turns a repair report into the command's exit status.
//
// BOTH failure buckets count. The first version printed suspect failures and
// then returned nil, so a repair that left data unrepaired exited 0 — invisible
// to any script, and to an operator who trusts the status (codex round 5).
//
// Extracted so the decision is testable without a database: the bug was in the
// decision, not in the repair, and a test that needed a fixture to reach it is
// a test nobody writes.
func nulRepairExitError(report *store.NULRepairReport) error {
	n := len(report.Failed) + len(report.SuspectsFailed)
	if n == 0 {
		return nil
	}
	return fmt.Errorf("%d value(s) could not be repaired", n)
}
