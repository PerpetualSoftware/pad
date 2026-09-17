package store

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3101, store half. RestoreItem with WithRestorePlanLimit decides
// items_per_workspace in its own transaction, under the workspace seq lock.
// Same construction as limits_workspace_mint_race_test.go: a competitor holds
// the seq lock and has inserted a live item without committing; the restore
// must wait for it, then refuse. The control holds the lock and inserts
// nothing; the restore waits, then succeeds.

// restoreFixture is newLimitedWorkspaceFixture's workspace with one archived
// item and the cap set to one more than the live count, so exactly one
// restore fits.
func restoreFixture(t *testing.T, s *Store) (f limitedWorkspaceFixture, archived string) {
	t.Helper()
	f = newLimitedWorkspaceFixture(t, s, limitedWorkspaceInserts[0]) // the CreateItem case: feature items_per_workspace
	it, err := s.CreateItem(f.ws, f.collID, models.ItemCreate{Title: "to restore"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if err := s.DeleteItem(it.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	return f, it.ID
}

func runRestoreRace(t *testing.T, s *Store, competitorCounts bool) {
	t.Helper()
	f, archived := restoreFixture(t, s)

	tx, err := s.db.Begin() // on SQLite this is BEGIN IMMEDIATE
	if err != nil {
		t.Fatalf("begin competitor: %v", err)
	}
	defer tx.Rollback()
	if s.dialect.Driver() == DriverPostgres {
		lockWorkspaceSeq(t, s, tx, f.ws)
	}
	if competitorCounts {
		limitedWorkspaceInserts[0].competitor(t, s, tx, f.ws, f.collID, "")
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.RestoreItem(archived, WithRestorePlanLimit())
		done <- err
	}()

	if s.dialect.Driver() == DriverPostgres {
		waitForLockWait(t, s, "pg_advisory_xact_lock(hashtext($1))", done)
	} else {
		select {
		case err := <-done:
			t.Fatalf("the restore finished (err = %v) while the competitor held the write lock", err)
		case <-time.After(300 * time.Millisecond):
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit competitor: %v", err)
	}
	restoreErr := <-done

	got, err := s.featureCountOn(s.db, f.ws, "", "items_per_workspace")
	if err != nil {
		t.Fatalf("count after: %v", err)
	}
	if competitorCounts {
		var ple *PlanLimitError
		if !errors.As(restoreErr, &ple) {
			t.Fatalf("restore err = %v, want *PlanLimitError (count %d, cap %d)", restoreErr, got, f.limit)
		}
		if ple.Result.Limit != f.limit || ple.Result.Current != f.limit {
			t.Errorf("PlanLimitError result = %+v, want %d of %d", ple.Result, f.limit, f.limit)
		}
		if got != f.limit {
			t.Errorf("live items = %d after a refused restore, want the cap %d", got, f.limit)
		}
		if it, _ := s.GetItemIncludeDeleted(archived); it == nil || it.DeletedAt == nil {
			t.Errorf("the refused item is no longer archived: %+v", it)
		}
		return
	}
	if restoreErr != nil {
		t.Fatalf("control: restore err = %v, want success", restoreErr)
	}
	if got != f.limit {
		t.Errorf("control: live items = %d, want exactly the cap %d", got, f.limit)
	}
}

func TestRestoreItemLimit_WaitsForCompetitor_ThenRefuses(t *testing.T) {
	t.Parallel()
	runRestoreRace(t, testStore(t), true)
}

func TestRestoreItemLimit_WaitsForNonCountingHolder_ThenAdmits(t *testing.T) {
	t.Parallel()
	runRestoreRace(t, testStore(t), false)
}

// At the cap: without the option the restore is unlimited; with it, a live
// item is still not-found (never a plan refusal) and an archived one refuses.
func TestRestoreItemLimit_AtCap_OptionAndLiveItem(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	f, archived := restoreFixture(t, s)
	owner, err := s.workspaceOwnerForTest(f.ws)
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	live, err := s.CreateItem(f.ws, f.collID, models.ItemCreate{Title: "live"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	current, err := s.featureCountOn(s.db, f.ws, "", "items_per_workspace")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if err := s.SetUserPlanOverrides(owner, fmt.Sprintf(`{"items_per_workspace":%d}`, current)); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}

	if _, err := s.RestoreItem(live.ID, WithRestorePlanLimit()); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("restore of a live item at the cap: err = %v, want sql.ErrNoRows", err)
	}
	var ple *PlanLimitError
	if _, err := s.RestoreItem(archived, WithRestorePlanLimit()); !errors.As(err, &ple) {
		t.Errorf("restore at the cap with the option: err = %v, want *PlanLimitError", err)
	}
	if _, err := s.RestoreItem(archived); err != nil {
		t.Errorf("restore at the cap without the option: %v, want success", err)
	}
	if got, _ := s.featureCountOn(s.db, f.ws, "", "items_per_workspace"); got != current+1 {
		t.Errorf("live items = %d, want %d after the unlimited restore", got, current+1)
	}
}
