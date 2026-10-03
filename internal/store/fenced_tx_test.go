package store

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// insertTestInstall writes an app_installs row directly. No production
// writer exists before SPEC-6 U8.
func insertTestInstall(t *testing.T, s *Store, workspaceID, state string, epoch int64) string {
	t.Helper()
	id := newID()
	ts := now()
	if _, err := s.db.Exec(s.q(`INSERT INTO app_installs (id, workspace_id, origin, state, auth_epoch, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
		id, workspaceID, "https://app.example", state, epoch, ts, ts); err != nil {
		t.Fatal(err)
	}
	return id
}

type fenceFixture struct {
	s         *Store
	ws        *models.Workspace
	companion *models.Collection
	other     *models.Collection
	install   string
}

func newFenceFixture(t *testing.T) fenceFixture {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Fence WS")
	return fenceFixture{
		s:         s,
		ws:        ws,
		companion: createTestCollection(t, s, ws.ID, "Tickets"),
		other:     createTestCollection(t, s, ws.ID, "Private"),
		install:   insertTestInstall(t, s, ws.ID, "active", 1),
	}
}

func (f fenceFixture) spec() FenceSpec {
	return FenceSpec{InstallID: f.install, WorkspaceID: f.ws.ID, Epoch: 1, Companions: []string{f.companion.ID}}
}

// The fence admits only the install, epoch and workspace the request was
// admitted under, while the install is active.
func TestFencedTx_AdmitsOnlyTheCurrentEpochOfAnActiveInstall(t *testing.T) {
	f := newFenceFixture(t)
	ctx := context.Background()
	ftx, err := f.s.BeginFenced(ctx, f.spec())
	if err != nil {
		t.Fatalf("current epoch: %v", err)
	}
	if err := ftx.Rollback(); err != nil {
		t.Fatal(err)
	}

	otherWS := createTestWorkspace(t, f.s, "Elsewhere")
	for name, spec := range map[string]FenceSpec{
		"stale epoch":     {InstallID: f.install, WorkspaceID: f.ws.ID, Epoch: 2},
		"other workspace": {InstallID: f.install, WorkspaceID: otherWS.ID, Epoch: 1},
		"unknown install": {InstallID: "nope", WorkspaceID: f.ws.ID, Epoch: 1},
		"no install":      {WorkspaceID: f.ws.ID, Epoch: 1},
		"zero epoch":      {InstallID: f.install, WorkspaceID: f.ws.ID},
	} {
		if _, err := f.s.BeginFenced(ctx, spec); !errors.Is(err, ErrFenceStale) {
			t.Errorf("%s: %v, want ErrFenceStale", name, err)
		}
	}
	for _, state := range []string{"disabling", "inactive", "uninstalling", "uninstalled"} {
		if _, err := f.s.db.Exec(f.s.q(`UPDATE app_installs SET state = ? WHERE id = ?`), state, f.install); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.BeginFenced(ctx, f.spec()); !errors.Is(err, ErrFenceStale) {
			t.Errorf("state %s: %v, want ErrFenceStale", state, err)
		}
	}
}

// Writes are fenced on companion collections, and a derived row on the
// CURRENT collection of the live item it hangs off, in this workspace.
func TestFencedTx_CompanionChecks(t *testing.T) {
	f := newFenceFixture(t)
	mine := createTestItem(t, f.s, f.ws.ID, f.companion.ID, "Ticket", "")
	hidden := createTestItem(t, f.s, f.ws.ID, f.other.ID, "Private note", "")
	gone := createTestItem(t, f.s, f.ws.ID, f.companion.ID, "Deleted ticket", "")
	if err := f.s.DeleteItem(gone.ID); err != nil {
		t.Fatal(err)
	}
	otherWS := createTestWorkspace(t, f.s, "Elsewhere")
	foreign := createTestItem(t, f.s, otherWS.ID, createTestCollection(t, f.s, otherWS.ID, "Tickets").ID, "Foreign", "")

	ftx, err := f.s.BeginFenced(context.Background(), f.spec())
	if err != nil {
		t.Fatal(err)
	}
	defer ftx.Rollback()

	if err := ftx.requireCompanionCollection(f.companion.ID); err != nil {
		t.Errorf("companion collection: %v", err)
	}
	if err := ftx.requireCompanionCollection(f.other.ID); !errors.Is(err, ErrNotCompanion) {
		t.Errorf("other collection: %v", err)
	}
	if got, err := ftx.requireCompanionItem(mine.ID); err != nil || got != f.companion.ID {
		t.Errorf("companion item: %q %v", got, err)
	}
	for name, id := range map[string]string{"non-companion": hidden.ID, "deleted": gone.ID, "other workspace": foreign.ID, "unknown": "nope"} {
		if _, err := ftx.requireCompanionItem(id); !errors.Is(err, ErrNotCompanion) {
			t.Errorf("%s item: %v, want ErrNotCompanion", name, err)
		}
	}
}

// The numbers a fenced write would allocate are the human create's own.
func TestFencedTx_AllocatesWhatTheHumanPathWould(t *testing.T) {
	f := newFenceFixture(t)
	it := createTestItem(t, f.s, f.ws.ID, f.companion.ID, "First", "")
	ftx, err := f.s.BeginFenced(context.Background(), f.spec())
	if err != nil {
		t.Fatal(err)
	}
	defer ftx.Rollback()
	n, err := ftx.NextItemNumber()
	if err != nil || it.ItemNumber == nil || n != int64(*it.ItemNumber)+1 {
		t.Errorf("next item_number %d (%v), want one past %v", n, err, it.ItemNumber)
	}
	seq, err := ftx.NextSeq()
	if err != nil || seq != it.Seq+1 {
		t.Errorf("next seq %d (%v), want %d", seq, err, it.Seq+1)
	}
}

func TestFencedTx_FinishedTransactionRefusesWork(t *testing.T) {
	f := newFenceFixture(t)
	ftx, err := f.s.BeginFenced(context.Background(), f.spec())
	if err != nil {
		t.Fatal(err)
	}
	if err := ftx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := ftx.Commit(); !errors.Is(err, ErrFenceClosed) {
		t.Errorf("second commit: %v", err)
	}
	if err := ftx.Rollback(); err != nil {
		t.Errorf("rollback after commit: %v", err)
	}
	if err := ftx.LockWorkspaceSeq(); !errors.Is(err, ErrFenceClosed) {
		t.Errorf("lock after commit: %v", err)
	}
	if err := ftx.requireCompanionCollection(f.companion.ID); !errors.Is(err, ErrFenceClosed) {
		t.Errorf("write check after commit: %v", err)
	}
}

// Postgres: the fence's shared lock and the disable's exclusive lock
// serialize. A disable waits for an open FencedTx; once it commits, the next
// BeginFenced is refused. SQLite's single writer serializes the same pair
// without row locks.
func TestFencedTx_DisableWaitsForAnOpenFence(t *testing.T) {
	f := newFenceFixture(t)
	if f.s.dialect.Driver() != DriverPostgres {
		t.Skip("row locks: run under make test-pg")
	}
	ftx, err := f.s.BeginFenced(context.Background(), f.spec())
	if err != nil {
		t.Fatal(err)
	}
	disabled := make(chan error, 1)
	go func() {
		tx, err := f.s.db.Begin()
		if err != nil {
			disabled <- err
			return
		}
		defer tx.Rollback()
		if _, err := tx.Exec(f.s.q(`SELECT id FROM app_installs WHERE id = ? FOR UPDATE`), f.install); err != nil {
			disabled <- err
			return
		}
		if _, err := tx.Exec(f.s.q(`UPDATE app_installs SET state = 'disabling', auth_epoch = auth_epoch + 1 WHERE id = ?`), f.install); err != nil {
			disabled <- err
			return
		}
		disabled <- tx.Commit()
	}()
	waitForFenceLockWait(t, f.s, disabled, "the disable")
	if err := ftx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-disabled; err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := f.s.BeginFenced(context.Background(), f.spec()); !errors.Is(err, ErrFenceStale) {
		t.Fatalf("fence after disable: %v", err)
	}
}

// Postgres: a FencedTx holding the workspace seq lock makes a human create in
// the same workspace wait, so the two cannot allocate the same seq.
func TestFencedTx_SeqLockBlocksAHumanCreate(t *testing.T) {
	f := newFenceFixture(t)
	if f.s.dialect.Driver() != DriverPostgres {
		t.Skip("advisory locks: run under make test-pg")
	}
	ftx, err := f.s.BeginFenced(context.Background(), f.spec())
	if err != nil {
		t.Fatal(err)
	}
	if err := ftx.LockWorkspaceSeq(); err != nil {
		t.Fatal(err)
	}
	created := make(chan error, 1)
	go func() {
		_, err := f.s.CreateItem(f.ws.ID, f.other.ID, models.ItemCreate{Title: "Human"})
		created <- err
	}()
	waitForFenceLockWait(t, f.s, created, "a human create")
	if err := ftx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-created; err != nil {
		t.Fatalf("human create after release: %v", err)
	}
}

// Only internal/appstore opens a FencedTx (SPEC-6 §4). Every other package in
// the module, its tests included, is parsed and must not reference
// BeginFenced. The match is on the selector in the syntax tree, so a comment
// or a string naming it does not count.
func TestFencedTx_OnlyAppstoreOpensOne(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			switch {
			case rel == "internal/store", rel == "internal/appstore",
				d.Name() == "node_modules", d.Name() == "testdata", strings.HasPrefix(d.Name(), "."):
				if rel != "." {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "BeginFenced" {
				offenders = append(offenders, rel)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("BeginFenced referenced outside internal/appstore: %v", offenders)
	}
}

// No package but this one can make a usable FencedTx: every field is
// unexported (so a literal elsewhere does not compile), and a zero value, the
// only thing new or reflect.New can make, refuses every method.
func TestFencedTx_CannotBeMadeOutsideBeginFenced(t *testing.T) {
	typ := reflect.TypeOf(FencedTx{})
	for i := 0; i < typ.NumField(); i++ {
		if f := typ.Field(i); f.IsExported() {
			t.Errorf("FencedTx.%s is exported; another package could set it", f.Name)
		}
	}
	for _, zero := range []*FencedTx{new(FencedTx), reflect.New(typ).Interface().(*FencedTx), nil} {
		if err := zero.LockWorkspaceSeq(); !errors.Is(err, ErrFenceClosed) {
			t.Errorf("LockWorkspaceSeq on a zero FencedTx: %v", err)
		}
		if _, err := zero.NextSeq(); !errors.Is(err, ErrFenceClosed) {
			t.Errorf("NextSeq: %v", err)
		}
		if _, err := zero.NextItemNumber(); !errors.Is(err, ErrFenceClosed) {
			t.Errorf("NextItemNumber: %v", err)
		}
		if err := zero.requireCompanionCollection("x"); !errors.Is(err, ErrFenceClosed) {
			t.Errorf("requireCompanionCollection: %v", err)
		}
		if _, err := zero.requireCompanionItem("x"); !errors.Is(err, ErrFenceClosed) {
			t.Errorf("requireCompanionItem: %v", err)
		}
		if err := zero.Commit(); !errors.Is(err, ErrFenceClosed) {
			t.Errorf("Commit: %v", err)
		}
		if err := zero.Rollback(); err != nil {
			t.Errorf("Rollback: %v", err)
		}
	}
}

// waitForFenceLockWait returns once Postgres reports a backend in this test's
// database waiting on a LOCK, which is the proof the worker is blocked on the
// fence and not merely slow to be scheduled. It fails if the worker finishes
// first (it was not blocked) or no lock wait appears within 10s.
func waitForFenceLockWait(t *testing.T, s *Store, done chan error, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("%s finished without waiting on the fence: %v", what, err)
		default:
		}
		var waiting int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never waited on a lock", what)
}
