package store

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2943, one test per door that can put a prefix on a collection. The bug
// was two implicit definitions of a valid prefix — the generator admitted any
// first byte, parseItemRef accepted only A-Z — disagreeing at READ time about
// an identifier the product itself minted. One definition now, so each door
// gets the same answer, and each door is asserted separately because "they all
// call the same helper" is a claim about the code rather than about behaviour.

// Door 2: create with an EXPLICIT prefix.
func TestCreateCollection_ExplicitPrefixMustBeResolvable(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Explicit Prefix Create")

	t.Run("a digit-bearing prefix is accepted and resolves", func(t *testing.T) {
		c, err := s.CreateCollection(ws.ID, models.CollectionCreate{
			Name: "Alpha One", Slug: "alpha-one", Prefix: "AB1",
		})
		if err != nil {
			t.Fatalf("AB1 is inside the grammar and must be accepted: %v", err)
		}
		if c.Prefix != "AB1" {
			t.Errorf("stored prefix = %q, want AB1 — an accepted prefix must not be rewritten", c.Prefix)
		}
		if _, _, ok := parseItemRef(c.Prefix + "-7"); !ok {
			t.Errorf("accepted prefix %q does not resolve — the two definitions have drifted again", c.Prefix)
		}
	})

	for _, bad := range []string{"ab1", "1AB", "A B", "A-B", "A!", "Ω", "0"} {
		t.Run("refused: "+bad, func(t *testing.T) {
			_, err := s.CreateCollection(ws.ID, models.CollectionCreate{
				Name: "Bad " + bad, Slug: "bad-" + strings.ToLower(strings.TrimSpace(bad)), Prefix: bad,
			})
			if err == nil {
				t.Fatalf("prefix %q is outside the grammar and must be refused", bad)
			}
			if !strings.Contains(err.Error(), "uppercase") {
				t.Errorf("refusal should name the rule; got %v", err)
			}
		})
	}
}

// Door 3: update. This one matters most — update is what someone reaches for
// to FIX a bad prefix, so accepting another bad one would trade one
// unresolvable id-space for the next.
func TestUpdateCollection_PrefixMustBeResolvable(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Prefix Update")

	c, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Things", Slug: "things-prefix-update",
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	good := "AB1"
	updated, err := s.UpdateCollection(c.ID, models.CollectionUpdate{Prefix: &good})
	if err != nil {
		t.Fatalf("AB1 must be accepted on update: %v", err)
	}
	if updated.Prefix != good {
		t.Errorf("prefix = %q, want %q", updated.Prefix, good)
	}

	bad := "ab1"
	if _, err := s.UpdateCollection(c.ID, models.CollectionUpdate{Prefix: &bad}); err == nil {
		t.Fatal("a lowercase prefix must be refused on update")
	}

	// ...and the refusal must not have written anything.
	after, err := s.GetCollection(c.ID)
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}
	if after.Prefix != good {
		t.Errorf("a refused update changed the stored prefix to %q", after.Prefix)
	}
}

// The parser half of the claim, stated as a round trip rather than as a
// restatement of the rule: what the doors accept, the parser resolves.
func TestParseItemRef_AcceptsTheGrammarTheDoorsEnforce(t *testing.T) {
	t.Parallel()

	for _, prefix := range []string{"TASK", "AB1", "A1", "ABCDE", "X9Z"} {
		gotPrefix, gotNum, ok := parseItemRef(prefix + "-42")
		if !ok {
			t.Errorf("parseItemRef(%q-42) refused a prefix the doors accept", prefix)
			continue
		}
		if gotPrefix != prefix || gotNum != 42 {
			t.Errorf("parseItemRef(%q-42) = (%q, %d), want (%q, 42)", prefix, gotPrefix, gotNum, prefix)
		}
	}

	// A prefix beginning with a digit stays refused, so PREFIX-NUMBER cannot
	// start with a digit and remains unambiguous to read.
	for _, ref := range []string{"1AB-42", "9-42", "A B-42", "A!-42", "AB1-", "-42", "AB1"} {
		if _, _, ok := parseItemRef(ref); ok {
			t.Errorf("parseItemRef(%q) accepted a malformed ref", ref)
		}
	}

	// A LOWERCASE ref still resolves, and that is pre-existing and deliberate
	// rather than a hole this widening opened: parseItemRef upper-cases its
	// input before splitting, which is what makes `pad item show task-5` work.
	// Pinned here because my first version of this test asserted the opposite
	// and was wrong about the code — a case-sensitivity rule is exactly the
	// kind of thing a later reader would "fix" from the grammar comment alone.
	gotPrefix, gotNum, ok := parseItemRef("ab1-42")
	if !ok || gotPrefix != "AB1" || gotNum != 42 {
		t.Errorf(`parseItemRef("ab1-42") = (%q, %d, %v), want ("AB1", 42, true) — refs are case-insensitive`,
			gotPrefix, gotNum, ok)
	}
}

// Door 4: import. The permissive door, because a refusal here has a cost the
// other doors do not: a workspace that already carries a bad prefix would fail
// to come back at all. So it accepts anything the parser resolves — which now
// includes digits — and refuses only what NO surface could resolve, since
// restoring items whose printed IDs answer "not found" is this bug, not a
// compatibility owed.
func TestImportWorkspace_PrefixMustBeResolvable(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	owner := createTestUser(t, s, "prefix-import-owner@test.com", "Prefix Owner", "password123")
	src := createTestWorkspace(t, s, "Prefix Import Source")
	if err := s.SeedCollectionsFromTemplate(src.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	export := func(t *testing.T) *models.WorkspaceExport {
		t.Helper()
		exp, err := s.ExportWorkspace(src.Slug)
		if err != nil {
			t.Fatalf("ExportWorkspace: %v", err)
		}
		return exp
	}

	t.Run("a digit-bearing prefix restores and its items resolve", func(t *testing.T) {
		exp := export(t)
		var patched bool
		for i := range exp.Collections {
			if exp.Collections[i].Slug == "tasks" {
				exp.Collections[i].Prefix = "TSK1"
				patched = true
			}
		}
		if !patched {
			t.Fatal("control leg failed: export carried no tasks collection to patch")
		}

		imported, err := s.ImportWorkspace(exp, "prefix-digit-target", owner.ID, "")
		if err != nil {
			t.Fatalf("a prefix the parser resolves must restore: %v", err)
		}
		got, err := s.GetCollectionBySlug(imported.ID, "tasks")
		if err != nil || got == nil {
			t.Fatalf("imported workspace has no tasks collection: %v", err)
		}
		if got.Prefix != "TSK1" {
			t.Errorf("stored prefix = %q, want TSK1 — a restorable prefix must not be rewritten", got.Prefix)
		}
		if _, _, ok := parseItemRef(got.Prefix + "-1"); !ok {
			t.Errorf("restored prefix %q does not resolve", got.Prefix)
		}
	})

	t.Run("an unresolvable prefix is refused, naming the collection", func(t *testing.T) {
		exp := export(t)
		for i := range exp.Collections {
			if exp.Collections[i].Slug == "tasks" {
				exp.Collections[i].Prefix = "T S K"
			}
		}
		_, err := s.ImportWorkspace(exp, "prefix-bad-target", owner.ID, "")
		if err == nil {
			t.Fatal("a prefix no surface can resolve must be refused rather than restored")
		}
		if !strings.Contains(err.Error(), "Tasks") {
			t.Errorf("refusal must name the collection so the operator can find it: %v", err)
		}
		if !strings.Contains(err.Error(), "export") {
			t.Errorf("refusal must say the export can be edited: %v", err)
		}
	})

	// An ABSENT prefix is not an unresolvable one — old bundles carry "", and
	// refusing those would turn a fix for unresolvable IDs into one that
	// cannot restore an old bundle at all. Three server tests caught exactly
	// that in the first version of this check.
	t.Run("an absent prefix takes the create-path fallback", func(t *testing.T) {
		exp := export(t)
		for i := range exp.Collections {
			exp.Collections[i].Prefix = ""
		}
		imported, err := s.ImportWorkspace(exp, "prefix-absent-target", owner.ID, "")
		if err != nil {
			t.Fatalf("an export with no prefixes must still restore: %v", err)
		}
		got, err := s.GetCollectionBySlug(imported.ID, "tasks")
		if err != nil || got == nil {
			t.Fatalf("imported workspace has no tasks collection: %v", err)
		}
		// PIN THE VALUE, not just resolvability (codex round 2 [P2]). Asserting
		// only "non-empty and parseable" passes an implementation that stamps
		// ITEM on every absent prefix, which is not the documented
		// derive-then-ITEM fallback and would give every collection in a
		// restored workspace the same id-space.
		if got.Prefix != "TASK" {
			t.Errorf("imported prefix = %q, want TASK — the fallback DERIVES from the name "+
				"before reaching for ITEM", got.Prefix)
		}
		if _, _, ok := parseItemRef(got.Prefix + "-1"); !ok {
			t.Errorf("the filled-in prefix %q does not resolve", got.Prefix)
		}
	})
}

// ...and the other half of the fallback: a collection whose NAME yields no
// letters falls through DerivePrefix's empty return to ITEM. Without this leg
// the assertion above is satisfied by an implementation that only derives and
// never reaches the fallback, so the two legs are what make the rule
// "derive, THEN ITEM" rather than either half alone.
func TestImportWorkspace_LetterlessNameFallsBackToITEM(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	owner := createTestUser(t, s, "letterless-owner@test.com", "Letterless Owner", "password123")
	src := createTestWorkspace(t, s, "Letterless Source")
	if _, err := s.CreateCollection(src.ID, models.CollectionCreate{
		Name: "2026", Slug: "twenty-twenty-six",
	}); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	exp, err := s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	for i := range exp.Collections {
		exp.Collections[i].Prefix = ""
	}

	imported, err := s.ImportWorkspace(exp, "letterless-target", owner.ID, "")
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}
	got, err := s.GetCollectionBySlug(imported.ID, "twenty-twenty-six")
	if err != nil || got == nil {
		t.Fatalf("imported workspace has no twenty-twenty-six collection: %v", err)
	}
	if got.Prefix != "ITEM" {
		t.Errorf("prefix for a letterless name = %q, want ITEM", got.Prefix)
	}
}

// The WARN the ruling asked for: a prefix accepted ONLY because the parser
// widened is visible to an operator, rather than being inferred from a resolve
// failure that no longer happens. Untested, this is a line of code nobody
// would notice was gone.
//
// Not t.Parallel: it swaps the process-wide default slog handler, which is
// exactly the shared global CONVE-2086 says disqualifies a test from running
// beside others.
func TestImportWorkspace_DigitBearingPrefixIsLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := testStore(t)
	owner := createTestUser(t, s, "warn-owner@test.com", "Warn Owner", "password123")
	src := createTestWorkspace(t, s, "Warn Source")
	if err := s.SeedCollectionsFromTemplate(src.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	exp, err := s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	for i := range exp.Collections {
		if exp.Collections[i].Slug == "tasks" {
			exp.Collections[i].Prefix = "TSK1"
		}
	}
	if _, err := s.ImportWorkspace(exp, "warn-target", owner.ID, ""); err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "digit-bearing prefix") {
		t.Errorf("no warning logged for a digit-bearing prefix:\n%s", logged)
	}
	if !strings.Contains(logged, "TSK1") {
		t.Errorf("the warning must name the prefix:\n%s", logged)
	}
	// The control: an ORDINARY prefix must NOT warn, or the log is noise and
	// an operator learns to ignore it.
	if strings.Count(logged, "digit-bearing prefix") != 1 {
		t.Errorf("exactly one collection had a digit-bearing prefix; got %d warnings:\n%s",
			strings.Count(logged, "digit-bearing prefix"), logged)
	}
}
