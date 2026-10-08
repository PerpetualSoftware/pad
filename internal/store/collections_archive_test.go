package store_test

import (
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-2189: list and restore archived collections, on both dialects (the
// deleted_at scan and the item-count subquery), and a restore race.

func TestArchivedCollections_SQLite(t *testing.T)   { archivedCollections(t, storetest.NewSQLite(t)) }
func TestArchivedCollections_Postgres(t *testing.T) { archivedCollections(t, storetest.NewPostgres(t)) }

func archivedCollections(t *testing.T, s *store.Store) {
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Archive 2189"})
	if err != nil {
		t.Fatal(err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Field Notes"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "note"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteCollection(coll.ID, ""); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListArchivedCollections(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != coll.ID || list[0].ItemCount != 2 || list[0].ArchivedAt == "" {
		t.Fatalf("archived = %+v, want the collection with 2 items and a time", list)
	}

	// A live slug, an unknown ref and another workspace's ref name nothing.
	other, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Other 2189"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ ws, ref string }{{ws.ID, "nope"}, {other.ID, coll.ID}, {other.ID, coll.Slug}} {
		if _, err := s.ResolveArchivedCollection(c.ws, c.ref); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("ResolveArchivedCollection(%s, %s) = %v, want ErrNoRows", c.ws, c.ref, err)
		}
	}

	// Two concurrent restores: exactly one wins, the other is told there is
	// nothing archived to restore.
	id, err := s.ResolveArchivedCollection(ws.ID, coll.Slug)
	if err != nil || id != coll.ID {
		t.Fatalf("resolve by slug = %q, %v", id, err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, losses := 0, 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.RestoreCollection(ws.ID, id)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, sql.ErrNoRows):
				losses++
			default:
				t.Errorf("restore: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || losses != 1 {
		t.Fatalf("concurrent restores: %d won, %d lost; want exactly one of each", wins, losses)
	}
	live, err := s.GetCollection(coll.ID)
	if err != nil || live == nil || live.DeletedAt != nil {
		t.Fatalf("after restore: %+v %v, want the live collection", live, err)
	}
	if list, _ := s.ListArchivedCollections(ws.ID); len(list) != 0 {
		t.Fatalf("still listed as archived: %+v", list)
	}
}
