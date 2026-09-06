package store

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TASK-2900. TestUpdateDocument_CascadeExhaustionRollsBackTheWholeRename
// assigns the package-level cascadeRewriteAttempts and restores it in a defer.
// That is safe only while the test runs in the SEQUENTIAL phase, where no
// t.Parallel() test is in flight; adding t.Parallel() to it would make the
// mutation visible to every other cascade test, and the symptom would appear
// in THOSE tests rather than in this one.
//
// A comment saying "must not call t.Parallel()" protects nobody against the
// next sweep — this repository just ran one that touched 844 test functions.
// So the invariant is enforced rather than asserted.
//
// This guard reads SOURCE, which makes it an instrument with an adversary, so
// its own failure modes are worth stating:
//
//   - It anchors the identifier END (`\b`) so a future
//     `...RollsBackTheWholeRenameV2` is not silently matched by a prefix.
//   - It strips line comments before scanning, so the words "t.Parallel()"
//     inside the explanatory comment above the mutation cannot satisfy or
//     trip it. Verified by the fact that the comment above that line contains
//     the string and this test passes.
//   - It FAILS CLOSED: if the function or the file cannot be found, that is a
//     failure, not a pass. A guard that silently finds nothing to check is the
//     failure mode this whole class has.
func TestCascadeExhaustionStaysSerial(t *testing.T) {
	const file = "documents_cascade_lost_update_test.go"
	const fn = "TestUpdateDocument_CascadeExhaustionRollsBackTheWholeRename"

	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("guard cannot read %s: %v — failing closed rather than passing vacuously", file, err)
	}

	// Anchor the identifier's END so a renamed-with-suffix function does not
	// match this guard while escaping what it guards.
	start := regexp.MustCompile(`func ` + regexp.QuoteMeta(fn) + `\b`).FindIndex(src)
	if start == nil {
		t.Fatalf("guard cannot find %s in %s — the function was renamed or moved, so this guard is "+
			"no longer guarding anything. Re-point it or delete it deliberately; do not leave it green.", fn, file)
	}

	rest := string(src[start[1]:])
	// Bound the scan at the next top-level func so a later test's
	// t.Parallel() is not attributed to this one.
	if end := strings.Index(rest, "\nfunc "); end >= 0 {
		rest = rest[:end]
	}

	// Strip line comments: the mutation site carries an explanatory comment
	// that mentions t.Parallel() by name, and a guard that a comment can trip
	// is the comment-blindness failure inverted.
	var body strings.Builder
	for _, line := range strings.Split(rest, "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		body.WriteString(line)
		body.WriteString("\n")
	}

	if strings.Contains(body.String(), "t.Parallel()") {
		t.Fatalf("%s calls t.Parallel(), but it assigns the package-level cascadeRewriteAttempts. "+
			"That mutation would become visible to every concurrently-running test, and the flakes "+
			"would appear in the OTHER cascade tests rather than in this one. Either drop the "+
			"t.Parallel() or stop mutating the global.", fn)
	}

	// Positive control on the instrument itself: the same scan must FIND
	// t.Parallel() in a function that has it, or this guard proves nothing.
	ctl, err := os.ReadFile("collections_test.go")
	if err == nil && !strings.Contains(string(ctl), "t.Parallel()") {
		t.Fatal("control failed: the sweep should have added t.Parallel() to collections_test.go, " +
			"so a scan that cannot see it there cannot be trusted to see it above")
	}
}

// TestMigrationGuardTestsStaySerial is the same guard for the other file whose
// tests mutate package globals — store.AllowSchemaAhead and store.BinaryVersion,
// both EXPORTED, which is why my `^var [a-z]` sweep missed them and codex did
// not. It guards the whole FILE rather than named functions, because every test
// in it touches one of those two globals and a new one added there would too.
func TestMigrationGuardTestsStaySerial(t *testing.T) {
	const file = "migration_guard_test.go"

	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("guard cannot read %s: %v — failing closed rather than passing vacuously", file, err)
	}

	var body strings.Builder
	for _, line := range strings.Split(string(src), "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		body.WriteString(line)
		body.WriteString("\n")
	}

	if strings.Contains(body.String(), "t.Parallel()") {
		t.Fatalf("a test in %s calls t.Parallel(), but every test in that file mutates a package-level "+
			"global (AllowSchemaAhead / BinaryVersion) and restores it in a t.Cleanup. Under parallelism "+
			"that window is open across every concurrently-running test — AllowSchemaAhead=true would "+
			"disarm the schema-ahead guard for whoever else is running, which is the property two of "+
			"those tests exist to assert.", file)
	}

	// Same positive control as the guard above: the scan must be able to SEE a
	// real t.Parallel() somewhere, or its silence here means nothing.
	ctl, err := os.ReadFile("collections_test.go")
	if err == nil && !strings.Contains(string(ctl), "t.Parallel()") {
		t.Fatal("control failed: the sweep should have added t.Parallel() to collections_test.go, " +
			"so a scan that cannot see it there cannot be trusted to see it in migration_guard_test.go")
	}
}
