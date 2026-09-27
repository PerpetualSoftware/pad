package store_test

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-3244, store half, on both dialects (migration 097 / pg 072): the
// set-aside move, the content_state value it produces, and the bundle.
// Postgres legs skip unless PAD_TEST_POSTGRES_URL is set (make test-pg).

// A content-bearing sync update, distinct from every other frame here.
var setAsideFrame = []byte{0x00, 0x02, 0x05, 0x01, 0x44, 0x00, 0x7F, 0x00}

func eachBackend(t *testing.T, fn func(t *testing.T, s *store.Store)) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) *store.Store
	}{
		{"SQLite", storetest.NewSQLite},
		{"Postgres", storetest.NewPostgres},
	} {
		t.Run(backend.name, func(t *testing.T) { fn(t, backend.open(t)) })
	}
}

func setAsideOne(t *testing.T, s *store.Store, itemID string) {
	t.Helper()
	if _, err := s.AppendYjsUpdate(itemID, setAsideFrame, "1"); err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}
	moved, cleared, err := s.SetAsideAndClearOpLog(itemID)
	if err != nil {
		t.Fatalf("SetAsideAndClearOpLog: %v", err)
	}
	if moved != 1 || cleared != 1 {
		t.Fatalf("SetAsideAndClearOpLog = (%d moved, %d cleared), want (1, 1)", moved, cleared)
	}
}

func TestSetAsideMovesRowsAndMarksTheItem(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store) {
		_, _, item := seedStaleItem(t, s)
		setAsideOne(t, s, item.ID)

		got, err := s.GetItem(item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ContentState != models.ContentStateSetAside || got.Content != "stored body" {
			t.Fatalf("after set-aside: content_state %q content %q", got.ContentState, got.Content)
		}
		rows, err := s.ListYjsSetAside(item.ID)
		if err != nil || len(rows) != 1 || string(rows[0].UpdateData) != string(setAsideFrame) {
			t.Fatalf("ListYjsSetAside = %+v (err %v), want the frame verbatim", rows, err)
		}
		if ops, _ := s.LoadYjsUpdatesSince(item.ID, 0); len(ops) != 0 {
			t.Fatalf("op-log rows after set-aside = %d, want 0", len(ops))
		}

		// Discard clears the state; the body is untouched.
		n, err := s.DiscardYjsSetAside(item.ID)
		if err != nil || n != 1 {
			t.Fatalf("DiscardYjsSetAside = %d, %v", n, err)
		}
		got, _ = s.GetItem(item.ID)
		if got.ContentState != "" || got.Content != "stored body" {
			t.Fatalf("after discard: content_state %q content %q", got.ContentState, got.Content)
		}
	})
}

// The bundle carries set-aside rows (condition a) and an import reattaches
// them, so the imported item reads the same state as its source.
func TestSetAsideRoundTripsThroughTheBundle(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store) {
		_, _, item := seedStaleItem(t, s)
		setAsideOne(t, s, item.ID)

		exp, err := s.ExportWorkspace("cs")
		if err != nil {
			t.Fatalf("ExportWorkspace: %v", err)
		}
		var ex *models.ItemExport
		for i := range exp.Items {
			if exp.Items[i].ID == item.ID {
				ex = &exp.Items[i]
			}
		}
		if ex == nil || ex.ContentState != models.ContentStateSetAside || len(ex.CollabSetAside) != 1 ||
			string(ex.CollabSetAside[0].UpdateData) != string(setAsideFrame) || ex.CollabSetAside[0].SchemaVersion != "1" {
			t.Fatalf("exported item = %+v, want content_state %q and the one frame", ex, models.ContentStateSetAside)
		}

		dst, err := s.ImportWorkspace(exp, "Imported set-aside", "", "test")
		if err != nil {
			t.Fatalf("ImportWorkspace: %v", err)
		}
		items, err := s.ListItems(dst.ID, models.ItemListParams{})
		if err != nil {
			t.Fatal(err)
		}
		var imported *models.Item
		for i := range items {
			if items[i].Title == item.Title {
				imported = &items[i]
			}
		}
		if imported == nil {
			t.Fatal("imported item not found")
		}
		rows, err := s.ListYjsSetAside(imported.ID)
		if err != nil || len(rows) != 1 || string(rows[0].UpdateData) != string(setAsideFrame) {
			t.Fatalf("imported set-aside rows = %+v (err %v)", rows, err)
		}
		got, _ := s.GetItem(imported.ID)
		if got.ContentState != models.ContentStateSetAside {
			t.Fatalf("imported content_state = %q, want %q", got.ContentState, models.ContentStateSetAside)
		}
	})
}

// A bundle row that did not come from a server is skipped, not stored and not
// fatal: this file's lenient import precedent.
func TestSetAsideImportSkipsMalformedRows(t *testing.T) {
	s := storetest.NewSQLite(t)
	_, _, item := seedStaleItem(t, s)
	setAsideOne(t, s, item.ID)
	exp, err := s.ExportWorkspace("cs")
	if err != nil {
		t.Fatal(err)
	}
	for i := range exp.Items {
		if exp.Items[i].ID == item.ID {
			good := exp.Items[i].CollabSetAside[0]
			badVersion, badTime, empty := good, good, good
			badVersion.SchemaVersion = "1\x00; DROP"
			badTime.CreatedAt = "yesterday"
			empty.UpdateData = nil
			exp.Items[i].CollabSetAside = append(exp.Items[i].CollabSetAside, badVersion, badTime, empty)
		}
	}
	dst, err := s.ImportWorkspace(exp, "Imported malformed", "", "test")
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}
	items, _ := s.ListItems(dst.ID, models.ItemListParams{})
	for _, it := range items {
		if it.Title != item.Title {
			continue
		}
		rows, err := s.ListYjsSetAside(it.ID)
		if err != nil || len(rows) != 1 {
			t.Fatalf("imported rows = %d (err %v), want only the 1 well-formed row", len(rows), err)
		}
		return
	}
	t.Fatal("imported item not found")
}

// The migrate-to-pg gate asks about the OP-LOG only. Set-aside rows travel in
// the bundle, so an item holding only those is not refused; an item holding
// both still is, although its content_state reads superseded_set_aside.
func TestMigrationGateAsksTheOpLogOnly(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store) {
		wsID, _, item := seedStaleItem(t, s)
		setAsideOne(t, s, item.ID)

		pending, err := s.ListItemsPendingContentFlush(wsID)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 0 {
			t.Fatalf("gate names %+v for an item holding only set-aside rows", pending)
		}

		if _, err := s.AppendYjsUpdate(item.ID, []byte{0x00, 0x02, 0x03, 0x0A, 0x0B, 0x0C}, "2"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetItem(item.ID)
		if got.ContentState != models.ContentStateSetAside {
			t.Fatalf("premise: an item holding both reads %q, want %q", got.ContentState, models.ContentStateSetAside)
		}
		pending, err = s.ListItemsPendingContentFlush(wsID)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 1 {
			t.Fatalf("gate names %d item(s) for an item holding set-aside AND pending rows, want 1", len(pending))
		}
	})
}
