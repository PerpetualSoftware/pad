package main

import (
	"database/sql"
	"errors"
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
	if err := preflightNULForMigration(s, dbPath); err != nil {
		t.Fatalf("preflight refused a clean database: %v", err)
	}

	plantNULInWorkspaceName(t, dbPath, ws.ID, "bad"+textguard.NUL+"name")

	err = preflightNULForMigration(s, dbPath)
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

// TestPreflightRefusesTheShadowedNULAndPassesTheLiteral covers the two values
// the deleted SUSPECT class was built around, end to end through the preflight
// (BUG-3220). It used to need a PostgreSQL destination, because the preflight
// asked it about each suspect. Since BUG-2812 the scan itself sees the shadowed
// NUL, so neither leg needs one.
//
// Both legs matter and they are opposites. The literal leg is the over-refusal
// control: a preflight that refused every pre-filter match would block
// migrations over prose that merely writes about this bug, and would pass the
// refusal leg while doing it.
func TestPreflightRefusesTheShadowedNULAndPassesTheLiteral(t *testing.T) {
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

		ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Preflight"})
		if err != nil {
			t.Fatalf("create workspace: %v", err)
		}
		plantWorkspaceSettings(t, dbPath, ws.ID, blob)
		return s, dbPath
	}

	t.Run("a NUL behind a repeated key refuses the migration", func(t *testing.T) {
		src, path := newSource(t, `{"a":"`+esc+`","a":"clean"}`)

		scan, err := src.ScanNUL()
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if scan.Total() != 1 {
			t.Fatalf("expected the shadowed NUL as one violation, got %v", scan.Violations)
		}

		err = preflightNULForMigration(src, path)
		if err == nil {
			t.Fatal("the preflight accepted a value PostgreSQL refuses")
		}
		if !strings.Contains(err.Error(), "nothing was migrated") {
			t.Errorf("the refusal does not say the migration did not start: %v", err)
		}
		// THE COUNT, not just the phrase: an earlier version shipped a refusal
		// whose message counted a different set than its listing.
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
		if scan.Total() != 0 {
			t.Fatalf("the literal is reported as a violation: %v", scan.Violations)
		}
		if err := preflightNULForMigration(src, path); err != nil {
			t.Fatalf("the preflight refused a value PostgreSQL accepts: %v", err)
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

// TestRepairExitStatusCountsBothFailureBuckets pins the exit code.
//
// A repair that leaves data unrepaired and exits 0 is invisible: a script sees
// success, and an operator who trusts the status moves on (codex round 5).
func TestRepairExitStatusCountsFailures(t *testing.T) {
	boom := errors.New("nope")

	cases := []struct {
		name    string
		report  store.NULRepairReport
		wantErr bool
		why     string
	}{
		{
			name:    "nothing failed",
			report:  store.NULRepairReport{Repaired: []store.NULViolation{{Table: "items"}}},
			wantErr: false,
			why:     "the control: a clean run must not report failure.",
		},
		{
			name: "a violation failed",
			report: store.NULRepairReport{
				Failed: []store.NULRepairFailure{{Err: boom}},
			},
			wantErr: true,
			why:     "a failed repair must exit non-zero.",
		},
		{
			name: "skips are not failures",
			report: store.NULRepairReport{
				Skipped: []store.NULRepairSkip{{Reason: "primary key"}},
			},
			wantErr: false,
			why: "a deliberate skip is a reported outcome, not an error; exiting non-zero on it would " +
				"make every run with an email_optouts row look broken.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := nulRepairExitError(&tc.report)
			if (err != nil) != tc.wantErr {
				t.Errorf("%s\n  got err=%v, want error=%v", tc.why, err, tc.wantErr)
			}
		})
	}
}

// TestPreflightIgnoresTablesTheMigrationDoesNotCopy is codex round 9's second
// finding.
//
// `migrate-to-pg` copies workspace content and nothing else — its own help says
// users, platform settings and auth data are not migrated. A NUL in one of
// those tables therefore cannot break the copy, and refusing on it demanded the
// operator rewrite content unrelated to the migration they asked for.
//
// The row is still REPORTED. Staying silent about a broken row because this
// command does not care about it would be the same information-discarding this
// preflight already had to be corrected for once.
func TestPreflightIgnoresTablesTheMigrationDoesNotCopy(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "src.db")
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer s.Close()

	// platform_settings.value is protected and is NOT one of the six tables
	// ImportWorkspace writes.
	plantPlatformSetting(t, dbPath, "branding", "site"+textguard.NUL+"name")

	scan, err := s.ScanNUL()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// The premise: the SCAN does see it. If it did not, this test would pass
	// for the wrong reason.
	if scan.Total() != 1 {
		t.Fatalf("the scan should still report the row; got %d violations: %v", scan.Total(), scan.Violations)
	}
	if store.MigratedTables()["platform_settings"] {
		t.Fatal("platform_settings is listed as migrated; pick a table the migration really skips")
	}

	if err := preflightNULForMigration(s, dbPath); err != nil {
		t.Errorf("the preflight blocked a migration over a table it does not copy: %v", err)
	}

	// CONTROL: the same value in a table the migration DOES copy must still
	// refuse. Without this, a preflight that refused nothing would pass above.
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Blocking"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	plantNULInWorkspaceName(t, dbPath, ws.ID, "bad"+textguard.NUL+"name")

	if err := preflightNULForMigration(s, dbPath); err == nil {
		t.Error("the preflight accepted a NUL in a table the migration DOES copy")
	}
}

// plantPlatformSetting writes a settings row through a raw handle with the
// relevant triggers dropped.
func plantPlatformSetting(t *testing.T, dbPath, key, value string) {
	t.Helper()

	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()

	rows, err := raw.Query(
		`SELECT name FROM sqlite_master WHERE type='trigger' AND name GLOB 'pad_nul_platform_settings_*'`)
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
		t.Fatal("no platform_settings triggers found; the fixture would prove nothing")
	}
	for _, n := range names {
		if _, err := raw.Exec(`DROP TRIGGER IF EXISTS "` + n + `"`); err != nil {
			t.Fatalf("drop %s: %v", n, err)
		}
	}
	if _, err := raw.Exec(
		`INSERT INTO platform_settings (key, value, updated_at) VALUES (?, ?, datetime('now'))`,
		key, value); err != nil {
		t.Fatalf("plant: %v", err)
	}
}

// TestPreflightRefusesInvalidUTF8 is BUG-3222: PostgreSQL refuses a value that
// is not valid UTF-8 (SQLSTATE 22021), so the preflight refuses on one in a
// table the migration copies, and reports without blocking one in a table it
// does not.
//
// The last leg is the case BUG-3220 had to keep refused when it deleted the
// NUL suspect path: an invalid byte in a JSON value that ALSO carries the
// escape text as a harmless literal. Before BUG-3222 only the destination cast
// on suspects caught it, by coincidence. BUG-3222 (c31fb06c) made it an
// ordinary violation, so with that cast gone this preflight still refuses it
// and asks no destination.
func TestPreflightRefusesInvalidUTF8(t *testing.T) {
	bad := string([]byte{0x80})
	backslash := textguard.EscNUL[:1]

	newSource := func(t *testing.T) (*store.Store, string, string) {
		t.Helper()
		dbPath := filepath.Join(t.TempDir(), "src.db")
		s, err := store.New(dbPath)
		if err != nil {
			t.Fatalf("open source: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "UTF8"})
		if err != nil {
			t.Fatalf("create workspace: %v", err)
		}
		return s, dbPath, ws.ID
	}

	t.Run("control: valid multibyte text passes", func(t *testing.T) {
		s, path, wsID := newSource(t)
		plantNULInWorkspaceName(t, path, wsID, "é"+string(rune(0x4E2D))+string(rune(0x1F600)))
		if err := preflightNULForMigration(s, path); err != nil {
			t.Fatalf("the preflight refused valid UTF-8: %v", err)
		}
	})

	t.Run("invalid UTF-8 in a migrated text column refuses", func(t *testing.T) {
		s, path, wsID := newSource(t)
		plantNULInWorkspaceName(t, path, wsID, "bad"+bad+"name")
		err := preflightNULForMigration(s, path)
		if err == nil || !strings.HasPrefix(err.Error(), "1 stored value") || !strings.Contains(err.Error(), "nothing was migrated") {
			t.Fatalf("want a refusal of exactly one value, got %v", err)
		}
	})

	t.Run("invalid UTF-8 in a table the migration does not copy is not blocking", func(t *testing.T) {
		s, path, _ := newSource(t)
		plantPlatformSetting(t, path, "branding", "site"+bad+"name")
		scan, err := s.ScanNUL()
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if scan.Total() != 1 {
			t.Fatalf("the scan should report the row; got %v", scan.Violations)
		}
		if err := preflightNULForMigration(s, path); err != nil {
			t.Errorf("the preflight blocked on a table it does not copy: %v", err)
		}
	})

	t.Run("the BUG-3220 sliver: invalid byte plus the escape as a literal, in JSON", func(t *testing.T) {
		s, path, wsID := newSource(t)
		plantWorkspaceSettings(t, path, wsID, `{"a":"`+bad+`","b":"x`+backslash+textguard.EscNUL+`y"}`)
		scan, err := s.ScanNUL()
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if scan.Total() != 1 || !scan.Violations[0].InvalidUTF8 {
			t.Fatalf("want one invalid-UTF-8 violation, got %v", scan.Violations)
		}
		if err := preflightNULForMigration(s, path); err == nil {
			t.Fatal("the preflight let the sliver through")
		}
	})
}
