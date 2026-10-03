package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"runtime"
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

// fatalRecorder stands in for *testing.T so a test can observe the harness
// REFUSING a capture: Fatalf records the message and ends the goroutine, as
// the real one does.
type fatalRecorder struct {
	testing.TB
	msg string
}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.msg = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (r *fatalRecorder) Fatal(args ...any) {
	r.msg = fmt.Sprint(args...)
	runtime.Goexit()
}

// runRecorded runs CaptureWrites under a recorder and returns its fatal
// message, or "" if the capture completed.
func runRecorded(t *testing.T, s *store.Store, fn func()) string {
	t.Helper()
	r := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		CaptureWrites(r, s, fn)
	}()
	<-done
	return r.msg
}

// A write committed on a connection OTHER than the hooked one is not
// observed by the hook, so the capture must refuse rather than report a
// clean result (codex round 1 on TASK-3388: before this check, widening the
// pool mid-capture lost a write with both sentinels passing).
func TestCaptureWrites_RefusesAWriteOnAnotherConnection(t *testing.T) {
	s := NewSQLite(t)
	if _, err := s.DB().Exec(`CREATE TABLE cap_other (x TEXT)`); err != nil {
		t.Fatal(err)
	}
	msg := runRecorded(t, s, func() {
		db := s.DB()
		db.SetMaxOpenConns(2)
		hooked, err := db.Conn(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		defer hooked.Close()
		// With the hooked connection checked out, this runs on a second one.
		if _, err := db.Exec(`INSERT INTO cap_other VALUES ('lost')`); err != nil {
			t.Error(err)
		}
	})
	// Either refusal is right: another connection committed, or the hooked
	// connection itself was replaced.
	if !strings.Contains(msg, "another connection committed") && !strings.Contains(msg, "no longer on the hooked connection") {
		t.Fatalf("capture did not refuse a write on another connection; got %q", msg)
	}
}

// Postgres: a table created after an earlier capture is covered by the next
// one, and a table created DURING a capture is refused, not silently missed
// (codex round 1 on TASK-3388).
func TestCaptureWrites_PostgresCoversTablesCreatedLater(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	s := NewPostgres(t)
	CaptureWrites(t, s, func() {})
	for _, q := range []string{
		`CREATE TABLE cap_src (x TEXT)`,
		`CREATE TABLE cap_log (x TEXT)`,
		`CREATE FUNCTION cap_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO cap_log VALUES (NEW.x); RETURN NULL; END $$`,
		`CREATE TRIGGER cap_tr AFTER INSERT ON cap_src FOR EACH ROW EXECUTE FUNCTION cap_fn()`,
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
	if tables := Tables(writes); !contains(tables, "cap_src") || !contains(tables, "cap_log") {
		t.Fatalf("tables created after the first capture were not covered: %v", tables)
	}
	msg := runRecorded(t, s, func() {
		if _, err := s.DB().Exec(`CREATE TABLE cap_during (x TEXT)`); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(msg, "unaudited") {
		t.Fatalf("a table created during capture was not refused; got %q", msg)
	}
}

// The data_version branch: the hooked connection stays in place while a
// write commits through a different handle on the same database file.
func TestCaptureWrites_RefusesACommitFromAnotherHandle(t *testing.T) {
	s := NewSQLite(t)
	if _, err := s.DB().Exec(`CREATE TABLE cap_other (x TEXT)`); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := s.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path); err != nil || path == "" {
		t.Fatalf("database file: %q %v", path, err)
	}
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	msg := runRecorded(t, s, func() {
		if _, err := other.Exec(`INSERT INTO cap_other VALUES ('lost')`); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(msg, "another connection committed") {
		t.Fatalf("capture did not refuse a commit from another handle; got %q", msg)
	}
}
