package main

import (
	"database/sql"
	"errors"
	"io"
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

// TestRepairExitStatusCountsBothFailureBuckets pins the exit code.
//
// A repair that leaves data unrepaired and exits 0 is invisible: a script sees
// success, and an operator who trusts the status moves on. The first version
// checked only the violation bucket, so a failed SUSPECT repair — the shape
// that needs the most attention, since no other check sees those values —
// exited cleanly (codex round 5).
func TestRepairExitStatusCountsBothFailureBuckets(t *testing.T) {
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
			why:     "the case the first version did catch.",
		},
		{
			name: "only a SUSPECT failed",
			report: store.NULRepairReport{
				SuspectsFailed: []store.NULSuspectFailure{{Err: boom}},
			},
			wantErr: true,
			why:     "the case it did not. These are the values no other check in Pad can see.",
		},
		{
			name: "skips are not failures",
			report: store.NULRepairReport{
				Skipped:       []store.NULRepairSkip{{Reason: "primary key"}},
				SuspectsClean: []store.NULSuspect{{Table: "items"}},
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

	if err := preflightNULForMigration(s, nil, dbPath); err != nil {
		t.Errorf("the preflight blocked a migration over a table it does not copy: %v", err)
	}

	// CONTROL: the same value in a table the migration DOES copy must still
	// refuse. Without this, a preflight that refused nothing would pass above.
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Blocking"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	plantNULInWorkspaceName(t, dbPath, ws.ID, "bad"+textguard.NUL+"name")

	if err := preflightNULForMigration(s, nil, dbPath); err == nil {
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

// TestPreflightDoesNotFailClosedOnUnmigratedSuspects is codex round 10, and it
// is round 9's over-refusal reintroduced through the other path.
//
// The fail-closed rule refuses when a suspect cannot be VERIFIED. Applied
// before the table filter, an unverifiable suspect in a table the migration
// never copies blocked the copy.
//
// It needs a REAL DESTINATION: the fail-closed branch only runs once there is
// something to ask, so a nil-destination fixture passes whether or not the
// ordering is right. The first version of this test made exactly that mistake
// and proved nothing.
//
// "Unverifiable" here is a NULL primary key — SQLite permits one in a declared
// TEXT PRIMARY KEY, which no other engine does — so the row's value genuinely
// cannot be read back to be cast.
func TestPreflightDoesNotFailClosedOnUnmigratedSuspects(t *testing.T) {
	dsn := os.Getenv("PAD_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("the fail-closed branch needs a real destination to be reachable at all")
	}

	dbPath := filepath.Join(t.TempDir(), "src.db")
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer s.Close()

	dst, err := store.NewPostgres(dsn)
	if err != nil {
		t.Fatalf("open destination: %v", err)
	}
	defer dst.Close()

	// activities.metadata is JSON-classed (so the escape makes it a SUSPECT
	// rather than a violation) and its table is NOT migrated.
	plantActivitySuspect(t, dbPath, `{"a":"`+textguard.EscNUL+`","a":"clean"}`)

	scan, err := s.ScanNUL()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scan.Total() != 0 {
		t.Fatalf("the fixture should be a suspect, not a violation: %v", scan.Violations)
	}
	// The premise, asserted so the test cannot pass because the fixture stopped
	// being unverifiable: the scan sees the row and cannot address it.
	if len(scan.Suspects) != 1 {
		t.Fatalf("expected one suspect, got %d", len(scan.Suspects))
	}
	if !scan.Suspects[0].KeyIncomplete {
		t.Fatalf("the fixture is addressable, so it would be verified rather than failing closed: %v",
			scan.Suspects[0].Key)
	}

	// The preflight writes its notes to stderr; capture them so the advisory
	// below is asserted rather than assumed.
	stderr := os.Stderr
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("pipe: %v", perr)
	}
	os.Stderr = w
	err = preflightNULForMigration(s, dst, dbPath)
	w.Close()
	os.Stderr = stderr
	out, _ := io.ReadAll(r)

	if err != nil {
		t.Errorf("the preflight failed closed over an unverifiable suspect in a table the migration "+
			"does not copy: %v", err)
	}

	// AND it says so. Excluding the row from the destination probe must not
	// turn into dropping it from the output: a comment that claims these are
	// reported while the code goes quiet is exactly what the first version of
	// this filter shipped (codex round 11).
	if !strings.Contains(string(out), "does not copy") {
		t.Errorf("the suspect was filtered out of the check AND out of the report; an operator sees "+
			"nothing about it. stderr was:\n%s", out)
	}
}

// plantActivitySuspect writes a suspect value into a non-migrated table.
func plantActivitySuspect(t *testing.T, dbPath, value string) {
	t.Helper()

	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()

	if store.MigratedTables()["activities"] {
		t.Fatal("activities is listed as migrated; pick a table the migration really skips")
	}
	// The triggers have to go first, and that is itself worth recording: Layer B
	// REFUSES this value. SQLite's json_tree walks tokens rather than building a
	// map, so it sees the NUL in the shadowed member that our Go predicate
	// cannot — the database is stricter than the shared predicate for exactly
	// this shape. Such a row can therefore only be LEGACY data, written before
	// the triggers existed, which is precisely the population BUG-2810 is about.
	rows, err := raw.Query(
		`SELECT name FROM sqlite_master WHERE type='trigger' AND name GLOB 'pad_nul_activities_metadata_*'`)
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
		t.Fatal("no activities.metadata triggers found; the fixture would prove nothing")
	}
	for _, n := range names {
		if _, derr := raw.Exec(`DROP TRIGGER IF EXISTS "` + n + `"`); derr != nil {
			t.Fatalf("drop %s: %v", n, derr)
		}
	}

	// id NULL, deliberately: SQLite permits a NULL in a declared TEXT PRIMARY
	// KEY, which is what makes this row unaddressable and therefore
	// unverifiable. workspace_id is omitted too — an instance-wide row.
	if _, err := raw.Exec(
		`INSERT INTO activities (id, action, actor, source, metadata, created_at)
		 VALUES (NULL, 'created', 'agent', 'cli', ?, datetime('now'))`, value); err != nil {
		t.Fatalf("plant: %v", err)
	}
}
