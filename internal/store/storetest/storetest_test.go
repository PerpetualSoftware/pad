package storetest

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func TestMain(m *testing.M) {
	// See internal/server/main_test.go for the full BUG-1371 rationale —
	// this package's own probe test creates a user, and there's no
	// reason to pay the production bcrypt cost for it.
	store.SetBcryptCostForTesting(bcrypt.MinCost)
	code := m.Run()
	Cleanup()
	os.Exit(code)
}

// TestNewSQLite_TemplateBuildsOnce asserts the migration chain (store.New
// on the template path) runs at most once per test binary regardless of
// how many times NewSQLite is called — the whole point of BUG-1913's
// fix. buildCount is monotonic and capped at 1 by templateOnce, so
// checking it's exactly 1 after at least one NewSQLite call is valid
// irrespective of what ran before this test in the binary.
func TestNewSQLite_TemplateBuildsOnce(t *testing.T) {
	_ = NewSQLite(t)
	_ = NewSQLite(t)
	_ = NewSQLite(t)

	if got := atomic.LoadInt32(&buildCount); got != 1 {
		t.Fatalf("buildTemplate ran %d times total, want exactly 1 (sync.Once)", got)
	}
}

// TestNewSQLite_NoWALSidecars asserts the template directory contains
// only the template file itself — the checkpoint + journal_mode=DELETE
// sequence in buildTemplate must leave no -wal/-shm sidecars for
// NewSQLite's plain file copy to lose track of.
func TestNewSQLite_NoWALSidecars(t *testing.T) {
	_ = NewSQLite(t)

	if templatePath == "" {
		t.Fatal("templatePath unset after NewSQLite")
	}
	entries, err := os.ReadDir(filepath.Dir(templatePath))
	if err != nil {
		t.Fatalf("read template dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(templatePath) {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("template dir contains %v, want exactly [%s]", names, filepath.Base(templatePath))
	}
}

// TestNewSQLite_CopiesAreIsolatedAndFunctional proves each NewSQLite call
// returns its own writable, working copy: writes to one copy (a real
// end-to-end op, not just a file-existence check) must not be visible
// from another.
func TestNewSQLite_CopiesAreIsolatedAndFunctional(t *testing.T) {
	sA := NewSQLite(t)

	u, err := sA.CreateUser(models.UserCreate{
		Email:    "storetest-probe@example.com",
		Username: "storetest-probe",
		Name:     "Storetest Probe",
		Password: "correcthorsebattery",
		Role:     "admin",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := sA.CreateWorkspace(models.WorkspaceCreate{Name: "Probe", Slug: "storetest-probe", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	got, err := sA.GetWorkspaceBySlug(ws.Slug)
	if err != nil || got == nil {
		t.Fatalf("workspace not readable back from its own copy: got=%v err=%v", got, err)
	}

	sB := NewSQLite(t)
	if leaked, err := sB.GetWorkspaceBySlug(ws.Slug); err != nil {
		t.Fatalf("query store B: %v", err)
	} else if leaked != nil {
		t.Fatal("workspace created in store A is visible in store B's copy; copies are not isolated")
	}
}
