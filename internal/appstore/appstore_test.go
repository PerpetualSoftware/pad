package appstore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		return storetest.NewPostgres(t)
	}
	return storetest.NewSQLite(t)
}

// fixture: a workspace, one companion collection and an install at epoch 1.
// No production writer of app_installs exists before SPEC-6 U8, so the row
// is inserted directly.
func fixture(t *testing.T) (*store.Store, store.FenceSpec) {
	t.Helper()
	s := testStore(t)
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Apps"})
	if err != nil {
		t.Fatal(err)
	}
	col, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tickets", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.DB().Exec(s.D().Rebind(`INSERT INTO app_installs (id, workspace_id, origin, state, auth_epoch, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, ?, ?)`),
		id, ws.ID, "https://app.example", ts, ts); err != nil {
		t.Fatal(err)
	}
	return s, store.FenceSpec{InstallID: id, WorkspaceID: ws.ID, Epoch: 1, Companions: []string{col.ID}}
}

// A fence that admits writes nothing by itself, and a refused one writes
// nothing at all: the census baseline every U2 mutation is measured against.
func TestCheckFence_WritesNothing(t *testing.T) {
	s, spec := fixture(t)
	a := New(s, Options{})
	ctx := context.Background()

	writes := storetest.CaptureWrites(t, s, func() {
		if err := a.CheckFence(ctx, spec); err != nil {
			t.Fatalf("admitted fence: %v", err)
		}
	})
	if len(writes) != 0 {
		t.Fatalf("an admitted, empty fence wrote: %v", writes)
	}

	stale := spec
	stale.Epoch = 2
	writes = storetest.CaptureWrites(t, s, func() {
		if err := a.CheckFence(ctx, stale); !errors.Is(err, store.ErrFenceStale) {
			t.Fatalf("stale fence: %v", err)
		}
	})
	if len(writes) != 0 {
		t.Fatalf("a refused fence wrote: %v", writes)
	}
}
