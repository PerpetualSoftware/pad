package storetest

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// captureStore is Postgres under make test-pg, SQLite otherwise.
func captureStore(t *testing.T) *store.Store {
	t.Helper()
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		return NewPostgres(t)
	}
	return NewSQLite(t)
}

func seedCollection(t *testing.T, s *store.Store) (*models.Workspace, *models.Collection) {
	t.Helper()
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Capture"})
	if err != nil {
		t.Fatal(err)
	}
	col, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tickets",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ws, col
}

// The instrument must go RED on real writes before its silence about an app
// write means anything. A human create writes the item, and on SQLite its
// search-index triggers write FTS5 shadow tables at depth > 0.
func TestCaptureWrites_SeesARealWrite(t *testing.T) {
	s := captureStore(t)
	ws, col := seedCollection(t, s)
	writes := CaptureWrites(t, s, func() {
		if _, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Captured", Content: "searchable body", Fields: `{"status":"open"}`}); err != nil {
			t.Fatal(err)
		}
	})
	tables := Tables(writes)
	if !contains(tables, "items") {
		t.Fatalf("an item create did not capture items: %v", tables)
	}
	if s.D().Driver() == store.DriverPostgres {
		return
	}
	// The schema's FTS triggers write the virtual items_fts (blind); FTS5
	// then writes its shadow tables through its own statements, which the
	// hook sees at depth 0.
	var shadow bool
	for _, w := range writes {
		if strings.HasPrefix(w.Table, "items_fts_") {
			shadow = true
		}
	}
	if !shadow {
		t.Fatalf("FTS5 shadow writes not captured: %v", writes)
	}
}

// A write a TRIGGER makes into an ordinary table is captured, at depth > 0.
// No trigger in today's schema writes an ordinary table (every data-writing
// trigger targets an FTS virtual table), so a test-only one on this store's
// private copy stands in for the first that does.
func TestCaptureWrites_SeesATriggerWrite(t *testing.T) {
	s := NewSQLite(t)
	for _, q := range []string{
		`CREATE TABLE cap_src (x TEXT)`,
		`CREATE TABLE cap_log (x TEXT)`,
		`CREATE TRIGGER cap_tr AFTER INSERT ON cap_src BEGIN INSERT INTO cap_log VALUES (NEW.x); END`,
	} {
		if _, err := s.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	writes := CaptureWrites(t, s, func() {
		if _, err := s.DB().Exec(`INSERT INTO cap_src VALUES ('a')`); err != nil {
			t.Fatal(err)
		}
	})
	var direct, triggered bool
	for _, w := range writes {
		if w.Table == "cap_src" && w.Depth == 0 {
			direct = true
		}
		if w.Table == "cap_log" && w.Depth > 0 {
			triggered = true
		}
	}
	if !direct || !triggered {
		t.Fatalf("direct=%v triggered=%v: %v", direct, triggered, writes)
	}
}

// A read writes nothing, and the capture says so.
func TestCaptureWrites_ReadCapturesNothing(t *testing.T) {
	s := captureStore(t)
	ws, col := seedCollection(t, s)
	if _, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Existing", Fields: `{"status":"open"}`}); err != nil {
		t.Fatal(err)
	}
	writes := CaptureWrites(t, s, func() {
		if _, err := s.ListItems(ws.ID, models.ItemListParams{}); err != nil {
			t.Fatal(err)
		}
	})
	if len(writes) != 0 {
		t.Fatalf("a read captured writes: %v", writes)
	}
}

// The pre-update hook is blind to virtual tables and to SQLite's own system
// tables. DOC-3371 §4 states that inventory exactly; a new virtual or system
// table fails here until its blind spot is considered.
func TestBlindInventory(t *testing.T) {
	s := NewSQLite(t)
	rows, err := s.DB().Query(`SELECT name FROM sqlite_master
		WHERE type = 'table' AND (sql LIKE 'CREATE VIRTUAL TABLE%' OR name LIKE 'sqlite\_%' ESCAPE '\')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"comments_fts", "documents_fts", "items_fts", "sqlite_sequence"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("blind inventory = %v, want exactly %v", got, want)
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
