package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// TestRepairNULHintNamesARealCommand is the guard the hint's own comment
// promises.
//
// Three surfaces quote this command at an operator — the migrate-to-pg
// preflight, the workspace import's strict refusal, and scan-nul's help — and
// each of them is a claim that typing it does something. Walking the real cobra
// tree is what makes a rename fail here rather than in front of a user, which
// is the difference between a cited convention and a consulted one.
func TestRepairNULHintNamesARealCommand(t *testing.T) {
	root := newRootCmd()

	// The hint is a full command line ("pad db repair-nul"); resolve it as a
	// path through the tree rather than by string comparison against a second
	// spelling, which would only prove two constants agree.
	fields := strings.Fields(repairNULCommandHint)
	if len(fields) < 2 || fields[0] != "pad" {
		t.Fatalf("the hint is not a 'pad ...' command line: %q", repairNULCommandHint)
	}
	cmd, _, err := root.Find(fields[1:])
	if err != nil {
		t.Fatalf("the hint names a command that does not exist: %q (%v)", repairNULCommandHint, err)
	}
	// Find falls back to the closest ancestor rather than failing, so the
	// resolved command must actually BE the leaf named — otherwise "pad db
	// repair-nonsense" resolves to "db" and passes.
	if cmd.Name() != fields[len(fields)-1] {
		t.Fatalf("the hint %q resolves to %q, not to a command of its own name",
			repairNULCommandHint, cmd.CommandPath())
	}
	if cmd.RunE == nil && cmd.Run == nil {
		t.Errorf("%q exists but does nothing when run", repairNULCommandHint)
	}

	// And the store's constant — which the SERVER quotes in the import
	// refusal — is the same string, so all three surfaces move together.
	if repairNULCommandHint != store.RepairNULCommand {
		t.Errorf("the CLI hint (%q) and the string the server quotes (%q) have drifted apart",
			repairNULCommandHint, store.RepairNULCommand)
	}
}

// TestMigrateToPgPreflightRefusesAndNamesTheRepair covers the preflight's whole
// contract: it refuses, it names the rows, it names the command, and — the part
// that matters most — it does so BEFORE anything has moved.
func TestMigrateToPgPreflightRefusesAndNamesTheRepair(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "preflight.db")
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Preflight"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	// CONTROL FIRST: a clean database passes the preflight. Without this, a
	// preflight that refused everything would satisfy the assertion below.
	//
	// A nil destination is the "no oracle" path: this leg is about the
	// VIOLATION half, which needs no Postgres, and the suspect half has its own
	// test that does. The function says so in its output rather than skipping
	// silently.
	if err := preflightNULForMigration(s, nil, dbPath); err != nil {
		t.Fatalf("preflight refused a clean database: %v", err)
	}

	plantNULInWorkspaceName(t, dbPath, ws.ID, "bad"+textguard.NUL+"name")

	err = preflightNULForMigration(s, nil, dbPath)
	if err == nil {
		t.Fatal("the preflight accepted a database carrying a value PostgreSQL will refuse — the migration " +
			"would fail partway through the copy, which is the failure this replaces")
	}
	if !strings.Contains(err.Error(), "nothing was migrated") {
		t.Errorf("the refusal does not say the migration did not start: %v", err)
	}
}

// TestScanAndRepairAgreeThroughTheCommandPath drives the store API the two
// commands call, so the CLI's promise — scan-nul is the dry run for
// repair-nul — is measured rather than asserted in help text.
func TestScanAndRepairAgreeThroughTheCommandPath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "scanrepair.db")
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "ScanRepair"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	plantNULInWorkspaceName(t, dbPath, ws.ID, "bad"+textguard.NUL+"name")

	scan, err := s.ScanNUL()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scan.Total() != 1 {
		t.Fatalf("scan found %d violations, want 1: %v", scan.Total(), scan.Violations)
	}

	report, err := s.RepairNUL()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	// The dry run's count and the repair's count are the same number, which is
	// the entire reason scan-nul is offered instead of a --dry-run flag.
	if len(report.Repaired) != scan.Total() {
		t.Errorf("scan promised %d change(s), repair made %d", scan.Total(), len(report.Repaired))
	}

	var name string
	if err := s.DB().QueryRow(`SELECT name FROM workspaces WHERE id = ?`, ws.ID).Scan(&name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if want := "bad" + textguard.Replacement + "name"; name != want {
		t.Errorf("repaired name = %q, want %q", name, want)
	}
}

// plantNULInWorkspaceName writes the legacy state through a raw handle with the
// NUL triggers dropped — which is what a pre-enforcement binary was.
func plantNULInWorkspaceName(t *testing.T, dbPath, wsID, value string) {
	t.Helper()

	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()

	rows, err := raw.Query(
		`SELECT name FROM sqlite_master WHERE type = 'trigger' AND name GLOB 'pad_nul_workspaces_name_*'`)
	if err != nil {
		t.Fatalf("list triggers: %v", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		names = append(names, n)
	}
	rows.Close()
	if len(names) == 0 {
		t.Fatal("no workspaces.name triggers found — the fixture would plant nothing and the test " +
			"would pass for the wrong reason")
	}
	for _, n := range names {
		if _, err := raw.Exec(`DROP TRIGGER IF EXISTS "` + n + `"`); err != nil {
			t.Fatalf("drop %s: %v", n, err)
		}
	}
	if _, err := raw.Exec(`UPDATE workspaces SET name = ? WHERE id = ?`, value, wsID); err != nil {
		t.Fatalf("plant: %v", err)
	}
}

// TestSameFilePathIdentifiesTheServersDatabase covers the comparison the
// running-server guard is built on.
//
// The guard used to be skipped whenever --from was given, which made it
// opt-out by accident: the most natural --from an operator types is the path
// `pad db scan-nul` just printed, which IS the live database. The fix compares
// resolved paths, so the cases that matter are the ones where two spellings
// name one file.
func TestSameFilePathIdentifiesTheServersDatabase(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "pad.db")
	if err := os.WriteFile(live, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	other := filepath.Join(dir, "backup.db")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "linked.db")
	if err := os.Symlink(live, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	cases := []struct {
		name string
		a, b string
		want bool
		why  string
	}{
		{"identical paths", live, live, true, "the plain case: --from naming the live database."},
		{"a symlink to it", link, live, true,
			"a symlinked data directory is the shape where two spellings name one file, and the one a " +
				"string comparison misses."},
		{"an unrelated file", other, live, false,
			"the control. A guard that answered true for everything would also pass every case above, " +
				"and would refuse repairing a backup for no reason."},
		{"a path with redundant segments", filepath.Join(dir, ".", "pad.db"), live, true,
			"Clean/Abs normalisation, so a path typed from a different working directory still matches."},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameFilePath(tc.a, tc.b); got != tc.want {
				t.Errorf("sameFilePath(%q, %q) = %v, want %v — %s", tc.a, tc.b, got, tc.want, tc.why)
			}
		})
	}
}

// TestPreflightAsksTheDestinationAboutSuspects is the day-54 ruling's whole
// point, end to end: the preflight refuses a row NO CHECK IN PAD CAN SEE,
// because it asks the database that is about to reject it.
//
// Both legs matter and they are opposites. The literal-only leg is the
// over-refusal control — a preflight that refused every suspect would block
// migrations over prose that merely writes about this bug, and would pass the
// refusal leg while doing it.
func TestPreflightAsksTheDestinationAboutSuspects(t *testing.T) {
	dsn := os.Getenv("PAD_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("the preflight's oracle needs a real PostgreSQL destination (set PAD_TEST_POSTGRES_URL)")
	}

	esc := textguard.EscNUL
	backslash := esc[:1]

	newSource := func(t *testing.T, blob string) (*store.Store, string) {
		t.Helper()
		dbPath := filepath.Join(t.TempDir(), "src.db")
		s, err := store.New(dbPath)
		if err != nil {
			t.Fatalf("open source: %v", err)
		}
		t.Cleanup(func() { s.Close() })

		ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Suspect"})
		if err != nil {
			t.Fatalf("create workspace: %v", err)
		}
		plantWorkspaceSettings(t, dbPath, ws.ID, blob)
		return s, dbPath
	}

	dst, err := store.NewPostgres(dsn)
	if err != nil {
		t.Fatalf("open destination: %v", err)
	}
	defer dst.Close()

	t.Run("a NUL behind a repeated key refuses the migration", func(t *testing.T) {
		src, path := newSource(t, `{"a":"`+esc+`","a":"clean"}`)

		// The premise, asserted so this cannot quietly become a case we catch
		// ourselves: our own scan finds NO violation here.
		scan, err := src.ScanNUL()
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if scan.Total() != 0 {
			t.Fatalf("the scan now reports this as a violation, so the oracle is not what refuses it: %v",
				scan.Violations)
		}
		if len(scan.Suspects) != 1 {
			t.Fatalf("expected exactly one suspect, got %d", len(scan.Suspects))
		}

		err = preflightNULForMigration(src, dst, path)
		if err == nil {
			t.Fatal("the preflight accepted a value PostgreSQL refuses — this is the row the ruling " +
				"exists for, and it is invisible to every check Pad makes")
		}
		if !strings.Contains(err.Error(), "nothing was migrated") {
			t.Errorf("the refusal does not say the migration did not start: %v", err)
		}
		// THE COUNT, not just the phrase. The first version of this assertion
		// read only the phrase, and the message shipped saying "0 stored
		// value(s) carry a NUL; nothing was migrated" — a refusal whose reason
		// says there was nothing to refuse, because it counted violations while
		// the listing counted violations plus refused suspects.
		if !strings.HasPrefix(err.Error(), "1 stored value") {
			t.Errorf("the refusal miscounts what it refused on: %v", err)
		}
	})

	t.Run("a harmless literal does NOT refuse the migration", func(t *testing.T) {
		src, path := newSource(t, `{"note":"x`+backslash+esc+`y"}`)

		scan, err := src.ScanNUL()
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(scan.Suspects) != 1 {
			t.Fatalf("expected the literal to be a suspect, got %d", len(scan.Suspects))
		}

		if err := preflightNULForMigration(src, dst, path); err != nil {
			t.Fatalf("the preflight refused a value PostgreSQL accepts, so every suspect would block a "+
				"migration: %v", err)
		}
	})
}

// plantWorkspaceSettings writes a settings blob through a raw handle with the
// relevant triggers dropped — the pre-enforcement binary again.
func plantWorkspaceSettings(t *testing.T, dbPath, wsID, blob string) {
	t.Helper()

	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()

	rows, err := raw.Query(
		`SELECT name FROM sqlite_master WHERE type='trigger' AND name GLOB 'pad_nul_workspaces_settings_*'`)
	if err != nil {
		t.Fatalf("list triggers: %v", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		names = append(names, n)
	}
	rows.Close()
	if len(names) == 0 {
		t.Fatal("no workspaces.settings triggers found; the fixture would prove nothing")
	}
	for _, n := range names {
		if _, err := raw.Exec(`DROP TRIGGER IF EXISTS "` + n + `"`); err != nil {
			t.Fatalf("drop %s: %v", n, err)
		}
	}
	if _, err := raw.Exec(`UPDATE workspaces SET settings = ? WHERE id = ?`, blob, wsID); err != nil {
		t.Fatalf("plant: %v", err)
	}
}
