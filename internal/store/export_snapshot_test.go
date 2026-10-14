package store

import (
	"context"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3654. An export read each section on the pool, so a writer landing
// between the collections scan and the items scan put an item in the bundle
// without its collection, and ImportWorkspace dropped it as an orphan. The
// attachment manifest, read in a separate call by the bundle door, had the same
// gap against the items it names.
//
// The hook commits a collection, an item in it and an attachment on that item
// on the POOL, between the collections section and the items section. A
// snapshot export carries none of the three; a pooled one carries the item and
// the attachment without the collection.
//
// The writer runs in a goroutine with a deadline, because the other way to get
// this wrong is a snapshot that holds SQLite's write lock (db.Begin() is BEGIN
// IMMEDIATE under the DSN's _txlock): the write would then wait out the 30s
// busy_timeout behind the export instead of committing beside it.
//
// Not parallel: the hook is a package variable.
func TestExportSnapshotExcludesWritesLandingMidExport(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Export Snapshot 3654")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	early := createTestItem(t, s, ws.ID, col.ID, "Early", "here before the export")

	var (
		lateCol  *models.Collection
		lateItem *models.Item
		lateAtt  *models.Attachment
		writeErr error
		fired    int
	)
	exportAfterCollectionsHook = func(slug string) {
		if slug != ws.Slug {
			return
		}
		fired++
		done := make(chan struct{})
		go func() {
			defer close(done)
			c, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Late", Prefix: "LATE"})
			if err != nil {
				writeErr = err
				return
			}
			it, err := s.CreateItem(ws.ID, c.ID, models.ItemCreate{Title: "Late item"})
			if err != nil {
				writeErr = err
				return
			}
			itemID := it.ID
			a := &models.Attachment{
				WorkspaceID: ws.ID, ItemID: &itemID, UploadedBy: "uploader",
				StorageKey: "fs:" + newID(), ContentHash: newID(), MimeType: "image/png",
				SizeBytes: 64, Filename: "late.png",
			}
			if err := s.CreateAttachment(a); err != nil {
				writeErr = err
				return
			}
			lateCol, lateItem, lateAtt = c, it, a
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("a write issued mid-export did not commit within 5s: the export is holding the write lock")
			// Let the export finish and release it, so the writer can end.
			go func() { <-done }()
		}
	}
	t.Cleanup(func() { exportAfterCollectionsHook = nil })

	exp, atts, err := s.ExportWorkspaceSnapshot(context.Background(), ws.Slug, true)
	exportAfterCollectionsHook = nil
	if err != nil {
		t.Fatalf("ExportWorkspaceSnapshot: %v", err)
	}
	if fired != 1 {
		t.Fatalf("hook fired %d times, want 1: the mid-export write never happened", fired)
	}
	if t.Failed() {
		return
	}
	if writeErr != nil {
		t.Fatalf("mid-export write: %v", writeErr)
	}

	// Control: the writes committed, and a later export sees all three. Without
	// this the absence assertions below would also pass for a write that
	// silently did nothing.
	after, afterAtts, err := s.ExportWorkspaceSnapshot(context.Background(), ws.Slug, true)
	if err != nil {
		t.Fatalf("second ExportWorkspaceSnapshot: %v", err)
	}
	if !exportHasCollection(after, lateCol.ID) || !exportHasItem(after, lateItem.ID) || !exportHasAttachment(afterAtts, lateAtt.ID) {
		t.Fatal("control leg failed: an export after the write does not carry the late collection, item and attachment")
	}

	if !exportHasItem(exp, early.ID) {
		t.Fatal("control leg failed: the export dropped the item that existed before it began")
	}
	if exportHasCollection(exp, lateCol.ID) {
		t.Error("the export carries a collection created after its collections section was read")
	}
	if exportHasItem(exp, lateItem.ID) {
		t.Error("the export carries an item committed after it began; with its collection absent, import drops it")
	}
	if exportHasAttachment(atts, lateAtt.ID) {
		t.Error("the bundle manifest carries an attachment committed after the export began")
	}
	inBundle := map[string]bool{}
	for _, c := range exp.Collections {
		inBundle[c.ID] = true
	}
	for _, it := range exp.Items {
		if !inBundle[it.CollectionID] {
			t.Errorf("item %q names collection %s, absent from the bundle", it.Title, it.CollectionID)
		}
	}
}

// TestExportWorkspaceIsASnapshot pins that the plain ExportWorkspace (the JSON
// door's shape) reads through the snapshot too, not only the bundle variant.
func TestExportWorkspaceIsASnapshot(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Export Snapshot Plain 3654")
	createTestCollection(t, s, ws.ID, "Tasks")

	var lateItem *models.Item
	var writeErr error
	exportAfterCollectionsHook = func(slug string) {
		if slug != ws.Slug {
			return
		}
		c, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Late", Prefix: "LATE"})
		if err == nil {
			lateItem, err = s.CreateItem(ws.ID, c.ID, models.ItemCreate{Title: "Late item"})
		}
		writeErr = err
	}
	t.Cleanup(func() { exportAfterCollectionsHook = nil })

	exp, err := s.ExportWorkspace(ws.Slug)
	exportAfterCollectionsHook = nil
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	if writeErr != nil || lateItem == nil {
		t.Fatalf("mid-export write did not happen: %v", writeErr)
	}
	if exportHasItem(exp, lateItem.ID) {
		t.Error("ExportWorkspace carries an item committed after it began: it is not reading one snapshot")
	}
}

func exportHasCollection(exp *models.WorkspaceExport, id string) bool {
	for _, c := range exp.Collections {
		if c.ID == id {
			return true
		}
	}
	return false
}

func exportHasItem(exp *models.WorkspaceExport, id string) bool {
	for _, it := range exp.Items {
		if it.ID == id {
			return true
		}
	}
	return false
}

func exportHasAttachment(atts []models.Attachment, id string) bool {
	for _, a := range atts {
		if a.ID == id {
			return true
		}
	}
	return false
}
