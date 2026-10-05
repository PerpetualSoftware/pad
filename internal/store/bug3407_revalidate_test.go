package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3407: an item write validated against a collection's OLD schema must not
// store a value its NEW schema forbids. The handler validates before the store
// takes the workspace seq lock; the store re-validates the keys the write sets
// against the schema read under that lock.

const bug3407Base = `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open"}]}`
const bug3407WithColor = `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open"},{"key":"color","label":"Color","type":"select","options":["red"]}]}`
const bug3407Unrelated = `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open"},{"key":"size","label":"Size","type":"text"}]}`

func bug3407Fixture(t *testing.T) (*Store, *models.Workspace, *models.Collection, *models.Item) {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Revalidate")
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Things", Slug: "things", Schema: bug3407Base})
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "One", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	return s, ws, coll, item
}

// schemaBytes is the collection's schema exactly as stored: what a handler
// reads and validates against, and passes as ValidatedSchema.
func schemaBytes(t *testing.T, s *Store, collID string) *string {
	t.Helper()
	var raw string
	if err := s.db.QueryRow(s.q(`SELECT schema FROM collections WHERE id = ?`), collID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return &raw
}

func setSchema(t *testing.T, s *Store, collID, schema string) {
	t.Helper()
	if _, err := s.UpdateCollection(collID, models.CollectionUpdate{Schema: &schema}); err != nil {
		t.Fatalf("update schema: %v", err)
	}
}

func wantValidation(t *testing.T, err error, what string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("%s: err = %v, want a ValidationError", what, err)
	}
}

func storedColor(t *testing.T, s *Store, id string) (any, bool) {
	t.Helper()
	it, err := s.GetItem(id)
	if err != nil || it == nil {
		t.Fatal(err)
	}
	m, err := decodeFieldsBlob(it.Fields)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := m["color"]
	return v, ok
}

// The reported race, on the update path: the schema change commits after the
// write's pre-lock read and before its lock.
func TestBug3407_APatchValidatedAgainstTheOldSchemaIsRefusedUnderTheLock(t *testing.T) {
	s, _, coll, item := bug3407Fixture(t)
	validated := schemaBytes(t, s, coll.ID) // the handler's read, before the lock
	s.SetAfterItemPreLockReadHookForTesting(func(string) {
		s.SetAfterItemPreLockReadHookForTesting(nil)
		setSchema(t, s, coll.ID, bug3407WithColor)
	})
	_, err := s.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]any{"color": "blue"}, ValidatedSchema: validated})
	wantValidation(t, err, "patch")
	if v, ok := storedColor(t, s, item.ID); ok {
		t.Errorf("color stored as %v; nothing should be written", v)
	}
}

func TestBug3407_AFullFieldsWriteSettingAForbiddenValueIsRefused(t *testing.T) {
	s, _, coll, item := bug3407Fixture(t)
	validated := schemaBytes(t, s, coll.ID)
	setSchema(t, s, coll.ID, bug3407WithColor)
	full := `{"status":"open","color":"blue"}`
	_, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: &full, ValidatedSchema: validated})
	wantValidation(t, err, "full fields")
	if _, ok := storedColor(t, s, item.ID); ok {
		t.Error("color was stored")
	}
}

func TestBug3407_ACreateValidatedAgainstTheOldSchemaIsRefused(t *testing.T) {
	s, ws, coll, _ := bug3407Fixture(t)
	validated := schemaBytes(t, s, coll.ID)
	setSchema(t, s, coll.ID, bug3407WithColor)
	_, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Two", Fields: `{"status":"open","color":"blue"}`, ValidatedSchema: validated})
	wantValidation(t, err, "create")
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM items WHERE collection_id = ? AND title = ?`), coll.ID, "Two").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d items created; want none", n)
	}
}

// Carried values are never re-judged: an item already holding a value the new
// schema forbids can still be written, as long as the write does not set it.
func TestBug3407_ACarriedValueIsNotReJudged(t *testing.T) {
	s, _, coll, item := bug3407Fixture(t)
	if _, err := s.db.Exec(s.q(`UPDATE items SET fields = ? WHERE id = ?`), `{"status":"open","color":"blue"}`, item.ID); err != nil {
		t.Fatal(err)
	}
	validated := schemaBytes(t, s, coll.ID)
	setSchema(t, s, coll.ID, bug3407WithColor)
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]any{"status": "done"}, ValidatedSchema: validated}); err != nil {
		t.Fatalf("patching another key: %v", err)
	}
	full := `{"status":"open","color":"blue"}`
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: &full, ValidatedSchema: validated}); err != nil {
		t.Fatalf("a full write carrying the value unchanged: %v", err)
	}
	if v, _ := storedColor(t, s, item.ID); v != "blue" {
		t.Errorf("carried color = %v, want blue", v)
	}
	// Changing it is a SET, and is judged.
	green := `{"status":"open","color":"green"}`
	_, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: &green, ValidatedSchema: validated})
	wantValidation(t, err, "changing the carried value")
}

// Control (lead condition 4): the schema moves in the same window, but the
// written values still satisfy it, and the write lands. The refusals above are
// about the value, not about any schema edit.
func TestBug3407_ASchemaChangeTheValuesStillSatisfyDoesNotRefuse(t *testing.T) {
	s, _, coll, item := bug3407Fixture(t)
	validated := schemaBytes(t, s, coll.ID)
	s.SetAfterItemPreLockReadHookForTesting(func(string) {
		s.SetAfterItemPreLockReadHookForTesting(nil)
		setSchema(t, s, coll.ID, bug3407Unrelated)
	})
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]any{"color": "blue"}, ValidatedSchema: validated}); err != nil {
		t.Fatalf("an undeclared key under an unrelated edit: %v", err)
	}
	if v, _ := storedColor(t, s, item.ID); v != "blue" {
		t.Errorf("color = %v, want blue (undeclared keys are accepted)", v)
	}
}

// The app path (BUG-3407): appstore validates against the schema it reads
// through CompanionCollectionSchema, so that read must hold the lock every
// schema writer takes.
func TestBug3407_TheFencedSchemaReadHoldsTheSeqLock(t *testing.T) {
	f := newFenceFixture(t)
	ftx, err := f.s.BeginFenced(context.Background(), f.spec())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ftx.Rollback() }()
	if ftx.seqLocked {
		t.Fatal("precondition: a fresh fence holds no seq lock")
	}
	if _, _, err := ftx.CompanionCollectionSchema(f.companion.ID); err != nil {
		t.Fatal(err)
	}
	if !ftx.seqLocked {
		t.Error("the schema read returned without the workspace seq lock")
	}
}

// Behaviour: a schema change waits for a fenced write that has read the
// schema. Discriminates on Postgres; on SQLite every writer is serialized by
// BEGIN IMMEDIATE, so the wait holds with or without the fix there.
func TestBug3407_ASchemaChangeWaitsForAFencedWriteThatReadTheSchema(t *testing.T) {
	f := newFenceFixture(t)
	ftx, err := f.s.BeginFenced(context.Background(), f.spec())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ftx.CompanionCollectionSchema(f.companion.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		schema := bug3407WithColor
		_, err := f.s.UpdateCollection(f.companion.ID, models.CollectionUpdate{Schema: &schema})
		done <- err
	}()
	select {
	case err := <-done:
		_ = ftx.Rollback()
		t.Fatalf("the schema change committed while the fenced write held its schema read (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := ftx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("schema change after the fence released: %v", err)
	}
}

// A move writes destination fields its caller validated before the lock
// (codex r1): a destination schema change in between is checked too.
func TestBug3407_AMoveIntoACollectionWhoseSchemaChangedIsRefused(t *testing.T) {
	s, ws, _, item := bug3407Fixture(t)
	dest, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Dest", Slug: "dest", Schema: bug3407Base})
	if err != nil {
		t.Fatal(err)
	}
	validated := schemaBytes(t, s, dest.ID)
	setSchema(t, s, dest.ID, bug3407WithColor)
	_, err = s.MoveItemWithPreCheck(item.ID, dest.ID, `{"status":"open","color":"blue"}`, nil, WithValidatedSchema(*validated))
	wantValidation(t, err, "move")
	got, err := s.GetItem(item.ID)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.CollectionID == dest.ID {
		t.Error("the item moved")
	}
	// Control: the same move with a value the destination allows lands.
	if _, err := s.MoveItemWithPreCheck(item.ID, dest.ID, `{"status":"open","color":"red"}`, nil, WithValidatedSchema(*validated)); err != nil {
		t.Fatalf("a valid move: %v", err)
	}
}

// A grandfathered schema that still declares a reserved key must not refuse
// the system's own value on any re-validated path (codex r2): the handlers
// judge reserved metadata by no schema, and so does the re-check.
const bug3407Grandfathered = `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open"},{"key":"implementation_notes","label":"Notes","type":"select","options":["x"]}]}`

func TestBug3407_AGrandfatheredReservedDeclarationRefusesNothing(t *testing.T) {
	s, ws, coll, item := bug3407Fixture(t)
	validated := schemaBytes(t, s, coll.ID)
	setSchema(t, s, coll.ID, bug3407Grandfathered)
	// Update: an append writes implementation_notes, which the stale
	// declaration would forbid.
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{ImplementationNoteToAppend: &models.ItemImplementationNote{Summary: "did a thing"}, ValidatedSchema: validated}); err != nil {
		t.Fatalf("append under a grandfathered declaration: %v", err)
	}
	got, err := s.GetItem(item.ID)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	// Move: the notes travel with the item into a destination declaring the
	// same stale key.
	dest, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Dest", Slug: "dest", Schema: bug3407Base})
	if err != nil {
		t.Fatal(err)
	}
	destValidated := schemaBytes(t, s, dest.ID)
	setSchema(t, s, dest.ID, bug3407Grandfathered)
	if _, err := s.MoveItemWithPreCheck(item.ID, dest.ID, got.Fields, nil, WithValidatedSchema(*destValidated)); err != nil {
		t.Fatalf("move carrying notes: %v", err)
	}
	// Create (a copy's path): a blob carrying the notes.
	if _, err := s.CreateItem(ws.ID, dest.ID, models.ItemCreate{Title: "Copy", Fields: got.Fields, ValidatedSchema: destValidated}); err != nil {
		t.Fatalf("create carrying notes: %v", err)
	}
}

// The store's contract for callers that make no claim is unchanged: an
// internal flow writing below the schema on purpose is not refused.
func TestBug3407_ACallerPassingNoValidatedSchemaIsNotChecked(t *testing.T) {
	s, ws, coll, item := bug3407Fixture(t)
	setSchema(t, s, coll.ID, bug3407WithColor)
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]any{"color": "blue"}}); err != nil {
		t.Fatalf("update without a claim: %v", err)
	}
	if _, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Raw", Fields: `{"color":"blue"}`}); err != nil {
		t.Fatalf("create without a claim: %v", err)
	}
}

// Optimistic: when the schema has not moved, the store re-judges nothing,
// whatever the value. The handler's validation stands.
func TestBug3407_AnUnmovedSchemaIsNotReJudged(t *testing.T) {
	s, _, coll, item := bug3407Fixture(t)
	setSchema(t, s, coll.ID, bug3407WithColor)
	current := schemaBytes(t, s, coll.ID)
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]any{"color": "blue"}, ValidatedSchema: current}); err != nil {
		t.Fatalf("unmoved schema: %v", err)
	}
}

// codex r3: an UpdateCollection that supplies a schema takes the workspace
// seq lock even when the bytes look unchanged against its pre-transaction
// read, because whether they moved is decided only under the locks. Here
// the bytes really are unchanged, and the update still waits for a held seq
// lock. Discriminates on Postgres; on SQLite every writer is serialized.
func TestBug3407_ASuppliedSchemaTakesTheSeqLockEvenWhenItLooksUnchanged(t *testing.T) {
	f := newFenceFixture(t)
	ftx, err := f.s.BeginFenced(context.Background(), f.spec())
	if err != nil {
		t.Fatal(err)
	}
	if err := ftx.LockWorkspaceSeq(); err != nil {
		t.Fatal(err)
	}
	same := *schemaBytes(t, f.s, f.companion.ID)
	done := make(chan error, 1)
	go func() {
		_, err := f.s.UpdateCollection(f.companion.ID, models.CollectionUpdate{Schema: &same})
		done <- err
	}()
	select {
	case err := <-done:
		_ = ftx.Rollback()
		t.Fatalf("a supplied schema did not wait for the seq lock (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := ftx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("schema update after release: %v", err)
	}
}
