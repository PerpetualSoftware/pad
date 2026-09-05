package store

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2710. Two collections in one workspace could declare the same kernel
// trait, and with both live the resolver returned whichever the result order
// put first — arbitrary rather than tie-broken, and measured FLIPPING between
// runs on Postgres. These lock the de-duplication that makes the partial
// unique indexes creatable, and the rule that decides which one survives.

// duplicateTraitFixture reproduces the state from the task body: seed a
// template, rename the conventions collection (which re-slugs it), seed again
// so a fresh `conventions` appears, and both now declare artifact_kind =
// convention. When userWrote is true the user writes a convention into the
// RENAMED collection first, which is the only thing that distinguishes them.
func duplicateTraitFixture(t *testing.T, s *Store, name string, userWrote bool) (ws *models.Workspace, renamedID, dupID string) {
	t.Helper()
	ws = createTestWorkspace(t, s, name)
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	conv, err := s.GetCollectionBySlug(ws.ID, "conventions")
	if err != nil || conv == nil {
		t.Fatalf("get conventions: %v (nil=%v)", err, conv == nil)
	}
	newName := "House Rules"
	if _, err := s.UpdateCollection(conv.ID, models.CollectionUpdate{Name: &newName}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if userWrote {
		if _, err := s.CreateItem(ws.ID, conv.ID, models.ItemCreate{
			Title: "Our actual house rule", CreatedBy: "user", Source: "web",
		}); err != nil {
			t.Fatalf("user item: %v", err)
		}
	}
	// The duplicate is now UNREPRESENTABLE through ordinary code: the partial
	// unique index refuses it, and SeedCollectionsFromTemplate skips a
	// definition whose kind is already declared rather than colliding with it.
	// That is the fix working. It also means this fixture can only be built
	// the way the state actually exists in the wild — as legacy data written
	// before the index — so the declaration is copied onto a fresh collection
	// with enforcement suspended for that one statement.
	dup, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Conventions", Prefix: "CONV2"})
	if err != nil {
		t.Fatalf("create the would-be reseed: %v", err)
	}
	restore := s.SuspendTraitUniquenessForTesting()
	t.Cleanup(func() {
		// After de-duplication the workspace holds one declaration, so the
		// indexes must be creatable again — which is precisely the precondition
		// the migration needs, asserted here rather than assumed.
		if err := restore(); err != nil {
			t.Errorf("the indexes could not be recreated after de-duplication, so the migration would fail on this state: %v", err)
		}
	})
	if _, err := s.db.Exec(s.q(`UPDATE collections SET traits = ? WHERE id = ?`), conv.Traits, dup.ID); err != nil {
		t.Fatalf("plant the duplicate declaration: %v", err)
	}
	{
		// The reseed would have populated the duplicate with template rows, and
		// the bare reproduction ties on item count — that tie is what makes it
		// the arbitrary case. Give the planted duplicate the same count in both
		// fixtures, so the ONLY difference between them is the user-written
		// item, which is the thing under test.
		var n int
		if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM items WHERE collection_id = ? AND deleted_at IS NULL`), conv.ID).Scan(&n); err != nil {
			t.Fatalf("count original items: %v", err)
		}
		for i := 0; i < n; i++ {
			if _, err := s.CreateItem(ws.ID, dup.ID, models.ItemCreate{
				Title: "Template rule", CreatedBy: "system", Source: "template",
			}); err != nil {
				t.Fatalf("seed template item into the duplicate: %v", err)
			}
		}
	}
	return ws, conv.ID, dup.ID
}

func declaringConvention(t *testing.T, s *Store, workspaceID string) []string {
	t.Helper()
	traited, err := s.ListTraitedCollections(workspaceID)
	if err != nil {
		t.Fatalf("ListTraitedCollections: %v", err)
	}
	var out []string
	for _, tc := range traited {
		if tc.Traits.ArtifactKind != nil && tc.Traits.ArtifactKind.Kind == "convention" {
			out = append(out, tc.Slug)
		}
	}
	return out
}

// TestDedupeKeepsTheCollectionTheUserWroteIn is the fixture that MUST be
// decisive: the renamed collection holds a convention the user wrote, the
// accidental reseed holds only template rows, so the user's collection keeps
// the declaration every time. Measured 8/8 before this shipped; the point of
// the test is that it is not 7/8.
func TestDedupeKeepsTheCollectionTheUserWroteIn(t *testing.T) {
	s := testStore(t)
	ws, _, dupID := duplicateTraitFixture(t, s, "Dedupe User Wrote", true)

	// Control: the fixture really did produce the duplicate state, otherwise
	// the assertion below passes without exercising anything.
	if got := declaringConvention(t, s, ws.ID); len(got) != 2 {
		t.Fatalf("control leg failed: %d collections declare convention, want 2 (%v)", len(got), got)
	}

	if err := s.dedupeTraitDeclarations(); err != nil {
		t.Fatalf("dedupeTraitDeclarations: %v", err)
	}

	got := declaringConvention(t, s, ws.ID)
	if len(got) != 1 {
		t.Fatalf("after de-dup %d collections declare convention, want 1 (%v)", len(got), got)
	}
	if got[0] != "house-rules" {
		t.Errorf("declaration survived on %q, want house-rules — the collection the user actually wrote in", got[0])
	}

	// The loser keeps every item; it loses only the declaration.
	var loserItems int
	if err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM items WHERE collection_id = ? AND deleted_at IS NULL`), dupID).Scan(&loserItems); err != nil {
		t.Fatalf("count loser items: %v", err)
	}
	if loserItems == 0 {
		t.Error("the losing collection lost its items; the ruling is that it keeps them and loses only the trait")
	}
}

// TestDedupeReportsAnArbitraryTieAsArbitrary is the bare reproduction: nobody
// wrote anything, so both collections hold four identical template rows and
// tie on every orderable key. No rule can prefer either — measured as a 4/4
// coin flip — so the requirement is not WHICH one survives but that exactly
// one does and that the log says the choice was arbitrary rather than dressing
// it up as age.
func TestDedupeReportsAnArbitraryTieAsArbitrary(t *testing.T) {
	s := testStore(t)
	ws, _, _ := duplicateTraitFixture(t, s, "Dedupe Arbitrary Tie", false)
	if got := declaringConvention(t, s, ws.ID); len(got) != 2 {
		t.Fatalf("control leg failed: %d collections declare convention, want 2 (%v)", len(got), got)
	}

	var captured []slog.Record
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(&recordCapturingHandler{records: &captured}))
	err := s.dedupeTraitDeclarations()
	slog.SetDefault(prev)
	if err != nil {
		t.Fatalf("dedupeTraitDeclarations: %v", err)
	}

	if got := declaringConvention(t, s, ws.ID); len(got) != 1 {
		t.Fatalf("after de-dup %d collections declare convention, want exactly 1 (%v)", len(got), got)
	}

	var sawArbitrary, sawWinner, sawLoser bool
	for _, r := range captured {
		if r.Level != slog.LevelWarn || !strings.Contains(r.Message, "trait de-duplication") {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "decided_by":
				if strings.Contains(a.Value.String(), "arbitrary") &&
					strings.Contains(a.Value.String(), "NOT an age") {
					sawArbitrary = true
				}
			case "winner":
				sawWinner = true
			case "loser":
				sawLoser = true
			}
			return true
		})
	}
	if !sawArbitrary {
		t.Error("the tie was not reported as arbitrary; an operator reading this log cannot tell a considered resolution from a coin flip")
	}
	if !sawWinner || !sawLoser {
		t.Error("the report does not name both the winner and the loser")
	}
}

// TestTraitUniquenessIsEnforcedByTheDatabase locks the half the indexes own:
// once the duplicates are gone, a second declaration cannot be written at all.
// Raw SQL on purpose — the API gate refuses this too, so going through it
// would assert that the gate works rather than that the constraint does.
func TestTraitUniquenessIsEnforcedByTheDatabase(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Trait Uniqueness")
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	conv, err := s.GetCollectionBySlug(ws.ID, "conventions")
	if err != nil || conv == nil {
		t.Fatalf("get conventions: %v", err)
	}

	// Control: a collection with NO declaration inserts fine through the same
	// statement shape, so a failure below is the constraint and not the SQL.
	plain, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Plain", Prefix: "PLN"})
	if err != nil {
		t.Fatalf("control leg failed: a trait-free collection was rejected: %v", err)
	}

	if _, err := s.db.Exec(s.q(`UPDATE collections SET traits = ? WHERE id = ?`), conv.Traits, plain.ID); err == nil {
		t.Fatal("a second collection took the convention declaration; the unique index is not in force")
	} else {
		t.Logf("second declaration refused: %v", err)
	}
}
