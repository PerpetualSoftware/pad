package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2873. `models.FieldDef.Collection` holds the target's SLUG — it is the
// only pointer a relation field has, with no id alongside it — and renaming a
// collection re-slugifies it. Nothing migrated the relation definitions aimed
// at it, so every relation field pointing at a renamed collection was stranded:
// its picker filters on a slug that names nothing.
//
// These are written BEFORE the fix (team CONVE-29). Against the unfixed tree
// the propagation tests fail and the "leaves other fields alone" control
// passes — a pin that passes on both sides would not be measuring anything.

// relationSchema builds a one-relation-field schema aimed at `target`.
func relationSchema(t *testing.T, key, target string) string {
	t.Helper()
	b, err := json.Marshal(models.CollectionSchema{
		Fields: []models.FieldDef{
			{Key: "note", Label: "Note", Type: "text"},
			{Key: key, Label: "Rel", Type: "relation", Collection: target},
		},
	})
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return string(b)
}

// relationTargetOf reads back the stored target slug for a relation field.
func relationTargetOf(t *testing.T, s *Store, collID, key string) string {
	t.Helper()
	c, err := s.GetCollection(collID)
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}
	if c == nil {
		t.Fatalf("collection %s missing", collID)
	}
	var sch models.CollectionSchema
	if err := json.Unmarshal([]byte(c.Schema), &sch); err != nil {
		t.Fatalf("unmarshal schema %q: %v", c.Schema, err)
	}
	for _, f := range sch.Fields {
		if f.Key == key {
			return f.Collection
		}
	}
	t.Fatalf("field %q not found in %s", key, c.Slug)
	return ""
}

func TestRenameMigratesRelationTargetsInSiblingCollections(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "RelRenameSibling")

	target, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Colors", Slug: "colors"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	cars, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Cars", Slug: "cars", Schema: relationSchema(t, "color", "colors"),
	})
	if err != nil {
		t.Fatalf("create sibling: %v", err)
	}

	name := "Palette"
	renamed, err := s.UpdateCollection(target.ID, models.CollectionUpdate{Name: &name})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.Slug == "colors" {
		t.Fatalf("precondition: rename did not change the slug (got %q)", renamed.Slug)
	}

	if got := relationTargetOf(t, s, cars.ID, "color"); got != renamed.Slug {
		t.Errorf("sibling relation target = %q, want the new slug %q", got, renamed.Slug)
	}
}

func TestRenameMigratesASelfReferencingRelation(t *testing.T) {
	// The renamed collection is also the row being UPDATEd, so its own schema
	// rewrite has to compose with the update rather than be clobbered by it.
	s := testStore(t)
	ws := createTestWorkspace(t, s, "RelRenameSelf")

	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Nodes", Slug: "nodes", Schema: relationSchema(t, "parent_node", "nodes"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	name := "Graph Nodes"
	renamed, err := s.UpdateCollection(coll.ID, models.CollectionUpdate{Name: &name})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.Slug == "nodes" {
		t.Fatalf("precondition: rename did not change the slug")
	}

	if got := relationTargetOf(t, s, coll.ID, "parent_node"); got != renamed.Slug {
		t.Errorf("self-referencing target = %q, want %q", got, renamed.Slug)
	}
}

func TestRenameWithASimultaneousSchemaWriteMigratesTheSuppliedSchema(t *testing.T) {
	// A caller that renames AND supplies a new schema in one call: the
	// migration must apply to what the caller sent, not to what was stored,
	// or the rewrite silently reverts their edit.
	s := testStore(t)
	ws := createTestWorkspace(t, s, "RelRenameWithSchema")

	target, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Colors", Slug: "colors"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	_ = target

	self, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Cars", Slug: "cars", Schema: relationSchema(t, "color", "colors"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Rename `cars` while ALSO adding a field. Its own relation points at
	// `colors`, which is untouched — so the caller's schema must survive whole.
	newSchema := `{"fields":[{"key":"note","label":"Note","type":"text"},{"key":"color","label":"Rel","type":"relation","collection":"colors"},{"key":"vin","label":"VIN","type":"text"}]}`
	name := "Vehicles"
	if _, err := s.UpdateCollection(self.ID, models.CollectionUpdate{Name: &name, Schema: &newSchema}); err != nil {
		t.Fatalf("rename+schema: %v", err)
	}

	c, err := s.GetCollection(self.ID)
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}
	var sch models.CollectionSchema
	if err := json.Unmarshal([]byte(c.Schema), &sch); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(sch.Fields) != 3 {
		t.Fatalf("caller's schema was not preserved: got %d fields, want 3 (%s)", len(sch.Fields), c.Schema)
	}
	if got := relationTargetOf(t, s, self.ID, "color"); got != "colors" {
		t.Errorf("untouched target rewritten to %q, want %q", got, "colors")
	}
}

func TestRenameLeavesUnrelatedFieldsAndCollectionsAlone(t *testing.T) {
	// CONTROL. Without this the propagation tests are satisfied by a change
	// that rewrites the string everywhere it appears.
	s := testStore(t)
	ws := createTestWorkspace(t, s, "RelRenameControl")

	if _, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Colors", Slug: "colors"}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	// A relation aimed somewhere ELSE, plus a non-relation field whose VALUE
	// happens to be the renamed slug.
	other, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Other", Slug: "other",
		Schema: `{"fields":[{"key":"rel","label":"Rel","type":"relation","collection":"other"},{"key":"label","label":"Label","type":"text","default":"colors"},{"key":"pick","label":"Pick","type":"select","options":["colors","x"]}]}`,
	})
	if err != nil {
		t.Fatalf("create other: %v", err)
	}

	name := "Palette"
	// Rename `colors`, which `other` does NOT point at.
	colors, err := s.GetCollectionBySlug(ws.ID, "colors")
	if err != nil || colors == nil {
		t.Fatalf("lookup colors: %v", err)
	}
	if _, err := s.UpdateCollection(colors.ID, models.CollectionUpdate{Name: &name}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	c, err := s.GetCollection(other.ID)
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}
	var sch models.CollectionSchema
	if err := json.Unmarshal([]byte(c.Schema), &sch); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, f := range sch.Fields {
		switch f.Key {
		case "rel":
			if f.Collection != "other" {
				t.Errorf("unrelated relation target rewritten to %q, want %q", f.Collection, "other")
			}
		case "label":
			if fmt.Sprint(f.Default) != "colors" {
				t.Errorf("text default rewritten to %v, want %q", f.Default, "colors")
			}
		case "pick":
			if len(f.Options) == 0 || f.Options[0] != "colors" {
				t.Errorf("select option rewritten to %v, want first option %q", f.Options, "colors")
			}
		}
	}
}

func TestConcurrentRenamesOfMutuallyReferencingCollectionsDoNotDeadlock(t *testing.T) {
	// The hazard the existing lock-order comment does NOT cover: it orders the
	// workspace lock against ONE collection row lock, because nothing took two.
	// A sibling-schema rewrite takes many, so two renames of mutually
	// referencing collections can take them in opposite orders.
	s := testStore(t)
	ws := createTestWorkspace(t, s, "RelRenameDeadlock")

	a, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Alpha", Slug: "alpha", Schema: relationSchema(t, "beta_ref", "beta"),
	})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	b, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Beta", Slug: "beta", Schema: relationSchema(t, "alpha_ref", "alpha"),
	})
	if err != nil {
		t.Fatalf("create beta: %v", err)
	}

	// REPEATED, and released together. A single unsynchronised pair almost never
	// interleaves at the point that matters — measured: the first version of
	// this test passed with the serialization REMOVED, in 0.44s, because the two
	// goroutines simply never collided. A start barrier plus repetition is what
	// turns it into an instrument. Each round renames back and forth so the pair
	// keeps referencing each other.
	const rounds = 40
	ids := [2]string{a.ID, b.ID}
	for round := 0; round < rounds; round++ {
		start := make(chan struct{})
		done := make(chan error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(id string, n int) {
				defer wg.Done()
				name := fmt.Sprintf("Coll%d R%d", n, round)
				<-start
				_, err := s.UpdateCollection(id, models.CollectionUpdate{Name: &name})
				done <- err
			}(ids[i], i)
		}
		close(start)

		finished := make(chan struct{})
		go func() { wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(20 * time.Second):
			t.Fatalf("round %d: concurrent renames did not complete within 20s: deadlock", round)
		}
		close(done)
		for err := range done {
			if err != nil {
				t.Fatalf("round %d: concurrent rename failed: %v", round, err)
			}
		}
	}
}

func TestRenameAdvancesTheMigratedSiblingsConcurrencyToken(t *testing.T) {
	// codex round 1 P1. `collections.updated_at` doubles as the OCC token
	// (BUG-2265). Rewriting a sibling's schema without advancing it lets a
	// client still holding the PRE-rename schema — and a token that still
	// matches — write it straight back and undo the migration.
	s := testStore(t)
	ws := createTestWorkspace(t, s, "RelRenameToken")

	target, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Colors", Slug: "colors"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	cars, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Cars", Slug: "cars", Schema: relationSchema(t, "color", "colors"),
	})
	if err != nil {
		t.Fatalf("create sibling: %v", err)
	}
	before, err := s.GetCollection(cars.ID)
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}

	name := "Palette"
	if _, err := s.UpdateCollection(target.ID, models.CollectionUpdate{Name: &name}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	after, err := s.GetCollection(cars.ID)
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("migrated sibling token did not advance: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
	}

	// And the stale token is now refused, which is the property the advance buys.
	stale := before.UpdatedAt.Format(time.RFC3339)
	revert := relationSchema(t, "color", "colors")
	_, err = s.UpdateCollection(cars.ID, models.CollectionUpdate{
		Schema: &revert, ExpectedUpdatedAt: stale,
	})
	var conflict *CollectionUpdateConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("a write with the pre-rename token should 409, got err=%v", err)
	}
}

func TestRenamePreservesSchemaPropertiesItDoesNotUnderstand(t *testing.T) {
	// codex round 1 P2. Round-tripping through `models.CollectionSchema` drops
	// every property that struct does not declare, so a rename would silently
	// erase forward-compatible metadata from any relation-bearing schema.
	s := testStore(t)
	ws := createTestWorkspace(t, s, "RelRenameUnknownKeys")

	target, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Colors", Slug: "colors"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	exotic := `{"fields":[{"key":"color","label":"Rel","type":"relation","collection":"colors","future_prop":{"nested":[1,2]}}],"schema_level_extra":"keep me"}`
	cars, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Cars", Slug: "cars", Schema: exotic,
	})
	if err != nil {
		t.Fatalf("create sibling: %v", err)
	}

	name := "Palette"
	renamed, err := s.UpdateCollection(target.ID, models.CollectionUpdate{Name: &name})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}

	after, err := s.GetCollection(cars.ID)
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(after.Schema), &doc); err != nil {
		t.Fatalf("unmarshal rewritten schema: %v", err)
	}
	if doc["schema_level_extra"] != "keep me" {
		t.Errorf("schema-level unknown property lost: %s", after.Schema)
	}
	fields, _ := doc["fields"].([]interface{})
	if len(fields) != 1 {
		t.Fatalf("expected 1 field, got %s", after.Schema)
	}
	f, _ := fields[0].(map[string]interface{})
	if f["future_prop"] == nil {
		t.Errorf("field-level unknown property lost: %s", after.Schema)
	}
	if f["collection"] != renamed.Slug {
		t.Errorf("target not migrated: got %v want %q", f["collection"], renamed.Slug)
	}
}

func TestConcurrentRenamesOfTheSameCollectionLeaveRelationsPointingAtIt(t *testing.T) {
	// codex round 1 P1 (the third one). Both racers read the collection's slug
	// via the pre-transaction GetCollection. If the retarget uses THAT value
	// rather than the one re-read under the row lock, the loser migrates
	// `original -> its own new slug` while the relations already say the
	// WINNER's slug — so it matches nothing, and the relation is stranded at a
	// name no collection holds.
	//
	// The invariant is end-state and therefore honest under any interleaving:
	// whatever slug the collection ends up with, every relation aimed at it
	// points there.
	s := testStore(t)
	ws := createTestWorkspace(t, s, "RelRenameSameColl")

	target, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Colors", Slug: "colors"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	cars, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Cars", Slug: "cars", Schema: relationSchema(t, "color", "colors"),
	})
	if err != nil {
		t.Fatalf("create sibling: %v", err)
	}

	const rounds = 40
	for round := 0; round < rounds; round++ {
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				name := fmt.Sprintf("Palette %d-%d", round, n)
				<-start
				_, _ = s.UpdateCollection(target.ID, models.CollectionUpdate{Name: &name})
			}(i)
		}
		close(start)
		wg.Wait()

		final, err := s.GetCollection(target.ID)
		if err != nil {
			t.Fatalf("round %d: GetCollection: %v", round, err)
		}
		if got := relationTargetOf(t, s, cars.ID, "color"); got != final.Slug {
			t.Fatalf("round %d: relation points at %q but the collection is %q", round, got, final.Slug)
		}
	}
}

func TestRenameNeverRegressesAMigratedSiblingsToken(t *testing.T) {
	// codex round 2 P1. A sibling updated between the rename's timestamp and
	// the scan already holds a NEWER token; stamping the rename's value on it
	// moves the OCC token BACKWARDS, breaking the strictly-increasing invariant
	// and re-validating a token the client should have lost.
	s := testStore(t)
	ws := createTestWorkspace(t, s, "RelRenameNoRegress")

	target, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Colors", Slug: "colors"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	cars, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Cars", Slug: "cars", Schema: relationSchema(t, "color", "colors"),
	})
	if err != nil {
		t.Fatalf("create sibling: %v", err)
	}

	// Push the sibling's token well past anything this rename will compute.
	future := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := s.db.Exec(s.q("UPDATE collections SET updated_at = ? WHERE id = ?"), future, cars.ID); err != nil {
		t.Fatalf("seed future token: %v", err)
	}
	before, err := s.GetCollection(cars.ID)
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}

	name := "Palette"
	renamed, err := s.UpdateCollection(target.ID, models.CollectionUpdate{Name: &name})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}

	after, err := s.GetCollection(cars.ID)
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("token regressed or stalled: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
	}
	// The migration itself still has to have happened.
	if got := relationTargetOf(t, s, cars.ID, "color"); got != renamed.Slug {
		t.Errorf("target = %q, want %q", got, renamed.Slug)
	}
}

func TestRenamePreservesLargeIntegersInUnknownSchemaProperties(t *testing.T) {
	// codex round 2 P2. Decoding into `interface{}` turns every JSON number
	// into float64, so an integer past 2^53 comes back CHANGED and the rename
	// silently corrupts a property it was only meant to carry through.
	s := testStore(t)
	ws := createTestWorkspace(t, s, "RelRenameBigInt")

	target, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Colors", Slug: "colors"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	const big = "9007199254740993" // 2^53 + 1: not representable as float64
	cars, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Cars", Slug: "cars",
		Schema: `{"fields":[{"key":"color","label":"Rel","type":"relation","collection":"colors","future_id":` + big + `}]}`,
	})
	if err != nil {
		t.Fatalf("create sibling: %v", err)
	}

	name := "Palette"
	if _, err := s.UpdateCollection(target.ID, models.CollectionUpdate{Name: &name}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	after, err := s.GetCollection(cars.ID)
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}
	if !strings.Contains(after.Schema, big) {
		t.Errorf("large integer corrupted by the rewrite: %s", after.Schema)
	}
}
