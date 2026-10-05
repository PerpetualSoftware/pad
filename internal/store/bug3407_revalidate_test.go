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
	s.SetAfterItemPreLockReadHookForTesting(func(string) {
		s.SetAfterItemPreLockReadHookForTesting(nil)
		setSchema(t, s, coll.ID, bug3407WithColor)
	})
	_, err := s.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]any{"color": "blue"}})
	wantValidation(t, err, "patch")
	if v, ok := storedColor(t, s, item.ID); ok {
		t.Errorf("color stored as %v; nothing should be written", v)
	}
}

func TestBug3407_AFullFieldsWriteSettingAForbiddenValueIsRefused(t *testing.T) {
	s, _, coll, item := bug3407Fixture(t)
	setSchema(t, s, coll.ID, bug3407WithColor)
	full := `{"status":"open","color":"blue"}`
	_, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: &full})
	wantValidation(t, err, "full fields")
	if _, ok := storedColor(t, s, item.ID); ok {
		t.Error("color was stored")
	}
}

func TestBug3407_ACreateValidatedAgainstTheOldSchemaIsRefused(t *testing.T) {
	s, ws, coll, _ := bug3407Fixture(t)
	setSchema(t, s, coll.ID, bug3407WithColor)
	_, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Two", Fields: `{"status":"open","color":"blue"}`})
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
	setSchema(t, s, coll.ID, bug3407WithColor)
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]any{"status": "done"}}); err != nil {
		t.Fatalf("patching another key: %v", err)
	}
	full := `{"status":"open","color":"blue"}`
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: &full}); err != nil {
		t.Fatalf("a full write carrying the value unchanged: %v", err)
	}
	if v, _ := storedColor(t, s, item.ID); v != "blue" {
		t.Errorf("carried color = %v, want blue", v)
	}
	// Changing it is a SET, and is judged.
	green := `{"status":"open","color":"green"}`
	_, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: &green})
	wantValidation(t, err, "changing the carried value")
}

// Control: a schema change that does not touch the written key lets the
// write through, so the refusals above are about the key, not any edit.
func TestBug3407_AnUnrelatedSchemaEditDoesNotRefuse(t *testing.T) {
	s, _, coll, item := bug3407Fixture(t)
	s.SetAfterItemPreLockReadHookForTesting(func(string) {
		s.SetAfterItemPreLockReadHookForTesting(nil)
		setSchema(t, s, coll.ID, bug3407Unrelated)
	})
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]any{"color": "blue"}}); err != nil {
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
