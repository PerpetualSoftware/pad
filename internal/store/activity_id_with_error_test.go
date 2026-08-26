package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2779. Both activity writers used to return a real id alongside a
// non-nil error — CreateActivity returned the id of the row it had just
// FAILED to insert, and the debounced path returned the id of the row it had
// CHOSEN but not written. The item-update handler discarded that error and
// linked a user's comment to whatever came back, so the comment appeared
// under an activity entry describing a different change, on a 200.
//
// The contract is now: a non-nil error is always paired with an empty id.
// These assert it at the source, where it can be enforced, rather than only
// at the one caller that misused it.
//
// The failure is induced by closing the store's database. That is the
// cheapest way to make every write fail deterministically, and what is under
// test is the pairing of the two return values, not which error occurs.
func TestActivityWriters_ErrorNeverCarriesAnID(t *testing.T) {
	base := testStore(t)
	ws := createTestWorkspace(t, base, "IDContract")
	doc := createTestDoc(t, base, ws.ID, "Doc", "content")

	// A row for the debounced path to find and try to merge into, written
	// while the database still works.
	seed := models.Activity{
		WorkspaceID: ws.ID, DocumentID: doc.ID,
		Action: "updated", Actor: "agent", Source: "cli",
		Metadata: `{"agent":"rook","changes":"title: a → b"}`,
	}
	if _, err := base.CreateActivityDebounced(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := base.DB().Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	t.Run("CreateActivity", func(t *testing.T) {
		id, err := base.CreateActivity(models.Activity{
			WorkspaceID: ws.ID, DocumentID: doc.ID,
			Action: "updated", Actor: "agent", Source: "cli",
		})
		if err == nil {
			t.Fatal("expected an error from a closed database")
		}
		if id != "" {
			t.Errorf("id = %q alongside error %v — a caller that ignores the error gets a handle to a row that was never written", id, err)
		}
	})

	t.Run("CreateActivityDebounced", func(t *testing.T) {
		next := seed
		next.Metadata = `{"agent":"rook","changes":"status: open → done"}`
		id, err := base.CreateActivityDebounced(next)
		if err == nil {
			t.Fatal("expected an error from a closed database")
		}
		if id != "" {
			t.Errorf("id = %q alongside error %v — that id names the PREVIOUS activity row, and the item-update handler linked comments to it", id, err)
		}
	})
}

// TestActivityWriters_SuccessStillReturnsTheID is the control: a contract
// change that returned "" unconditionally would satisfy every assertion
// above, and break every caller.
func TestActivityWriters_SuccessStillReturnsTheID(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "IDContractOK")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	created, err := s.CreateActivity(models.Activity{
		WorkspaceID: ws.ID, DocumentID: doc.ID,
		Action: "created", Actor: "user", Source: "web",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created == "" {
		t.Error("CreateActivity returned an empty id on success")
	}

	first := models.Activity{
		WorkspaceID: ws.ID, DocumentID: doc.ID,
		Action: "updated", Actor: "agent", Source: "cli",
		Metadata: `{"agent":"rook","changes":"title: a → b"}`,
	}
	firstID, err := s.CreateActivityDebounced(first)
	if err != nil {
		t.Fatalf("debounced create: %v", err)
	}
	if firstID == "" {
		t.Fatal("CreateActivityDebounced returned an empty id on success")
	}

	// The merge path returns the id of the row it extended — the value the
	// comment link depends on.
	second := first
	second.Metadata = `{"agent":"rook","changes":"status: open → done"}`
	mergedID, err := s.CreateActivityDebounced(second)
	if err != nil {
		t.Fatalf("debounced merge: %v", err)
	}
	if mergedID != firstID {
		t.Errorf("merge returned %q, want the coalesced row %q", mergedID, firstID)
	}
}

// TestActivityDebounce_MergeFailureCarriesNoID reaches the line the test
// above cannot. Closing the database up front makes the CANDIDATE SELECT fail
// first, so the call falls back to CreateActivity and the merge's own error
// return is never executed — the assertion passed for a reason unrelated to
// the change under test, and a mutation restoring `return existingID, err`
// there SURVIVED because of it.
//
// The seam (BUG-2770's afterDebounceRead) fires between the candidate read
// and the merge write, which is exactly the window needed: the read succeeds
// and finds the row, then the write fails. That is the state in which the old
// code handed back a real id for a PREVIOUS activity.
func TestActivityDebounce_MergeFailureCarriesNoID(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "MergeFailure")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	seed := models.Activity{
		WorkspaceID: ws.ID, DocumentID: doc.ID,
		Action: "updated", Actor: "agent", Source: "cli",
		Metadata: `{"agent":"rook","changes":"title: a → b"}`,
	}
	seedID, err := s.CreateActivityDebounced(seed)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if seedID == "" {
		t.Fatal("seed returned no id")
	}

	fired := 0
	s.afterDebounceRead = func() {
		if fired > 0 {
			return
		}
		fired++
		// The candidate has been read; break the write.
		if cerr := s.DB().Close(); cerr != nil {
			t.Errorf("close db: %v", cerr)
		}
	}

	next := seed
	next.Metadata = `{"agent":"rook","changes":"status: open → done"}`
	id, err := s.CreateActivityDebounced(next)
	s.afterDebounceRead = nil

	if fired != 1 {
		t.Fatalf("seam fired %d times, want 1 — the candidate read did not happen, so the merge error path was not reached", fired)
	}
	if err == nil {
		t.Fatal("expected the merge write to fail against a closed database")
	}
	if id != "" {
		t.Errorf("id = %q alongside error %v — and %q is the SEED row, which this call did not write", id, err, seedID)
	}
	if id == seedID {
		t.Errorf("the returned id is the previous activity's own id (%q); that is the value the handler linked comments to", seedID)
	}
}

// TestActivityDebounce_ClassifierFailureCarriesNoID covers the last error
// return, and exists because review pointed out that "no instrument can reach
// it" described the seams I had written rather than the code. The failure has
// to land between the merge UPDATE affecting zero rows and the probe that
// decides which of its two arms refused; afterDebounceRefusal schedules
// exactly that.
//
// The zero-row refusal is produced honestly, by the mechanism that produces
// it in production: a comment linked to the candidate row freezes it
// (TASK-2760), so the merge's NOT EXISTS arm declines the write.
func TestActivityDebounce_ClassifierFailureCarriesNoID(t *testing.T) {
	s, item := agentNameFixture(t)

	writer := models.Activity{
		WorkspaceID: item.WorkspaceID,
		DocumentID:  item.ID,
		Action:      "updated",
		Actor:       "agent",
		Source:      "cli",
		Metadata:    `{"agent":"rook","changes":"title: a → b"}`,
	}
	seedID, err := s.CreateActivityDebounced(writer)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.CreateComment(item.WorkspaceID, item.ID, "", models.CommentCreate{
		Body: "freezes the row", CreatedBy: "agent", Source: "cli", ActivityID: seedID,
	}); err != nil {
		t.Fatalf("link comment: %v", err)
	}

	fired := 0
	s.afterDebounceRefusal = func() {
		if fired > 0 {
			return
		}
		fired++
		if cerr := s.DB().Close(); cerr != nil {
			t.Errorf("close db: %v", cerr)
		}
	}

	next := writer
	next.Metadata = `{"agent":"rook","changes":"status: open → done"}`
	id, err := s.CreateActivityDebounced(next)
	s.afterDebounceRefusal = nil

	if fired != 1 {
		t.Fatalf("seam fired %d times, want 1 — the merge did not refuse, so the classifier was never reached", fired)
	}
	if err == nil {
		t.Fatal("expected the classifier probe to fail against a closed database")
	}
	if id != "" {
		t.Errorf("id = %q alongside error %v — and %q is the frozen row this call did not write", id, err, seedID)
	}
}
