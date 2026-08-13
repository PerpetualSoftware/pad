package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2539. items.updated_at / items.deleted_at are RFC3339 whole-second
// strings; the /changes cursor is a unix-millisecond value that gets formatted
// with the same second precision, i.e. truncated DOWN. Under the original
// strict `>` that made every change landing in the cursor's own second compare
// equal and vanish — permanently, since the caller advances its cursor past
// that second afterwards.
//
// Both legs use a cursor that is strictly EARLIER than the mutation in real
// time, so a correct implementation returns the change in BOTH. They differ
// only in whether the cursor truncates to the same second as the write, which
// is exactly the axis the bug lives on: the second leg is the control, and it
// passed before the fix as well as after.
//
// The same-second claim is checked against the timestamps the writes ACTUALLY
// stored, not against a wall-clock reading taken before them — a leg whose
// writes drifted into the next second would otherwise pass under the unfixed
// query and be counted as evidence (Codex P2). Alignment is retried rather than
// skipped so the leg cannot silently stop testing anything.
func TestItemsModifiedSince_SameSecondCursor(t *testing.T) {
	for _, tc := range []struct {
		name string
		// how far before the mutation the cursor sits
		cursorLead time.Duration
		// whether that should land the cursor inside the mutation's own second
		sameSecond bool
	}{
		{name: "cursor inside the mutation's own second", cursorLead: 200 * time.Millisecond, sameSecond: true},
		{name: "cursor in the previous second (control)", cursorLead: 1200 * time.Millisecond, sameSecond: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const attempts = 8
			for attempt := 1; ; attempt++ {
				aligned := runSameSecondCursorLeg(t, tc.cursorLead, tc.sameSecond, attempt == attempts)
				if aligned {
					return
				}
				if attempt == attempts {
					t.Fatalf("could not land the writes in the intended second after %d attempts", attempts)
				}
			}
		})
	}
}

// runSameSecondCursorLeg runs one attempt. It reports whether the writes landed
// in the intended second relative to the cursor; when they did, it asserts the
// delta. `strict` makes a misalignment fatal rather than a retry signal.
func runSameSecondCursorLeg(t *testing.T, cursorLead time.Duration, wantSameSecond, strict bool) bool {
	t.Helper()

	s := testStore(t)
	ws := createTestWorkspace(t, s, "ChangesCursor")
	coll := createTestCollection(t, s, ws.ID, "Tasks")

	// Park the mutations ~500ms into a wall-clock second so a 200ms lead stays
	// inside that second and a 1200ms lead cannot.
	for {
		if frac := time.Now().UnixMilli() % 1000; frac > 450 && frac < 550 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	archived := createTestItem(t, s, ws.ID, coll.ID, "archived in-window", "")
	updatedItem := createTestItem(t, s, ws.ID, coll.ID, "updated in-window", "")

	cursor := time.Now().Add(-cursorLead)

	if err := s.DeleteItem(archived.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	newTitle := "updated in-window (touched)"
	if _, err := s.UpdateItem(updatedItem.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("update: %v", err)
	}

	// The alignment that matters is between the cursor and what was STORED.
	storedDeletedAt := scalarString(t, s, `SELECT deleted_at FROM items WHERE id = `+s.dialect.Placeholder(1), archived.ID)
	storedUpdatedAt := scalarString(t, s, `SELECT updated_at FROM items WHERE id = `+s.dialect.Placeholder(1), updatedItem.ID)
	cursorSecond := cursor.UTC().Truncate(time.Second).Format(time.RFC3339)
	gotSameSecond := storedDeletedAt == cursorSecond && storedUpdatedAt == cursorSecond
	if gotSameSecond != wantSameSecond {
		if !strict {
			return false
		}
		t.Fatalf("writes landed at deleted_at=%s updated_at=%s against cursor second %s (same-second=%v, wanted %v)",
			storedDeletedAt, storedUpdatedAt, cursorSecond, gotSameSecond, wantSameSecond)
	}
	if cursor.After(time.Now()) {
		t.Fatalf("cursor %s is not earlier than the mutations", cursor)
	}

	updated, deletedIDs, err := s.ItemsModifiedSince(ws.ID, cursor)
	if err != nil {
		t.Fatalf("ItemsModifiedSince: %v", err)
	}

	if !containsID(deletedIDs, archived.ID) {
		t.Errorf("archive stored at %s is missing from the deleted list for cursor second %s (same-second=%v)",
			storedDeletedAt, cursorSecond, wantSameSecond)
	}

	// The archived row must ALSO come back in `updated` — that is the only
	// coverage of the `(deleted_at IS NULL OR deleted_at >= ?)` arm, which
	// could otherwise regress to `>` unnoticed (Codex P2). Archived views need
	// the full row, not just the id.
	var sawArchivedInUpdated, sawUpdated bool
	for _, it := range updated {
		switch it.ID {
		case archived.ID:
			sawArchivedInUpdated = true
		case updatedItem.ID:
			sawUpdated = true
		}
	}
	if !sawArchivedInUpdated {
		t.Errorf("archived row stored at %s is missing from the updated list for cursor second %s (same-second=%v)",
			storedDeletedAt, cursorSecond, wantSameSecond)
	}
	if !sawUpdated {
		t.Errorf("update stored at %s is missing from the updated list for cursor second %s (same-second=%v)",
			storedUpdatedAt, cursorSecond, wantSameSecond)
	}
	return true
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func scalarString(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	var v string
	if err := s.db.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("scalar query %q: %v", query, err)
	}
	return v
}
